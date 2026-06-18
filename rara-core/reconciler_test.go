package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// fakeActivator records every provider the reconciler tried to activate. Under symmetric
// activation the reconciler calls Run for ALL assignments (on_demand and resident); the
// real Runner dispatches by provider shape (Cloud Run `run` vs tailnet poke).
type fakeActivator struct{ woken []string }

func (f *fakeActivator) Run(_ context.Context, req RunRequest) error {
	f.woken = append(f.woken, req.Provider.Name)
	return nil
}

// seedAndIngestOne seeds the lane and ingests a single video, returning the item id. It
// also marks the resident scribe (asr-youtube) "known alive" with a fresh heartbeat — the
// realistic state of a live scribe. (A never-seen resident would still route under the
// router's bootstrap grace; a fresh heartbeat is what keeps it eligible once seen and lets
// staleness, not absence, be the offline signal.)
func seedAndIngestOne(t *testing.T, db *MockDatabase, videoID string) int {
	t.Helper()
	ctx := context.Background()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// SeedYouTubeLane seeds the flow disabled (opt-in); enable for reconciler tests.
	enableYouTubeFlow(t, db)
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{videos: []YouTubeVideo{{VideoID: videoID}}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	markProviderAlive(t, db, provASRYouTube)
	return db.items[itemKey(laneYouTube, videoID)].ID
}

// enableYouTubeFlow marks the youtube flow enabled. SeedYouTubeLane seeds it disabled
// (opt-in lane); tests that exercise active ingestion or routing must call this.
func enableYouTubeFlow(t *testing.T, db *MockDatabase) {
	t.Helper()
	f, ok := db.flows[youtubeFlowName]
	if !ok {
		t.Fatalf("youtube flow not found — call SeedYouTubeLane first")
	}
	f.Enabled = true
	db.flows[youtubeFlowName] = f
}

// markProviderAlive stamps a fresh heartbeat on a provider so the router's health gate
// treats it as online (what a live resident worker does for itself).
func markProviderAlive(t *testing.T, db *MockDatabase, name string) {
	t.Helper()
	if err := db.TouchProviderHeartbeat(context.Background(), name); err != nil {
		t.Fatalf("heartbeat %s: %v", name, err)
	}
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

// runGate simulates the gate worker finishing a gate step: it records the cascade decision
// and marks the step done — exactly what the addon work handler does for a gate verdict. The
// reconciler then routes the item from that decision on its next pass.
func runGate(t *testing.T, db *MockDatabase, itemID, seq int, gate, decision string) {
	t.Helper()
	if err := db.InsertGateDecision(context.Background(), GateDecision{
		ItemID: itemID, Gate: gate, Decision: decision, DecidedBy: "profile", Reason: "test gate",
	}); err != nil {
		t.Fatalf("gate decision: %v", err)
	}
	completeStep(t, db, itemID, seq, "")
}

// TestReconcileFirstPass: one pass over a freshly discovered item auto-satisfies coletar
// and stops having ASSIGNED gate_barato to the gate worker — the metadata gate runs BEFORE
// transcription, so transcrever is not materialized yet, and the reconciler records no
// decision itself (the gate worker will).
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
	// gate_barato assigned to the gate worker, pending (a real work step now, on metadata).
	s, ok := stepBySeq(db, itemID, 2)
	if !ok || s.Status != stepPending || s.AssignedProvider != provGateBarato {
		t.Errorf("gate_barato step = %+v, want pending+gate-barato", s)
	}
	// transcrever NOT materialized — it waits behind the metadata gate.
	if _, ok := stepBySeq(db, itemID, 3); ok {
		t.Error("transcrever should not be materialized before gate_barato decides")
	}
	// on_demand gate -> activation fired exactly once.
	if len(act.woken) != 1 || act.woken[0] != provGateBarato {
		t.Errorf("expected gate-barato activation, got %v", act.woken)
	}
	// The reconciler records NO gate decision — the worker writes it.
	if len(db.gateDecisions) != 0 {
		t.Errorf("reconciler must not record gate decisions, got %+v", db.gateDecisions)
	}
	// Item still discovered.
	if got := db.itemByID[itemID].Status; got != itemDiscovered {
		t.Errorf("item status = %q, want discovered", got)
	}
}

// TestReconcileGateKeepAdvances: a kept gate_barato lets the item proceed — the next pass
// advances past the done gate and assigns transcrever.
func TestReconcileGateKeepAdvances(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep) // gate worker keeps it
	if err := r.ReconcileOnce(ctx); err != nil {        // route keep -> advance
		t.Fatal(err)
	}

	if s, ok := stepBySeq(db, itemID, 3); !ok || s.Status != stepPending || s.AssignedProvider != provASRYouTube {
		t.Errorf("transcrever step = %+v, want pending+asr-youtube after a kept gate", s)
	}
	if got := db.itemByID[itemID].Status; got != itemDiscovered {
		t.Errorf("item status = %q, want still discovered (transcription pending)", got)
	}
}

// TestReconcileActivatesResidentOnAssign: symmetric activation (P1b). When the reconciler assigns a
// step to a RESIDENT provider (the Mac scribe), it calls Activate for it too — no longer special-
// casing on_demand. The real Activator turns that into a tailnet poke; here the fake just records it.
func TestReconcileActivatesResidentOnAssign(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato (on_demand)
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // route keep -> assign transcrever (resident)
		t.Fatal(err)
	}

	if s, ok := stepBySeq(db, itemID, 3); !ok || s.AssignedProvider != provASRYouTube {
		t.Fatalf("transcrever step = %+v, want assigned to asr-youtube", s)
	}
	var residentWoken bool
	for _, n := range act.woken {
		if n == provASRYouTube {
			residentWoken = true
		}
	}
	if !residentWoken {
		t.Errorf("resident scribe must be activated on assign (symmetric activation), got %v", act.woken)
	}
}

// TestReconcileCoalescesActivationPerPass: when several items are assigned to the SAME provider in
// one pass, the reconciler wakes it ONCE — one wake drains the whole queue, so it must not fan out
// into N Cloud Run executions / N pokes. Two fresh items both assign gate-barato on the first pass.
func TestReconcileCoalescesActivationPerPass(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = seedAndIngestOne(t, db, "vid1")
	_ = seedAndIngestOne(t, db, "vid2")
	act := &fakeActivator{}
	r := NewReconciler(db, act)

	if err := r.ReconcileOnce(ctx); err != nil { // both items assign gate-barato
		t.Fatal(err)
	}

	var n int
	for _, name := range act.woken {
		if name == provGateBarato {
			n++
		}
	}
	if n != 1 {
		t.Errorf("gate-barato activated %d times in one pass, want 1 (coalesced), woken=%v", n, act.woken)
	}
}

// TestReconcileGateDropFilters: a dropped gate_barato terminates the item as `filtered` —
// transcrever is never materialized, and the item leaves the active set.
func TestReconcileGateDropFilters(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionDrop)
	if err := r.ReconcileOnce(ctx); err != nil { // route drop -> filtered
		t.Fatal(err)
	}

	if got := db.itemByID[itemID].Status; got != itemFiltered {
		t.Errorf("item status = %q, want filtered after a dropped gate", got)
	}
	if _, ok := stepBySeq(db, itemID, 3); ok {
		t.Error("a dropped item must never materialize transcrever")
	}
	active, _ := db.ListActiveItems(ctx)
	if len(active) != 0 {
		t.Errorf("filtered item should leave the active set, got %d active", len(active))
	}
}

// TestReconcileGateDeferQuarantines: a deferred gate_barato terminates the item as
// `quarantine` (the cold-start review sample), not filtered.
func TestReconcileGateDeferQuarantines(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionDefer)
	if err := r.ReconcileOnce(ctx); err != nil { // route defer -> quarantine
		t.Fatal(err)
	}

	if got := db.itemByID[itemID].Status; got != itemQuarantine {
		t.Errorf("item status = %q, want quarantine after a deferred gate", got)
	}
	quarantined, _ := db.ListQuarantinedItems(ctx)
	if len(quarantined) != 1 || quarantined[0].ID != itemID {
		t.Errorf("deferred item should appear in the quarantine sample, got %+v", quarantined)
	}
}

// TestReconcileGateRicoDropAfterTranscribe: the SECOND gate (gate_rico, on full text) also
// routes — a drop there terminates the item `filtered` before distillation.
func TestReconcileGateRicoDropAfterTranscribe(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 3, "transcript-7")
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_rico
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 4); !ok || s.Status != stepPending || s.AssignedProvider != provGateRico {
		t.Fatalf("gate_rico step = %+v, want pending+gate-rico", s)
	}
	runGate(t, db, itemID, 4, gateRico, decisionDrop)
	if err := r.ReconcileOnce(ctx); err != nil { // route drop -> filtered
		t.Fatal(err)
	}

	if got := db.itemByID[itemID].Status; got != itemFiltered {
		t.Errorf("item status = %q, want filtered after gate_rico drop", got)
	}
	if _, ok := stepBySeq(db, itemID, 5); ok {
		t.Error("a gate_rico drop must never materialize destilar")
	}
}

// TestReconcileIdempotentWhileInFlight: re-running while gate_barato is pending changes
// nothing (level-triggered, no duplicate assignment).
func TestReconcileIdempotentWhileInFlight(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	attemptAfterFirst := db.itemSteps[itemStepKey{itemID, 2}].Attempt

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := db.itemSteps[itemStepKey{itemID, 2}].Attempt; got != attemptAfterFirst {
		t.Errorf("second pass mutated the in-flight step attempt: %d -> %d", attemptAfterFirst, got)
	}
	if s := db.itemSteps[itemStepKey{itemID, 2}]; s.Status != stepPending {
		t.Errorf("gate_barato should still be pending, got %q", s.Status)
	}
}

// TestReconcileAfterTranscrever: with both gates kept, once the worker finishes transcrever
// the next pass assigns gate_rico; keeping it then assigns destilar (on_demand Cloud Run).
func TestReconcileAfterTranscrever(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 3, "transcript-7") // worker transcribed
	if err := r.ReconcileOnce(ctx); err != nil {   // assign gate_rico
		t.Fatal(err)
	}
	// Item at to_text (transcription done); gate_rico pending on full text.
	if got := db.itemByID[itemID].Status; got != itemToText {
		t.Errorf("item status = %q, want to_text", got)
	}
	if s, ok := stepBySeq(db, itemID, 4); !ok || s.Status != stepPending || s.AssignedProvider != provGateRico {
		t.Errorf("gate_rico step = %+v, want pending+gate-rico", s)
	}

	runGate(t, db, itemID, 4, gateRico, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // route keep -> assign destilar
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 5); !ok || s.Status != stepPending || s.AssignedProvider != provDistill {
		t.Errorf("destilar step = %+v, want pending+distill", s)
	}
	// on_demand distill woken (gate-barato + gate-rico were woken on their own passes too).
	if len(act.woken) == 0 || act.woken[len(act.woken)-1] != provDistill {
		t.Errorf("expected distill activation last, got %v", act.woken)
	}
}

// TestReconcileCompletes: once destilar finishes (both gates kept), the item becomes
// terminal (done) and leaves the active set.
func TestReconcileCompletes(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 3, "transcript-7")
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_rico
		t.Fatal(err)
	}
	runGate(t, db, itemID, 4, gateRico, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign destilar
		t.Fatal(err)
	}
	completeStep(t, db, itemID, 5, "distill-9")
	if err := r.ReconcileOnce(ctx); err != nil { // complete
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

// TestReconcileFailPropagates: a failed gate step (the worker erred, distinct from a drop
// decision) makes the item terminal (failed).
func TestReconcileFailPropagates(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	// Worker reports a hard failure on the gate step.
	s := db.itemSteps[itemStepKey{itemID, 2}]
	s.Status = stepFailed
	s.Error = "gate worker exploded"
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
// error (the item is not advanced); the reconciler retries next pass once one appears. The
// metadata gate is the first work step now, so disabling its provider is the trigger.
func TestReconcileNoProviderErrors(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	// Disable BOTH gate_barato providers (cloud + self-host) so the capability is unroutable.
	for _, name := range []string{provGateBarato, provGateBaratoLocal} {
		p := db.providers[name]
		p.Enabled = false
		db.providers[name] = p
	}

	r := NewReconciler(db, &fakeActivator{})
	it := db.itemByID[itemID]
	if err := r.reconcileItem(ctx, it); err == nil {
		t.Fatal("reconcile should error when no provider serves gate_barato")
	}
	// gate_barato must remain unmaterialized (nothing to undo).
	if _, ok := stepBySeq(db, itemID, 2); ok {
		t.Error("gate_barato step should not be created without a provider")
	}
}

// TestReconcileRequeuesStaleRunningStep (#3): a running step whose heartbeat has gone
// stale (worker likely died) is returned to the pending frontier for re-claim; a fresh
// heartbeat is left alone.
func TestReconcileRequeuesStaleRunningStep(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Helper: put the first assigned work step (gate_barato, seq 2) into `running` with the
	// given heartbeat age, run a reconcile pass, and return the resulting step.
	runWithHeartbeat := func(heartbeat time.Time) ItemStep {
		db := newMockDatabase()
		itemID := seedAndIngestOne(t, db, "vid1")
		r := NewReconciler(db, &fakeActivator{})
		if err := r.ReconcileOnce(ctx); err != nil { // assigns gate_barato (pending)
			t.Fatal(err)
		}
		s := db.itemSteps[itemStepKey{itemID, 2}]
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
		return db.itemSteps[itemStepKey{itemID, 2}]
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

// setProviderHeartbeat stamps an explicit heartbeat on a provider (deterministic health,
// no wall clock) for the reconciler's router gate.
func setProviderHeartbeat(db *MockDatabase, name string, ts time.Time) {
	p := db.providers[name]
	p.HeartbeatAt = &ts
	db.providers[name] = p
}

// TestReconcileTimeoutFallsBackToNextProvider (#2): when a running step's worker dies (stale
// heartbeat), the reconciler re-routes it to the NEXT eligible provider — excluding the dead
// one — honouring constraints + health, rather than re-queuing the same dead worker.
func TestReconcileTimeoutFallsBackToNextProvider(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")

	// A second, healthy transcrever provider (a backup Mac): also local + residential, but
	// pricier so the primary asr-youtube is chosen first.
	mustProvider(t, db, Provider{Name: "asr-backup", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, Constraints: residential, Cost: 2.0, Quality: 0.9, Enabled: true})
	setProviderHeartbeat(db, provASRYouTube, base.Add(-1*time.Minute)) // both alive at base
	setProviderHeartbeat(db, "asr-backup", base.Add(-1*time.Minute))

	r := NewReconciler(db, &fakeActivator{})
	r.now = func() time.Time { return base }
	r.staleAfter = 10 * time.Minute
	r.healthTTL = 5 * time.Minute

	// Get past the metadata gate, then the pass assigns transcrever to the cheaper primary.
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever
		t.Fatal(err)
	}
	if st := db.itemSteps[itemStepKey{itemID, 3}]; st.AssignedProvider != provASRYouTube {
		t.Fatalf("primary assignment = %q, want %s", st.AssignedProvider, provASRYouTube)
	}

	// The worker claims it, then dies: running with a stale heartbeat.
	st := db.itemSteps[itemStepKey{itemID, 3}]
	st.Status = stepRunning
	st.HeartbeatAt = ptime(base.Add(-30 * time.Minute))
	mustStep(t, db, st)

	// Next pass detects the dead worker and fails over to the backup.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := db.itemSteps[itemStepKey{itemID, 3}]
	if got.Status != stepPending || got.HeartbeatAt != nil {
		t.Errorf("re-routed step = %+v, want pending with heartbeat cleared", got)
	}
	if got.AssignedProvider != "asr-backup" {
		t.Errorf("failover assigned %q, want asr-backup (the next provider, not the dead one)", got.AssignedProvider)
	}
}

// countingActivator records per-provider Activate calls and can be scripted to fail the first
// `failFirst` calls (a cold-start timeout / a SA still missing run.invoker) before succeeding. It
// is the fake for the A2 self-healing tests: the call counter proves a re-activation fired (or did
// NOT, under the anti-stampede backoff) without any real Cloud Run `run`.
type countingActivator struct {
	calls     map[string]int
	total     int
	failFirst int
}

func (c *countingActivator) Run(_ context.Context, req RunRequest) error {
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.total++
	c.calls[req.Provider.Name]++
	if c.total <= c.failFirst {
		return fmt.Errorf("simulated activation failure for %s", req.Provider.Name)
	}
	return nil
}

// TestReactivateRecoversAfterFailedActivation (#4): an on_demand cloudrun assignment whose
// activation FAILS on the first pass (cold-start timeout) is left pending with no poll safety net.
// The next pass's self-healing re-fires the activation — no manual `gcloud run jobs execute`. Once
// that wake lands the worker can claim and finish the step; we simulate that to show it advances.
func TestReactivateRecoversAfterFailedActivation(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &countingActivator{failFirst: 1} // the first activation (gate_barato assign) fails
	r := NewReconciler(db, act)
	r.now = func() time.Time { return base }

	// Pass 1: assigns gate_barato (on_demand cloudrun) and tries to wake it — that wake fails.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 2); !ok || s.Status != stepPending || s.AssignedProvider != provGateBarato {
		t.Fatalf("gate_barato step = %+v, want pending+gate-barato", s)
	}
	if got := act.calls[provGateBarato]; got != 1 {
		t.Fatalf("after the failed first pass, gate-barato activations = %d, want 1", got)
	}

	// Pass 2: the step is still pending with no worker — self-healing re-fires the wake (success).
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := act.calls[provGateBarato]; got != 2 {
		t.Fatalf("self-healing did not re-activate: gate-barato activations = %d, want 2", got)
	}

	// With the worker now awake it claims and finishes the gate; the item advances without any
	// manual intervention.
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 3); !ok || s.AssignedProvider != provASRYouTube {
		t.Errorf("transcrever step = %+v, want assigned after the recovered gate", s)
	}
}

// TestReactivateRespectsBackoffAfterSuccess (#4, the anti-stampede proof): once an on_demand
// provider has been activated SUCCESSFULLY, the woken worker drains the queue, so while the
// pending step persists WITHIN reactivateBackoff the reconciler must NOT re-fire — re-firing would
// spawn concurrent executions (the swarm bug). Several passes inside the window keep the count at 1.
func TestReactivateRespectsBackoffAfterSuccess(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &countingActivator{} // every activation succeeds
	r := NewReconciler(db, act)
	r.reactivateBackoff = 3 * time.Minute
	now := base
	r.now = func() time.Time { return now }

	// Pass 1: assign gate_barato and wake it (success) — anchors the backoff.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := act.calls[provGateBarato]; got != 1 {
		t.Fatalf("first pass activations = %d, want 1", got)
	}

	// Several more passes, all still WITHIN the backoff, with the step deliberately left pending
	// (the worker is "draining"). None may re-fire.
	for _, dt := range []time.Duration{30 * time.Second, 90 * time.Second, 150 * time.Second} {
		now = base.Add(dt)
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := act.calls[provGateBarato]; got != 1 {
		t.Errorf("re-activated within backoff: gate-barato activations = %d, want 1 (anti-stampede)", got)
	}
	if s, ok := stepBySeq(db, itemID, 2); !ok || s.Status != stepPending {
		t.Errorf("gate_barato step = %+v, want still pending for this scenario", s)
	}

	// Step once PAST the backoff with the same still-pending step: now it MUST re-fire. This pins
	// the guard to the window itself — a reactivateStalled that simply never fired would also have
	// held the count at 1 above, so without this the test could not tell "blocked by backoff" from
	// "does nothing".
	now = base.Add(4 * time.Minute)
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := act.calls[provGateBarato]; got != 2 {
		t.Errorf("did not re-activate once past backoff: gate-barato activations = %d, want 2", got)
	}
}

// TestReactivateAfterBackoffElapses (#4): a step still pending BEYOND reactivateBackoff means the
// woken worker likely died without claiming — so the reconciler re-fires the activation. This is
// the recovery half of the backoff: quiet while draining, then a fresh wake once the window passes.
func TestReactivateAfterBackoffElapses(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	db := newMockDatabase()
	_ = seedAndIngestOne(t, db, "vid1")
	act := &countingActivator{}
	r := NewReconciler(db, act)
	r.reactivateBackoff = 3 * time.Minute
	now := base
	r.now = func() time.Time { return now }

	if err := r.ReconcileOnce(ctx); err != nil { // assign + wake gate_barato (anchors at base)
		t.Fatal(err)
	}
	if got := act.calls[provGateBarato]; got != 1 {
		t.Fatalf("first pass activations = %d, want 1", got)
	}

	now = base.Add(5 * time.Minute) // past the 3m backoff, step still pending
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := act.calls[provGateBarato]; got != 2 {
		t.Errorf("did not re-activate past backoff: gate-barato activations = %d, want 2", got)
	}
}

// TestReactivateSkipsResidentProvider (#4): the self-healing re-activation is ONLY for on_demand
// cloudrun providers with no poll safety net. A resident (the Mac scribe) already has poll + poke,
// so a persistently pending resident step — even well beyond the backoff — must NOT be re-fired
// (turning a resident into a swarm is exactly what we avoid). It keeps its single assignment-time
// poke and nothing more.
func TestReactivateSkipsResidentProvider(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	setProviderHeartbeat(db, provASRYouTube, base) // resident known-alive at base
	act := &countingActivator{}
	r := NewReconciler(db, act)
	r.reactivateBackoff = 3 * time.Minute
	r.healthTTL = time.Hour // keep the resident eligible regardless of clock drift below
	now := base
	r.now = func() time.Time { return now }

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato (on_demand)
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever -> resident asr-youtube
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 3); !ok || s.Status != stepPending || s.AssignedProvider != provASRYouTube {
		t.Fatalf("transcrever step = %+v, want pending+asr-youtube", s)
	}
	wokenAfterAssign := act.calls[provASRYouTube]
	if wokenAfterAssign != 1 {
		t.Fatalf("resident assignment-time pokes = %d, want 1", wokenAfterAssign)
	}

	// Well beyond the backoff, with transcrever still pending: the resident must NOT be re-fired
	// through the on_demand self-healing path.
	now = base.Add(30 * time.Minute)
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := act.calls[provASRYouTube]; got != 1 {
		t.Errorf("resident was re-activated by on_demand self-healing: pokes = %d, want 1", got)
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
