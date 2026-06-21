package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

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
	r := NewReconciler(db)

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// coletar auto-done, linked back to the source video.
	if s, ok := stepBySeq(db, itemID, 1); !ok || s.Status != stepDone || s.OutputRef != "vid1" {
		t.Errorf("coletar step = %+v, want done with output_ref=vid1", s)
	}
	// gate_barato assigned to the VPC-first gate worker (local before cloud), pending.
	s, ok := stepBySeq(db, itemID, 2)
	if !ok || s.Status != stepPending || s.AssignedProvider != provGateBaratoLocal {
		t.Errorf("gate_barato step = %+v, want pending+gate-barato-local (VPC-first)", s)
	}
	// transcrever NOT materialized — it waits behind the metadata gate.
	if _, ok := stepBySeq(db, itemID, 3); ok {
		t.Error("transcrever should not be materialized before gate_barato decides")
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
	r := NewReconciler(db)

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

// TestReconcileGateDropFilters: a dropped gate_barato terminates the item as `filtered` —
// transcrever is never materialized, and the item leaves the active set.
func TestReconcileGateDropFilters(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db)

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
	r := NewReconciler(db)

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
	r := NewReconciler(db)

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
	if s, ok := stepBySeq(db, itemID, 4); !ok || s.Status != stepPending || s.AssignedProvider != provGateRicoLocal {
		t.Fatalf("gate_rico step = %+v, want pending+gate-rico-local (VPC-first)", s)
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
	r := NewReconciler(db)

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
	r := NewReconciler(db)

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
	if s, ok := stepBySeq(db, itemID, 4); !ok || s.Status != stepPending || s.AssignedProvider != provGateRicoLocal {
		t.Errorf("gate_rico step = %+v, want pending+gate-rico-local (VPC-first)", s)
	}

	runGate(t, db, itemID, 4, gateRico, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // route keep -> assign destilar
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 5); !ok || s.Status != stepPending || s.AssignedProvider != provDistillLocal {
		t.Errorf("destilar step = %+v, want pending+distill-local (VPC-first)", s)
	}
}

// TestReconcileCompletes: once destilar finishes (both gates kept), the item becomes
// terminal (done) and leaves the active set.
func TestReconcileCompletes(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db)

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
	r := NewReconciler(db)

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

	r := NewReconciler(db)
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
		r := NewReconciler(db)
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

	// A second, healthy transcrever provider (a backup Mac): also local + residential.
	// The routing policy pins asr-youtube first so it's the primary; asr-backup is the fallback.
	mustProvider(t, db, Provider{Name: "asr-backup", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, Constraints: residential, Enabled: true})
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: capTranscrever,
		Fallback: json.RawMessage(`["` + provASRYouTube + `","asr-backup"]`)})
	setProviderHeartbeat(db, provASRYouTube, base.Add(-1*time.Minute)) // both alive at base
	setProviderHeartbeat(db, "asr-backup", base.Add(-1*time.Minute))

	r := NewReconciler(db)
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

// TestReconcileNeverCallsRunner: F3 contract. The reconciler only assigns (persists desired state);
// the rara-runner dispatch loop does the waking. The reconciler has no runner field — waking is
// structurally impossible at this layer.
func TestReconcileNeverCallsRunner(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db)

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever
		t.Fatal(err)
	}

	// Both assignments are recorded; no runner was called (structurally impossible).
	if s, ok := stepBySeq(db, itemID, 2); !ok || s.Status != stepDone {
		t.Errorf("gate_barato step = %+v, want done after runGate", s)
	}
	if s, ok := stepBySeq(db, itemID, 3); !ok || s.Status != stepPending || s.AssignedProvider != provASRYouTube {
		t.Errorf("transcrever step = %+v, want pending+asr-youtube", s)
	}
}

// TestReconcileUsesStepProviders: when a flow_step carries options.providers, the reconciler
// honours that per-step priority list over the global routing policy. A second scribe is
// registered and the flow step is patched to pin it first; after reconcile the transcrever
// step must be assigned to the override provider, not the default asr-youtube.
func TestReconcileUsesStepProviders(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")

	// Register an alternative local scribe. The global routing policy pins asr-youtube first
	// so the per-step override is required to select alt-scribe instead.
	altScribe := "alt-scribe"
	if err := db.UpsertProvider(ctx, Provider{
		Name: altScribe, Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	markProviderAlive(t, db, altScribe)
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: capTranscrever,
		Fallback: json.RawMessage(`["` + provASRYouTube + `","` + altScribe + `"]`)})

	// Patch the transcrever flow step (seq 3) to pin alt-scribe first.
	optBytes, err := json.Marshal(struct {
		Providers []string `json:"providers"`
	}{Providers: []string{altScribe}})
	if err != nil {
		t.Fatal(err)
	}
	fid := db.flows[youtubeFlowName].ID
	if err := db.UpsertFlowStep(ctx, FlowStep{
		FlowID: fid, Seq: 3, Capability: capTranscrever, Enabled: true,
		Options: json.RawMessage(optBytes),
	}); err != nil {
		t.Fatal(err)
	}

	r := NewReconciler(db)
	if err := r.ReconcileOnce(ctx); err != nil { // assigns gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assigns transcrever
		t.Fatal(err)
	}

	s, ok := stepBySeq(db, itemID, 3)
	if !ok {
		t.Fatal("transcrever step not materialized")
	}
	if s.AssignedProvider != altScribe {
		t.Errorf("transcrever assigned to %q, want %q (step-level override)", s.AssignedProvider, altScribe)
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
