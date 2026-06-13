package main

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner is a StepRunner that returns a canned output_ref (or error) and records the
// items it was asked to run — so the claim/advance orchestration is tested with zero I/O.
type fakeRunner struct {
	outputRef string
	err       error
	ran       []string // source_refs seen
}

func (f *fakeRunner) Run(_ context.Context, item Item, _ ItemStep) (string, error) {
	f.ran = append(f.ran, item.SourceRef)
	return f.outputRef, f.err
}

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
	runner := &fakeRunner{outputRef: "transcript-7"}
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
	runner := &fakeRunner{outputRef: "x"}
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
	scribe := NewWorker(db, capTranscrever, &fakeRunner{outputRef: "transcript-7"})
	distill := NewWorker(db, capDestilar, &fakeRunner{outputRef: "distill-9"})

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
