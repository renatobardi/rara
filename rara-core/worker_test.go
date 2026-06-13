package main

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner is a StepRunner that returns a canned result (or error) and records the items
// it was asked to run — so the claim/advance orchestration is tested with zero I/O.
type fakeRunner struct {
	result RunResult
	err    error
	ran    []string // source_refs seen
}

func (f *fakeRunner) Run(_ context.Context, item Item, _ ItemStep) (RunResult, error) {
	f.ran = append(f.ran, item.SourceRef)
	return f.result, f.err
}

// out is a small constructor for a successful run returning an output_ref.
func out(ref string) RunResult { return RunResult{OutputRef: ref} }

// assignTranscrever drives the reconciler far enough to leave a pending transcrever step.
// The metadata gate now precedes transcription, so we keep gate_barato (as its worker would)
// before the reconciler reaches transcrever.
func assignTranscrever(t *testing.T, db *MockDatabase) int {
	t.Helper()
	ctx := context.Background()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db, &fakeActivator{})
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatalf("reconcile: %v", err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign transcrever
		t.Fatalf("reconcile: %v", err)
	}
	return itemID
}

// TestWorkerClaimsAndCompletes: the shim claims its pending step, runs it, and writes the
// domain row id back as output_ref with status done.
func TestWorkerClaimsAndCompletes(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{result: out("transcript-7")}
	w := NewWorker(db, capTranscrever, provASRYouTube, runner)

	claimed, err := w.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !claimed {
		t.Fatal("expected to claim the pending transcrever step")
	}
	if len(runner.ran) != 1 || runner.ran[0] != "vid1" {
		t.Errorf("runner saw %v, want [vid1]", runner.ran)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepDone || s.OutputRef != "transcript-7" {
		t.Errorf("step = %+v, want done with output_ref=transcript-7", s)
	}
	if s.Attempt != 1 {
		t.Errorf("claim should bump attempt to 1, got %d", s.Attempt)
	}
	if s.HeartbeatAt == nil {
		t.Error("claim should stamp a heartbeat")
	}
}

// TestWorkerNoWork: an empty queue yields claimed=false, no error.
func TestWorkerNoWork(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	claimed, err := NewWorker(db, capTranscrever, provASRYouTube, &fakeRunner{}).RunOnce(ctx)
	if err != nil || claimed {
		t.Errorf("empty queue: claimed=%v err=%v, want false/nil", claimed, err)
	}
}

// TestWorkerRunFailureMarksFailed: a runner error marks the step failed with the message,
// so the reconciler can fail the item next pass.
func TestWorkerRunFailureMarksFailed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	w := NewWorker(db, capTranscrever, provASRYouTube, &fakeRunner{err: errors.New("asr exploded")})

	claimed, err := w.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunOnce: claimed=%v err=%v", claimed, err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepFailed || s.Error != "asr exploded" {
		t.Errorf("step = %+v, want failed with the error recorded", s)
	}
}

// TestClaimNoDoubleClaimFIFO: two pending steps of one capability are claimed in
// insertion order, each exactly once; a third claim returns nothing (SKIP LOCKED).
func TestClaimNoDoubleClaimFIFO(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	flowID := db.flows[youtubeFlowName].ID
	// Two items, each with a pending transcrever step (seq 3).
	id1, _ := db.UpsertItem(ctx, Item{Lane: laneYouTube, SourceRef: "a", FlowID: flowID, FlowVersion: 1, Status: itemDiscovered})
	id2, _ := db.UpsertItem(ctx, Item{Lane: laneYouTube, SourceRef: "b", FlowID: flowID, FlowVersion: 1, Status: itemDiscovered})
	mustStep(t, db, ItemStep{ItemID: id1, Seq: 3, Capability: capTranscrever, Status: stepPending, AssignedProvider: provASRYouTube})
	mustStep(t, db, ItemStep{ItemID: id2, Seq: 3, Capability: capTranscrever, Status: stepPending, AssignedProvider: provASRYouTube})

	first, err := db.ClaimPendingStep(ctx, capTranscrever, provASRYouTube)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v / %v", first, err)
	}
	second, err := db.ClaimPendingStep(ctx, capTranscrever, provASRYouTube)
	if err != nil || second == nil {
		t.Fatalf("second claim: %v / %v", second, err)
	}
	if first.ItemID == second.ItemID {
		t.Fatalf("double-claimed the same step (item %d)", first.ItemID)
	}
	// FIFO: item 1's step was inserted first.
	if first.ItemID != id1 || second.ItemID != id2 {
		t.Errorf("claim order = (%d,%d), want FIFO (%d,%d)", first.ItemID, second.ItemID, id1, id2)
	}
	// Both are now running, so a third claim finds nothing.
	if third, _ := db.ClaimPendingStep(ctx, capTranscrever, provASRYouTube); third != nil {
		t.Errorf("third claim should be empty, got item %d", third.ItemID)
	}
}

// TestClaimProviderIsolation: with two pending steps of one capability assigned to DIFFERENT
// providers, each worker claims only the step routed to its own provider — never the sibling's.
// This is what keeps two transcrever providers (asr-youtube on the Mac, asr-direct-audio on
// Cloud Run) from stealing each other's work.
func TestClaimProviderIsolation(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, Provider{Name: "prov-a", Capability: capTranscrever, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true})
	mustProvider(t, db, Provider{Name: "prov-b", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident, Enabled: true})
	fid := seedFlow(t, db)
	idA, _ := db.UpsertItem(ctx, Item{Lane: laneYouTube, SourceRef: "a", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	idB, _ := db.UpsertItem(ctx, Item{Lane: laneYouTube, SourceRef: "b", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	mustStep(t, db, ItemStep{ItemID: idA, Seq: 3, Capability: capTranscrever, Status: stepPending, AssignedProvider: "prov-a"})
	mustStep(t, db, ItemStep{ItemID: idB, Seq: 3, Capability: capTranscrever, Status: stepPending, AssignedProvider: "prov-b"})

	claimedA, err := db.ClaimPendingStep(ctx, capTranscrever, "prov-a")
	if err != nil || claimedA == nil {
		t.Fatalf("prov-a claim: %v / %v", claimedA, err)
	}
	if claimedA.ItemID != idA {
		t.Errorf("prov-a claimed item %d, want %d (its own step)", claimedA.ItemID, idA)
	}
	// prov-a's queue is now empty (it must NOT see prov-b's step).
	if again, _ := db.ClaimPendingStep(ctx, capTranscrever, "prov-a"); again != nil {
		t.Errorf("prov-a should have no more work, claimed item %d", again.ItemID)
	}
	// prov-b still has its own step waiting.
	claimedB, _ := db.ClaimPendingStep(ctx, capTranscrever, "prov-b")
	if claimedB == nil || claimedB.ItemID != idB {
		t.Errorf("prov-b should claim its own step %d, got %v", idB, claimedB)
	}
}

// TestRunUntilDrained processes every pending step of a capability in one go.
func TestRunUntilDrained(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	flowID := db.flows[youtubeFlowName].ID
	for _, ref := range []string{"a", "b", "c"} {
		id, _ := db.UpsertItem(ctx, Item{Lane: laneYouTube, SourceRef: ref, FlowID: flowID, FlowVersion: 1, Status: itemDiscovered})
		mustStep(t, db, ItemStep{ItemID: id, Seq: 3, Capability: capTranscrever, Status: stepPending, AssignedProvider: provASRYouTube})
	}
	runner := &fakeRunner{result: out("x")}
	if err := NewWorker(db, capTranscrever, provASRYouTube, runner).RunUntilDrained(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(runner.ran) != 3 {
		t.Errorf("drained %d steps, want 3", len(runner.ran))
	}
}

// gateKeepRunner is a fake gate worker that always keeps (a verdict, no output_ref).
func gateKeepRunner() *fakeRunner {
	return &fakeRunner{result: RunResult{Gate: &GateVerdict{Decision: decisionKeep, DecidedBy: decidedByProfile, Reason: "e2e keep"}}}
}

// TestEndToEndYouTubeFlow drives a single video from discovery to done through alternating
// reconcile passes and worker shims — the whole control loop end to end, now WITH the two
// curation gates as real workers (both keep). The only seams are the fake worker runners and
// a fake activator.
func TestEndToEndYouTubeFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)
	workers := []*Worker{
		NewWorker(db, capGateBarato, provGateBarato, gateKeepRunner()),
		NewWorker(db, capTranscrever, provASRYouTube, &fakeRunner{result: out("transcript-7")}),
		NewWorker(db, capGateRico, provGateRico, gateKeepRunner()),
		NewWorker(db, capDestilar, provDistill, &fakeRunner{result: out("distill-9")}),
	}

	// Run to a fixed point: reconcile, then let every shim drain, until the item is done.
	for pass := 0; pass < 10; pass++ {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d reconcile: %v", pass, err)
		}
		for _, w := range workers {
			if _, err := w.RunOnce(ctx); err != nil {
				t.Fatalf("pass %d worker %s: %v", pass, w.capability, err)
			}
		}
		if db.itemByID[itemID].Status == itemDone {
			break
		}
	}

	if got := db.itemByID[itemID].Status; got != itemDone {
		t.Fatalf("item never completed, final status %q", got)
	}
	// The work steps are done, with output_refs linking back to the worker domain rows.
	wantRefs := map[int]string{1: "vid1", 3: "transcript-7", 5: "distill-9"}
	for seq, want := range wantRefs {
		s := db.itemSteps[itemStepKey{itemID, seq}]
		if s.Status != stepDone || s.OutputRef != want {
			t.Errorf("seq %d = %+v, want done output_ref=%s", seq, s, want)
		}
	}
	// Both gates ran as workers and are done.
	for _, seq := range []int{2, 4} {
		if s := db.itemSteps[itemStepKey{itemID, seq}]; s.Status != stepDone {
			t.Errorf("gate step seq %d = %+v, want done", seq, s)
		}
	}
	// Both gates kept (the workers recorded the decisions).
	if len(db.gateDecisions) != 2 {
		t.Errorf("expected 2 keep decisions, got %d", len(db.gateDecisions))
	}
	// on_demand providers (gate-barato, gate-rico, distill) were woken; the resident scribe
	// was not.
	woken := map[string]bool{}
	for _, n := range act.woken {
		woken[n] = true
	}
	for _, n := range []string{provGateBarato, provGateRico, provDistill} {
		if !woken[n] {
			t.Errorf("expected %s to be woken, got %v", n, act.woken)
		}
	}
	if woken[provASRYouTube] {
		t.Errorf("resident scribe must not be woken via activation, got %v", act.woken)
	}
}

// mustStep is a test helper that upserts an item_step or fails.
func mustStep(t *testing.T, db *MockDatabase, s ItemStep) {
	t.Helper()
	if err := db.UpsertItemStep(context.Background(), s); err != nil {
		t.Fatalf("upsert step: %v", err)
	}
}

// TestWorkerFiltersEmptyTranscript (#2): a benign no-content result marks the step done
// with its output_ref AND curates the item out (terminal `filtered`, not failed), so it
// is never driven into a distill that must fail.
func TestWorkerFiltersEmptyTranscript(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{result: RunResult{OutputRef: "transcript-empty", Filtered: true}}

	claimed, err := NewWorker(db, capTranscrever, provASRYouTube, runner).RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunOnce: claimed=%v err=%v", claimed, err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepDone || s.OutputRef != "transcript-empty" {
		t.Errorf("step = %+v, want done with output_ref", s)
	}
	if got := db.itemByID[itemID].Status; got != itemFiltered {
		t.Errorf("item status = %q, want filtered", got)
	}
	active, _ := db.ListActiveItems(ctx)
	if len(active) != 0 {
		t.Errorf("filtered item should leave the active set, got %d active", len(active))
	}
}

// gateScore is a small *float64 helper for verdicts.
func gateScore(f float64) *float64 { return &f }

// TestGateWorkerRecordsDecision: a gate worker claims a pending gate step, and for each
// cascade verdict (keep/drop/defer) it appends a gate_decisions row with the decision +
// score + decided_by + reason and marks the step DONE — but does NOT touch item status
// (the reconciler routes from the decision). This is deliverable #1's "grava gate_decisions"
// and #7's keep/drop/defer + gate_decisions recording, with zero I/O.
func TestGateWorkerRecordsDecision(t *testing.T) {
	for _, tc := range []struct {
		decision  string
		decidedBy string
	}{
		{decisionKeep, decidedByProfile},
		{decisionDrop, decidedByRules},
		{decisionDefer, decidedByLLM},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			ctx := context.Background()
			db := newMockDatabase()
			itemID := seedAndIngestOne(t, db, "vid1")
			// A pending gate_barato step at the metadata gate's seq (2), as the reconciler
			// would assign it to a gate provider (provider set so the provider-aware claim matches).
			mustStep(t, db, ItemStep{ItemID: itemID, Seq: 2, Capability: capGateBarato, Status: stepPending, AssignedProvider: provGateBarato})

			verdict := &GateVerdict{
				Decision: tc.decision, Score: gateScore(0.71),
				DecidedBy: tc.decidedBy, Reason: "unit-test verdict",
			}
			runner := &fakeRunner{result: RunResult{Gate: verdict}}
			claimed, err := NewWorker(db, capGateBarato, provGateBarato, runner).RunOnce(ctx)
			if err != nil || !claimed {
				t.Fatalf("RunOnce: claimed=%v err=%v", claimed, err)
			}

			// gate_decisions row appended with the full verdict.
			if len(db.gateDecisions) != 1 {
				t.Fatalf("expected 1 gate_decision, got %d", len(db.gateDecisions))
			}
			d := db.gateDecisions[0]
			if d.ItemID != itemID || d.Gate != gateBarato || d.Decision != tc.decision ||
				d.DecidedBy != tc.decidedBy || d.Reason != "unit-test verdict" {
				t.Errorf("gate_decision = %+v, want item=%d gate_barato %s by %s", d, itemID, tc.decision, tc.decidedBy)
			}
			if d.Score == nil || *d.Score != 0.71 {
				t.Errorf("score not recorded: %v", d.Score)
			}
			// Step done; item status untouched (reconciler routes, not the worker).
			if s := db.itemSteps[itemStepKey{itemID, 2}]; s.Status != stepDone {
				t.Errorf("gate step = %+v, want done", s)
			}
			if got := db.itemByID[itemID].Status; got != itemDiscovered {
				t.Errorf("worker must not route the item: status = %q, want still discovered", got)
			}
		})
	}
}

// TestWorkerRetriesTransientThenFails (#4): a retryable miss (distill's batch hasn't
// produced the row yet) re-queues the step as pending instead of failing the item — until
// the attempt ceiling, after which it fails for good.
func TestWorkerRetriesTransientThenFails(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{err: errRetryable}

	// First claim+run: transient -> re-queued pending, not failed.
	if _, err := NewWorker(db, capTranscrever, provASRYouTube, runner).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepPending {
		t.Fatalf("after a transient miss the step should be re-queued pending, got %q", s.Status)
	}
	if s.HeartbeatAt != nil {
		t.Error("re-queued step should have its heartbeat cleared")
	}

	// Draining keeps retrying until the attempt ceiling, then fails for good.
	if err := NewWorker(db, capTranscrever, provASRYouTube, runner).RunUntilDrained(ctx); err != nil {
		t.Fatal(err)
	}
	s = db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepFailed {
		t.Errorf("after %d attempts the step should fail, got %q", maxStepAttempts, s.Status)
	}
	if s.Attempt != maxStepAttempts {
		t.Errorf("attempt = %d, want the ceiling %d", s.Attempt, maxStepAttempts)
	}
}

// itemGoneDB makes GetItem report not-found while delegating everything else to the mock,
// to exercise the orphan-step path without an (un-)deletable item in the mock.
type itemGoneDB struct{ *MockDatabase }

func (itemGoneDB) GetItem(context.Context, int) (Item, bool, error) { return Item{}, false, nil }

// TestWorkerItemNotFound (#5): if the item vanished between claim and read, the orphan step
// is marked failed so it leaves the running set.
func TestWorkerItemNotFound(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)

	claimed, err := NewWorker(itemGoneDB{db}, capTranscrever, provASRYouTube, &fakeRunner{result: out("x")}).RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunOnce: claimed=%v err=%v", claimed, err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepFailed || s.Error != "item not found" {
		t.Errorf("orphan step = %+v, want failed 'item not found'", s)
	}
}

// TestRunUntilDrainedStopsOnContextCancel (#1): a cancelled context stops the drain loop
// promptly — the basis for graceful SIGTERM shutdown of the resident/on_demand workers.
func TestRunUntilDrainedStopsOnContextCancel(t *testing.T) {
	db := newMockDatabase()
	assignTranscrever(t, db) // a pending step is waiting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewWorker(db, capTranscrever, provASRYouTube, &fakeRunner{result: out("x")}).RunUntilDrained(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("drain should stop with context.Canceled, got %v", err)
	}
}
