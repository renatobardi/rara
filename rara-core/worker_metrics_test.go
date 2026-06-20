package main

// worker_metrics_test.go — tests for GET /v1/workers/metrics (slice 2/9 of the Workers screen).
// See CONSOLE-WORKERS.pt-BR.md §8.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Mock-level tests: WorkerMetrics rollup semantics
// ---------------------------------------------------------------------------

// seedMetricsDB creates a minimal capability+providers+item then seeds item_steps
// with a mix of statuses, providers and timestamps so every rollup field can be
// verified in a single fixture.
//
//	distill-a: 2 done (attempt 1+2), 1 failed (attempt 1), 1 running (attempt 1)
//	distill-b: 1 pending (attempt 0)
//	unassigned: 1 pending — must be ignored by WorkerMetrics
func seedMetricsDB(t *testing.T) (*MockDatabase, time.Time, time.Time) {
	t.Helper()
	ctx := context.Background()
	db := newMockDatabase()

	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar}); err != nil {
		t.Fatal(err)
	}
	mustProvider(t, db, Provider{Name: "distill-a", Capability: capDestilar,
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	mustProvider(t, db, Provider{Name: "distill-b", Capability: capDestilar,
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{
		Lane: laneYouTube, SourceRef: "v1", FlowID: fid, FlowVersion: 1, Status: itemDiscovered,
	})

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// distill-a steps
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 1, Capability: capDestilar,
		Status: stepDone, AssignedProvider: "distill-a", Attempt: 1, UpdatedAt: &old})
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 2, Capability: capDestilar,
		Status: stepDone, AssignedProvider: "distill-a", Attempt: 2, UpdatedAt: &recent})
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 3, Capability: capDestilar,
		Status: stepFailed, AssignedProvider: "distill-a", Attempt: 1, UpdatedAt: &old})
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 4, Capability: capDestilar,
		Status: stepRunning, AssignedProvider: "distill-a", Attempt: 1, UpdatedAt: &old})

	// distill-b: one pending step
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 5, Capability: capDestilar,
		Status: stepPending, AssignedProvider: "distill-b", Attempt: 0, UpdatedAt: &old})

	// unassigned step — must be excluded
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 6, Capability: capDestilar,
		Status: stepPending, Attempt: 0, UpdatedAt: &old})

	return db, old, recent
}

// TestWorkerMetricsUnassignedIgnored: steps with no assigned_provider must not appear.
func TestWorkerMetricsUnassignedIgnored(t *testing.T) {
	db, _, _ := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metrics {
		if m.Provider == "" {
			t.Error("WorkerMetrics returned a row with empty provider (unassigned step leaked)")
		}
	}
	// Only distill-a and distill-b should appear.
	if len(metrics) != 2 {
		t.Errorf("want 2 providers, got %d", len(metrics))
	}
}

// TestWorkerMetricsOrderedByProvider: results must be sorted by provider name for determinism.
func TestWorkerMetricsOrderedByProvider(t *testing.T) {
	db, _, _ := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(metrics))
	for i, m := range metrics {
		names[i] = m.Provider
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("WorkerMetrics not sorted by provider: %v", names)
	}
}

// TestWorkerMetricsTotalsAndByStatus: Total, ByStatus, Done, Failed for distill-a.
func TestWorkerMetricsTotalsAndByStatus(t *testing.T) {
	db, _, _ := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var a WorkerMetric
	for _, m := range metrics {
		if m.Provider == "distill-a" {
			a = m
			break
		}
	}
	if a.Provider == "" {
		t.Fatal("distill-a not found in WorkerMetrics")
	}
	// 2 done + 1 failed + 1 running = 4 total
	if a.Total != 4 {
		t.Errorf("distill-a Total=%d, want 4", a.Total)
	}
	if a.ByStatus[stepDone] != 2 {
		t.Errorf("distill-a done=%d, want 2", a.ByStatus[stepDone])
	}
	if a.ByStatus[stepFailed] != 1 {
		t.Errorf("distill-a failed=%d, want 1", a.ByStatus[stepFailed])
	}
	if a.ByStatus[stepRunning] != 1 {
		t.Errorf("distill-a running=%d, want 1", a.ByStatus[stepRunning])
	}
	if a.Done != 2 {
		t.Errorf("distill-a Done=%d, want 2", a.Done)
	}
	if a.Failed != 1 {
		t.Errorf("distill-a Failed=%d, want 1", a.Failed)
	}
}

// TestWorkerMetricsSuccessRate: done/(done+failed); 0 when neither exists.
func TestWorkerMetricsSuccessRate(t *testing.T) {
	db, _, _ := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range metrics {
		switch m.Provider {
		case "distill-a":
			// done=2, failed=1 → 2/3 ≈ 0.6667
			want := 2.0 / 3.0
			if diff := m.SuccessRate - want; diff < -0.001 || diff > 0.001 {
				t.Errorf("distill-a SuccessRate=%.4f, want %.4f", m.SuccessRate, want)
			}
		case "distill-b":
			// pending only → done=0, failed=0 → SuccessRate must be 0
			if m.SuccessRate != 0 {
				t.Errorf("distill-b SuccessRate=%.4f, want 0 (no done/failed)", m.SuccessRate)
			}
		}
	}
}

// TestWorkerMetricsQueue: pending+assigned+running count.
func TestWorkerMetricsQueue(t *testing.T) {
	db, _, _ := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range metrics {
		switch m.Provider {
		case "distill-a":
			// 0 pending + 0 assigned + 1 running = 1
			if m.Queue != 1 {
				t.Errorf("distill-a Queue=%d, want 1", m.Queue)
			}
		case "distill-b":
			// 1 pending
			if m.Queue != 1 {
				t.Errorf("distill-b Queue=%d, want 1", m.Queue)
			}
		}
	}
}

// TestWorkerMetricsAvgAttempt: average attempt across all steps for the provider.
func TestWorkerMetricsAvgAttempt(t *testing.T) {
	db, _, _ := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range metrics {
		if m.Provider == "distill-a" {
			// attempts: 1, 2, 1, 1 → avg = 5/4 = 1.25
			want := 5.0 / 4.0
			if diff := m.AvgAttempt - want; diff < -0.001 || diff > 0.001 {
				t.Errorf("distill-a AvgAttempt=%.4f, want %.4f", m.AvgAttempt, want)
			}
		}
	}
}

// TestWorkerMetricsLastActivityAt: must be the maximum updated_at across all provider's steps.
func TestWorkerMetricsLastActivityAt(t *testing.T) {
	db, _, recent := seedMetricsDB(t)
	metrics, err := db.WorkerMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range metrics {
		if m.Provider == "distill-a" {
			if m.LastActivityAt == nil {
				t.Fatal("distill-a LastActivityAt is nil")
			}
			// max(old, recent) must equal recent
			if !m.LastActivityAt.Equal(recent) {
				t.Errorf("distill-a LastActivityAt=%v, want %v", m.LastActivityAt, recent)
			}
		}
	}
}

// TestWorkerMetricsSinceFilter: steps outside the window must be excluded.
func TestWorkerMetricsSinceFilter(t *testing.T) {
	db, _, recent := seedMetricsDB(t)

	// since = just before the recent timestamp → only the recent step passes.
	cutoff := recent.Add(-time.Second)
	metrics, err := db.WorkerMetrics(context.Background(), &cutoff)
	if err != nil {
		t.Fatal(err)
	}

	// distill-a has only 1 step with updated_at=recent; the other 3 are "old".
	var a WorkerMetric
	for _, m := range metrics {
		if m.Provider == "distill-a" {
			a = m
			break
		}
	}
	if a.Provider == "" {
		// distill-a had 1 recent step so it must appear
		t.Fatal("distill-a should appear after since filter (has 1 recent step)")
	}
	if a.Total != 1 {
		t.Errorf("after since filter: distill-a Total=%d, want 1", a.Total)
	}
	if a.ByStatus[stepDone] != 1 {
		t.Errorf("after since filter: distill-a done=%d, want 1", a.ByStatus[stepDone])
	}

	// distill-b has no recent steps — must not appear.
	for _, m := range metrics {
		if m.Provider == "distill-b" {
			t.Error("distill-b should not appear after since filter (all steps are old)")
		}
	}
}

// TestWorkerMetricsSinceNilUpdatedAt: when since is set, steps with nil UpdatedAt
// must be excluded (treated as "before any window").
func TestWorkerMetricsSinceNilUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar}); err != nil {
		t.Fatal(err)
	}
	mustProvider(t, db, Provider{Name: "distill-nil", Capability: capDestilar,
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{
		Lane: laneYouTube, SourceRef: "v1", FlowID: fid, FlowVersion: 1, Status: itemDiscovered,
	})
	// Step with nil UpdatedAt — must be excluded by any since filter.
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 1, Capability: capDestilar,
		Status: stepDone, AssignedProvider: "distill-nil", Attempt: 1, UpdatedAt: nil})

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	metrics, err := db.WorkerMetrics(ctx, &cutoff)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metrics {
		if m.Provider == "distill-nil" {
			t.Error("step with nil UpdatedAt should be excluded when since filter is active")
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP surface tests
// ---------------------------------------------------------------------------

// seedWorkerMetricsHTTP creates the minimal surface mux + one capability + one provider +
// one item used by the HTTP handler tests. Returns (mux, db, itemID).
func seedWorkerMetricsHTTP(t *testing.T) (http.Handler, *MockDatabase, int) {
	t.Helper()
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar}); err != nil {
		t.Fatal(err)
	}
	mustProvider(t, db, Provider{Name: "distill-a", Capability: capDestilar,
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{
		Lane: laneYouTube, SourceRef: "v1", FlowID: fid, FlowVersion: 1, Status: itemDiscovered,
	})
	return NewSurfaceMux(core, testToken), db, itemID
}

func TestHTTPWorkerMetrics200(t *testing.T) {
	h, db, itemID := seedWorkerMetricsHTTP(t)
	now := time.Now()
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 1, Capability: capDestilar,
		Status: stepDone, AssignedProvider: "distill-a", Attempt: 1, UpdatedAt: &now})

	rec := do(t, h, http.MethodGet, "/v1/workers/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var metrics []WorkerMetric
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode []WorkerMetric: %v", err)
	}
	if len(metrics) != 1 {
		t.Errorf("want 1 provider, got %d", len(metrics))
	}
	if metrics[0].Provider != "distill-a" {
		t.Errorf("provider=%q, want distill-a", metrics[0].Provider)
	}
	if metrics[0].Done != 1 {
		t.Errorf("done=%d, want 1", metrics[0].Done)
	}
}

// TestHTTPWorkerMetricsInvalidDays: non-integer or out-of-range days returns 400.
func TestHTTPWorkerMetricsInvalidDays(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	cases := []string{"abc", "0", "-1", "366", "999999"}
	for _, raw := range cases {
		rec := do(t, h, http.MethodGet, "/v1/workers/metrics?days="+raw, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("days=%q should be 400, got %d: %s", raw, rec.Code, rec.Body.String())
		}
	}
}

// TestHTTPWorkerMetricsDaysFilter: ?days=7 restricts to recent steps only.
func TestHTTPWorkerMetricsDaysFilter(t *testing.T) {
	h, db, itemID := seedWorkerMetricsHTTP(t)
	// old step (30 days ago — outside ?days=7 window)
	old := time.Now().Add(-30 * 24 * time.Hour)
	mustStep(t, db, ItemStep{ItemID: itemID, Seq: 1, Capability: capDestilar,
		Status: stepDone, AssignedProvider: "distill-a", Attempt: 1, UpdatedAt: &old})

	rec := do(t, h, http.MethodGet, "/v1/workers/metrics?days=7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 with days=7, got %d: %s", rec.Code, rec.Body.String())
	}
	var metrics []WorkerMetric
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode []WorkerMetric: %v", err)
	}
	// the 30-days-old step must be excluded → no providers
	if len(metrics) != 0 {
		t.Errorf("days=7 should exclude the old step, got %d metrics", len(metrics))
	}
}

// TestCoreWorkerMetricsWrapsDBError: Core.WorkerMetrics must wrap db errors so the caller
// can identify the layer where the failure occurred.
func TestCoreWorkerMetricsWrapsDBError(t *testing.T) {
	core, _, _ := newTestCore(t)

	// A cancelled context causes MockDatabase.WorkerMetrics to return context.Canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := core.WorkerMetrics(ctx, nil)

	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "core worker metrics") {
		t.Errorf("error not wrapped with context; err = %v", err)
	}
}
