package main

import (
	"context"
	"testing"
)

// These tests cover what rara-core itself still owns of the contract now that every domain worker is
// its own app on the rara-addon SDK (rara-scribe, rara-distill, rara-sift, rara-glean): the
// contract-table CLAIM (the atomic pull + the per-provider isolation) and the reconciler control
// loop end to end. rara-core no longer runs a `work` role — the SDK's loop mechanics (poke, poll,
// heartbeat, requeue) are unit-tested in the rara-addon module, and each worker's domain logic in
// its own app — so the worker side here is SIMULATED the way an external app behaves: it completes
// its claimed step (completeStep) / records its gate decision (runGate), and the reconciler routes.

// TestClaimNoDoubleClaimFIFO: two pending steps of one capability are claimed in insertion order,
// each exactly once; a third claim returns nothing (SKIP LOCKED). Exercises the store claim directly.
func TestClaimNoDoubleClaimFIFO(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	flowID := db.flows[youtubeFlowName].ID
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
	if first.ItemID != id1 || second.ItemID != id2 {
		t.Errorf("claim order = (%d,%d), want FIFO (%d,%d)", first.ItemID, second.ItemID, id1, id2)
	}
	if third, _ := db.ClaimPendingStep(ctx, capTranscrever, provASRYouTube); third != nil {
		t.Errorf("third claim should be empty, got item %d", third.ItemID)
	}
}

// TestClaimProviderIsolation: with two pending steps of one capability assigned to DIFFERENT
// providers, each worker claims only the step routed to its own provider — never the sibling's.
// This is what keeps two transcrever providers (asr-youtube on the Mac, asr-direct-audio on Cloud
// Run) from stealing each other's work, and a private item from being pulled by a third party.
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
	if again, _ := db.ClaimPendingStep(ctx, capTranscrever, "prov-a"); again != nil {
		t.Errorf("prov-a should have no more work, claimed item %d", again.ItemID)
	}
	claimedB, _ := db.ClaimPendingStep(ctx, capTranscrever, "prov-b")
	if claimedB == nil || claimedB.ItemID != idB {
		t.Errorf("prov-b should claim its own step %d, got %v", idB, claimedB)
	}
}

// mustStep is a test helper that upserts an item_step or fails. (Shared by the reconciler/router/
// surface/mcp tests as well as this file.)
func mustStep(t *testing.T, db *MockDatabase, s ItemStep) {
	t.Helper()
	if err := db.UpsertItemStep(context.Background(), s); err != nil {
		t.Fatalf("upsert step: %v", err)
	}
}

// TestEndToEndYouTubeFlow drives a single video from discovery to done through alternating reconcile
// passes and SIMULATED worker completions — the whole control loop end to end. Every domain worker
// now runs out of process on the SDK (rara-scribe, rara-distill, rara-sift); here the to-text and
// distill workers are simulated by completeStep (the external app writes its domain row and marks the
// step done) and the curation gates by runGate (rara-sift writes its gate_decision and marks the step
// done). The only seams are the simulators; the reconciler under test is real.
func TestEndToEndYouTubeFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")

	r := NewReconciler(db)
	// The work steps (transcrever seq 3, destilar seq 5) are completed by their out-of-process apps;
	// we simulate each finishing its claimed step with its domain row id.
	work := []struct {
		seq int
		ref string
	}{{3, "transcript-7"}, {5, "distill-9"}}
	// The gates (gate_barato seq 2, gate_rico seq 4) run out of process (rara-sift); runGate records
	// the decision + marks the step done after each pass.
	gates := []struct {
		seq        int
		capability string
	}{{2, capGateBarato}, {4, capGateRico}}

	// Run to a fixed point: reconcile, let the external gate workers decide, then let every work step
	// be completed by its app, until the item is done.
	for pass := 0; pass < 10; pass++ {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d reconcile: %v", pass, err)
		}
		for _, g := range gates {
			if s := db.itemSteps[itemStepKey{itemID, g.seq}]; s.Status == stepPending {
				runGate(t, db, itemID, g.seq, g.capability, decisionKeep) // rara-sift keeps
			}
		}
		for _, w := range work {
			if s := db.itemSteps[itemStepKey{itemID, w.seq}]; s.Status == stepPending {
				completeStep(t, db, itemID, w.seq, w.ref) // rara-scribe / rara-distill finishes
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
	// Both gates kept (the handlers recorded the decisions).
	if len(db.gateDecisions) != 2 {
		t.Errorf("expected 2 keep decisions, got %d", len(db.gateDecisions))
	}
}
