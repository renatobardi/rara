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
func assignTranscrever(t *testing.T, db *MockDatabase) int {
	t.Helper()
	itemID := seedAndIngestOne(t, db, "vid1")
	if err := NewReconciler(db, &fakeActivator{}).ReconcileOnce(context.Background()); err != nil {
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
	w := NewWorker(db, capTranscrever, runner)

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
	claimed, err := NewWorker(db, capTranscrever, &fakeRunner{}).RunOnce(ctx)
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
	w := NewWorker(db, capTranscrever, &fakeRunner{err: errors.New("asr exploded")})

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

	first, err := db.ClaimPendingStep(ctx, capTranscrever)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v / %v", first, err)
	}
	second, err := db.ClaimPendingStep(ctx, capTranscrever)
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
	if third, _ := db.ClaimPendingStep(ctx, capTranscrever); third != nil {
		t.Errorf("third claim should be empty, got item %d", third.ItemID)
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
	if err := NewWorker(db, capTranscrever, runner).RunUntilDrained(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(runner.ran) != 3 {
		t.Errorf("drained %d steps, want 3", len(runner.ran))
	}
}

// TestEndToEndYouTubeFlow drives a single video from discovery to done through alternating
// reconcile passes and worker shims — the whole Phase 1 control loop, end to end, with the
// only seams being the fake worker runners and a fake activator.
func TestEndToEndYouTubeFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)
	scribe := NewWorker(db, capTranscrever, &fakeRunner{result: out("transcript-7")})
	distill := NewWorker(db, capDestilar, &fakeRunner{result: out("distill-9")})

	// Run to a fixed point: reconcile, then let both shims drain, until the item is done.
	for pass := 0; pass < 6; pass++ {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d reconcile: %v", pass, err)
		}
		if _, err := scribe.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d scribe: %v", pass, err)
		}
		if _, err := distill.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d distill: %v", pass, err)
		}
		if db.itemByID[itemID].Status == itemDone {
			break
		}
	}

	if got := db.itemByID[itemID].Status; got != itemDone {
		t.Fatalf("item never completed, final status %q", got)
	}
	// Every step is done, with output_refs linking back to the worker domain rows.
	wantRefs := map[int]string{1: "vid1", 3: "transcript-7", 5: "distill-9"}
	for seq, want := range wantRefs {
		s := db.itemSteps[itemStepKey{itemID, seq}]
		if s.Status != stepDone || s.OutputRef != want {
			t.Errorf("seq %d = %+v, want done output_ref=%s", seq, s, want)
		}
	}
	// Both pass-through gates kept.
	if len(db.gateDecisions) != 2 {
		t.Errorf("expected 2 keep decisions, got %d", len(db.gateDecisions))
	}
	// distill (on_demand) was woken; scribe (resident) was not.
	if len(act.woken) != 1 || act.woken[0] != provDistill {
		t.Errorf("activation = %v, want [distill]", act.woken)
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

	claimed, err := NewWorker(db, capTranscrever, runner).RunOnce(ctx)
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

// TestWorkerRetriesTransientThenFails (#4): a retryable miss (distill's batch hasn't
// produced the row yet) re-queues the step as pending instead of failing the item — until
// the attempt ceiling, after which it fails for good.
func TestWorkerRetriesTransientThenFails(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{err: errRetryable}

	// First claim+run: transient -> re-queued pending, not failed.
	if _, err := NewWorker(db, capTranscrever, runner).RunOnce(ctx); err != nil {
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
	if err := NewWorker(db, capTranscrever, runner).RunUntilDrained(ctx); err != nil {
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

	claimed, err := NewWorker(itemGoneDB{db}, capTranscrever, &fakeRunner{result: out("x")}).RunOnce(ctx)
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

	err := NewWorker(db, capTranscrever, &fakeRunner{result: out("x")}).RunUntilDrained(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("drain should stop with context.Canceled, got %v", err)
	}
}
