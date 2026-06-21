package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func mustCapability(t *testing.T, db *MockDatabase, name string) {
	t.Helper()
	if err := db.UpsertCapability(context.Background(), Capability{Name: name}); err != nil {
		t.Fatalf("upsert capability %q: %v", name, err)
	}
}

// TestCoreWorkersGroupsByWorkerField: two providers sharing worker="distill" → one Worker with
// two placements; one provider with worker="asr-youtube" → one Worker with one placement.
func TestCoreWorkersGroupsByWorkerField(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	mustCapability(t, db, capDestilar)
	mustCapability(t, db, capTranscrever)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	mustProvider(t, db, Provider{Name: "distill-local", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeLocal, Activation: activationResident, Enabled: true})
	mustProvider(t, db, Provider{Name: "asr-youtube", Capability: capTranscrever, Worker: "asr-youtube",
		Runtime: runtimeLocal, Activation: activationResident, Enabled: true})

	workers, err := core.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers, got %d: %+v", len(workers), workers)
	}

	// workers ordered by name: "asr-youtube" < "distill"
	if workers[0].Name != "asr-youtube" {
		t.Errorf("workers[0].Name = %q, want asr-youtube", workers[0].Name)
	}
	if workers[1].Name != "distill" {
		t.Errorf("workers[1].Name = %q, want distill", workers[1].Name)
	}
	if len(workers[1].Placements) != 2 {
		t.Fatalf("distill worker: want 2 placements, got %d", len(workers[1].Placements))
	}
	// placements ordered by name: "distill" < "distill-local"
	if workers[1].Placements[0].Name != "distill" {
		t.Errorf("placements[0].Name = %q, want distill", workers[1].Placements[0].Name)
	}
	if workers[1].Placements[1].Name != "distill-local" {
		t.Errorf("placements[1].Name = %q, want distill-local", workers[1].Placements[1].Name)
	}
	if workers[1].Capability != capDestilar {
		t.Errorf("distill worker capability = %q, want %s", workers[1].Capability, capDestilar)
	}
}

// TestCoreWorkersEmptyWorkerFallback: provider with empty Worker field falls back to Name.
func TestCoreWorkersEmptyWorkerFallback(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	mustCapability(t, db, capDestilar)
	// Worker field intentionally empty
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar,
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	workers, err := core.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("want 1 worker, got %d", len(workers))
	}
	if workers[0].Name != "distill" {
		t.Errorf("fallback worker name = %q, want distill", workers[0].Name)
	}
	if len(workers[0].Placements) != 1 {
		t.Fatalf("want 1 placement, got %d", len(workers[0].Placements))
	}
}

// TestCoreWorkersOrdering: workers and placements are both sorted by name deterministically.
func TestCoreWorkersOrdering(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	mustCapability(t, db, capDestilar)
	// Insert in reverse order to verify sort
	mustProvider(t, db, Provider{Name: "z-provider", Capability: capDestilar, Worker: "z-worker",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	mustProvider(t, db, Provider{Name: "a-provider", Capability: capDestilar, Worker: "a-worker",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	workers, err := core.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers, got %d", len(workers))
	}
	if workers[0].Name != "a-worker" || workers[1].Name != "z-worker" {
		t.Errorf("workers order = [%q, %q], want [a-worker, z-worker]",
			workers[0].Name, workers[1].Name)
	}
}

// TestHTTPListWorkers200: GET /v1/workers returns 200 with correct shape.
func TestHTTPListWorkers200(t *testing.T) {
	core, db, _ := newTestCore(t)
	mustCapability(t, db, capDestilar)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	mustProvider(t, db, Provider{Name: "distill-local", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeLocal, Activation: activationResident, Enabled: true})

	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/workers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var workers []Worker
	if err := json.Unmarshal(rec.Body.Bytes(), &workers); err != nil {
		t.Fatalf("decode []Worker: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("want 1 worker, got %d", len(workers))
	}
	if workers[0].Name != "distill" {
		t.Errorf("worker name = %q, want distill", workers[0].Name)
	}
	if workers[0].Capability != capDestilar {
		t.Errorf("worker capability = %q, want %s", workers[0].Capability, capDestilar)
	}
	if len(workers[0].Placements) != 2 {
		t.Errorf("want 2 placements, got %d", len(workers[0].Placements))
	}
}

// TestUpsertProviderRejectsInconsistentWorkerCapability: adding a placement with a different
// capability than existing siblings under the same worker is a bad-input error.
func TestUpsertProviderRejectsInconsistentWorkerCapability(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	mustCapability(t, db, capDestilar)
	mustCapability(t, db, capTranscrever)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	err := core.UpsertProvider(ctx, Provider{
		Name: "distill-local", Capability: capTranscrever, Worker: "distill",
		Runtime: runtimeLocal, Activation: activationResident, Enabled: true,
	})
	if err == nil {
		t.Fatal("expected error for inconsistent worker capability, got nil")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

// TestProviderLastErrorRoundTrip: a provider whose last_error was set (by the runner, P0d) is
// returned by GetProvider and ListProviders with the value intact — the column survives the read path.
func TestProviderLastErrorRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	mustCapability(t, db, capDestilar)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	// Simulate the runner (P0d) writing last_error directly into the store.
	errMsg := "context deadline exceeded"
	p := db.providers["distill"]
	p.LastError = &errMsg
	db.providers["distill"] = p

	got, found, err := db.GetProvider(ctx, "distill")
	if err != nil || !found {
		t.Fatalf("GetProvider: found=%v err=%v", found, err)
	}
	if got.LastError == nil || *got.LastError != errMsg {
		t.Errorf("GetProvider last_error = %v, want %q", got.LastError, errMsg)
	}

	all, err := db.ListProviders(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListProviders: len=%d err=%v", len(all), err)
	}
	if all[0].LastError == nil || *all[0].LastError != errMsg {
		t.Errorf("ListProviders last_error = %v, want %q", all[0].LastError, errMsg)
	}
}

// TestUpsertProviderPreservesLastError: editing a placement's config via UpsertProvider must not
// clear a last_error written by the runner (P0d). The column is runner-owned, not config-owned.
func TestUpsertProviderPreservesLastError(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	mustCapability(t, db, capDestilar)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	// Runner (P0d) sets last_error.
	errMsg := "connection refused"
	p := db.providers["distill"]
	p.LastError = &errMsg
	db.providers["distill"] = p

	// Operator edits the placement config (disable it).
	if err := db.UpsertProvider(ctx, Provider{
		Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: false,
	}); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}

	got, found, err := db.GetProvider(ctx, "distill")
	if err != nil || !found {
		t.Fatalf("GetProvider after upsert: found=%v err=%v", found, err)
	}
	if got.LastError == nil || *got.LastError != errMsg {
		t.Errorf("UpsertProvider clobbered last_error: got %v, want %q", got.LastError, errMsg)
	}
	if got.Enabled {
		t.Error("UpsertProvider did not apply Enabled=false")
	}
}

// TestProviderLastErrorTruncatedAtRead: last_error values longer than maxProviderErrorLen are
// capped when read via the mock (mirrors the scanProvider guard that bounds API response size).
func TestProviderLastErrorTruncatedAtRead(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	mustCapability(t, db, capDestilar)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	// Store a value longer than the cap directly (simulates a verbose runner message).
	long := string(make([]byte, maxProviderErrorLen+100))
	p := db.providers["distill"]
	p.LastError = &long
	db.providers["distill"] = p

	got, _, _ := db.GetProvider(ctx, "distill")
	if got.LastError == nil {
		t.Fatal("last_error unexpectedly nil")
	}
	if len(*got.LastError) > maxProviderErrorLen {
		t.Errorf("last_error len = %d, want ≤ %d", len(*got.LastError), maxProviderErrorLen)
	}
}

// TestHTTPListWorkersDoesNotBreakProviders: GET /v1/providers still works after adding /v1/workers.
func TestHTTPListWorkersDoesNotBreakProviders(t *testing.T) {
	core, db, _ := newTestCore(t)
	mustCapability(t, db, capDestilar)
	mustProvider(t, db, Provider{Name: "distill", Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})

	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/providers: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var providers []Provider
	if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
		t.Fatalf("decode []Provider: %v", err)
	}
	if len(providers) != 1 {
		t.Errorf("want 1 provider, got %d", len(providers))
	}
}
