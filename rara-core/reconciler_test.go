package main

import (
	"context"
	"testing"
	"time"
)

// fakeActivator records which on_demand providers the reconciler tried to wake.
type fakeActivator struct{ woken []string }

func (f *fakeActivator) Activate(_ context.Context, p Provider) error {
	f.woken = append(f.woken, p.Name)
	return nil
}

// seedAndIngestOne seeds the lane and ingests a single video, returning the item id.
func seedAndIngestOne(t *testing.T, db *MockDatabase, videoID string) int {
	t.Helper()
	ctx := context.Background()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{videos: []YouTubeVideo{{VideoID: videoID}}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return db.items[itemKey(laneYouTube, videoID)].ID
}

// stepBySeq is a test helper to fetch one item_step.
func stepBySeq(db *MockDatabase, itemID, seq int) (ItemStep, bool) {
	s, ok := db.itemSteps[itemStepKey{itemID, seq}]
	return s, ok
}

// completeStep simulates a worker finishing a step (what the shim writes back).
func completeStep(t *testing.T, db *MockDatabase, itemID, seq int, outputRef string) {
	t.Helper()
	s := db.itemSteps[itemStepKey{itemID, seq}]
	s.Status = stepDone
	s.OutputRef = outputRef
	if err := db.UpsertItemStep(context.Background(), s); err != nil {
		t.Fatalf("complete step: %v", err)
	}
}

// TestReconcileFirstPass: one pass over a freshly discovered item must blow through the
// auto-satisfiable steps (coletar, gate_barato) and stop having ASSIGNED transcrever.
func TestReconcileFirstPass(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// coletar auto-done, linked back to the source video.
	if s, ok := stepBySeq(db, itemID, 1); !ok || s.Status != stepDone || s.OutputRef != "vid1" {
		t.Errorf("coletar step = %+v, want done with output_ref=vid1", s)
	}
	// gate_barato auto-keep + done.
	if s, ok := stepBySeq(db, itemID, 2); !ok || s.Status != stepDone {
		t.Errorf("gate_barato step = %+v, want done", s)
	}
	// transcrever assigned to the resident scribe, still pending (awaiting the worker pull).
	s, ok := stepBySeq(db, itemID, 3)
	if !ok || s.Status != stepPending || s.AssignedProvider != provASRYouTube {
		t.Errorf("transcrever step = %+v, want pending+asr-youtube", s)
	}
	// destilar not materialized yet.
	if _, ok := stepBySeq(db, itemID, 5); ok {
		t.Error("destilar should not be materialized before transcrever completes")
	}
	// resident provider -> NOT woken via activation.
	if len(act.woken) != 0 {
		t.Errorf("resident transcrever should not fire activation, woke %v", act.woken)
	}
	// Item still discovered (transcription not done).
	if got := db.itemByID[itemID].Status; got != itemDiscovered {
		t.Errorf("item status = %q, want discovered", got)
	}
	// Pass-through gate recorded a keep decision.
	if len(db.gateDecisions) != 1 || db.gateDecisions[0].Decision != decisionKeep ||
		db.gateDecisions[0].DecidedBy != gateDecidedByPassthrough {
		t.Errorf("expected 1 pass-through keep decision, got %+v", db.gateDecisions)
	}
}

// TestReconcileIdempotentWhileInFlight: re-running while transcrever is pending changes
// nothing (level-triggered, no duplicate assignment, no new gate decisions).
func TestReconcileIdempotentWhileInFlight(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	gatesAfterFirst := len(db.gateDecisions)
	attemptAfterFirst := db.itemSteps[itemStepKey{itemID, 3}].Attempt

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(db.gateDecisions); got != gatesAfterFirst {
		t.Errorf("second pass added gate decisions: %d -> %d", gatesAfterFirst, got)
	}
	if got := db.itemSteps[itemStepKey{itemID, 3}].Attempt; got != attemptAfterFirst {
		t.Errorf("second pass mutated the in-flight step attempt: %d -> %d", attemptAfterFirst, got)
	}
	if s := db.itemSteps[itemStepKey{itemID, 3}]; s.Status != stepPending {
		t.Errorf("transcrever should still be pending, got %q", s.Status)
	}
}

// TestReconcileAfterTranscrever: once the worker finishes transcrever, the next pass
// auto-keeps gate_rico and ASSIGNS destilar — and fires activation (on_demand Cloud Run).
func TestReconcileAfterTranscrever(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 3, "transcript-7") // worker transcribed

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// gate_rico auto-kept.
	if s, ok := stepBySeq(db, itemID, 4); !ok || s.Status != stepDone {
		t.Errorf("gate_rico step = %+v, want done", s)
	}
	// destilar assigned to distill, pending.
	if s, ok := stepBySeq(db, itemID, 5); !ok || s.Status != stepPending || s.AssignedProvider != provDistill {
		t.Errorf("destilar step = %+v, want pending+distill", s)
	}
	// on_demand distill -> activation fired exactly once.
	if len(act.woken) != 1 || act.woken[0] != provDistill {
		t.Errorf("expected distill activation, got %v", act.woken)
	}
	// Item advanced to to_text (transcription done, distillation pending).
	if got := db.itemByID[itemID].Status; got != itemToText {
		t.Errorf("item status = %q, want to_text", got)
	}
	// Two pass-through keeps now (gate_barato + gate_rico).
	if len(db.gateDecisions) != 2 {
		t.Errorf("expected 2 pass-through keeps, got %d", len(db.gateDecisions))
	}
}

// TestReconcileCompletes: once destilar finishes, the item becomes terminal (done) and
// leaves the active set.
func TestReconcileCompletes(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // -> assign transcrever
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 3, "transcript-7")
	if err := r.ReconcileOnce(ctx); err != nil { // -> assign destilar
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 5, "distill-9")
	if err := r.ReconcileOnce(ctx); err != nil { // -> complete
		t.Fatal(err)
	}

	if got := db.itemByID[itemID].Status; got != itemDone {
		t.Errorf("item status = %q, want done", got)
	}
	active, _ := db.ListActiveItems(ctx)
	if len(active) != 0 {
		t.Errorf("completed item should leave the active set, got %d active", len(active))
	}
}

// TestReconcileFailPropagates: a failed step makes the item terminal (failed).
func TestReconcileFailPropagates(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Worker reports a hard failure on transcrever.
	s := db.itemSteps[itemStepKey{itemID, 3}]
	s.Status = stepFailed
	s.Error = "asr exploded"
	if err := db.UpsertItemStep(ctx, s); err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := db.itemByID[itemID].Status; got != itemFailed {
		t.Errorf("item status = %q, want failed", got)
	}
}

// TestReconcileNoProviderErrors: a missing provider for a work step is surfaced as an
// error (the item is not advanced); the reconciler retries next pass once one appears.
func TestReconcileNoProviderErrors(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	// Disable the only transcrever provider.
	p := db.providers[provASRYouTube]
	p.Enabled = false
	db.providers[provASRYouTube] = p

	r := NewReconciler(db, &fakeActivator{})
	it := db.itemByID[itemID]
	if err := r.reconcileItem(ctx, it); err == nil {
		t.Fatal("reconcile should error when no provider serves transcrever")
	}
	// transcrever must remain unmaterialized (nothing to undo).
	if _, ok := stepBySeq(db, itemID, 3); ok {
		t.Error("transcrever step should not be created without a provider")
	}
}

// TestReconcileRequeuesStaleRunningStep (#3): a running step whose heartbeat has gone
// stale (worker likely died) is returned to the pending frontier for re-claim; a fresh
// heartbeat is left alone.
func TestReconcileRequeuesStaleRunningStep(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Helper: put the transcrever step into `running` with the given heartbeat age, run a
	// reconcile pass, and return the resulting step.
	runWithHeartbeat := func(heartbeat time.Time) ItemStep {
		db := newMockDatabase()
		itemID := seedAndIngestOne(t, db, "vid1")
		r := NewReconciler(db, &fakeActivator{})
		if err := r.ReconcileOnce(ctx); err != nil { // assigns transcrever (pending)
			t.Fatal(err)
		}
		s := db.itemSteps[itemStepKey{itemID, 3}]
		s.Status = stepRunning
		s.HeartbeatAt = &heartbeat
		if err := db.UpsertItemStep(ctx, s); err != nil {
			t.Fatal(err)
		}
		r.now = func() time.Time { return base }
		r.staleAfter = 10 * time.Minute
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return db.itemSteps[itemStepKey{itemID, 3}]
	}

	// Stale (heartbeat 30m old) -> re-queued pending, heartbeat cleared.
	stale := runWithHeartbeat(base.Add(-30 * time.Minute))
	if stale.Status != stepPending || stale.HeartbeatAt != nil {
		t.Errorf("stale running step = %+v, want pending with heartbeat cleared", stale)
	}

	// Fresh (heartbeat 1m old) -> left running, untouched.
	fresh := runWithHeartbeat(base.Add(-1 * time.Minute))
	if fresh.Status != stepRunning || fresh.HeartbeatAt == nil {
		t.Errorf("fresh running step = %+v, want still running", fresh)
	}
}

// TestComputeItemStatus checks the step-completion -> item-status mapping in isolation.
func TestComputeItemStatus(t *testing.T) {
	flowSteps := []FlowStep{
		{Seq: 1, Capability: capColetar},
		{Seq: 2, Capability: capGateBarato},
		{Seq: 3, Capability: capTranscrever},
		{Seq: 4, Capability: capGateRico},
		{Seq: 5, Capability: capDestilar},
	}
	done := func(seqs ...int) map[int]ItemStep {
		m := map[int]ItemStep{}
		for _, s := range seqs {
			m[s] = ItemStep{Seq: s, Status: stepDone}
		}
		return m
	}
	cases := []struct {
		name string
		by   map[int]ItemStep
		want string
	}{
		{"nothing done", done(), itemDiscovered},
		{"through gate_barato", done(1, 2), itemDiscovered},
		{"transcrever done", done(1, 2, 3), itemToText},
		{"through gate_rico", done(1, 2, 3, 4), itemToText},
		{"destilar done but not all", done(1, 2, 3, 4, 5), itemDone}, // all -> done wins
	}
	for _, c := range cases {
		if got := computeItemStatus(flowSteps, c.by); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
