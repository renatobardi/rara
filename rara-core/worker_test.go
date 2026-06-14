package main

import (
	"context"
	"errors"
	"testing"

	addon "rara-addon"
)

// These tests cover rara-core's `work` role now that the claim/advance orchestration lives in the
// rara-addon SDK. They drive the SDK end to end through rara-core's adapters (coreStore + the work
// handler in addon_work.go), so they prove the wiring: the claim isolation, the done/failed/filtered
// outcomes, the gate-decision recording, and the errRetryable -> requeue mapping. The SDK's own loop
// mechanics (poke, poll, periodic heartbeat, the granular per-attempt requeue) are unit-tested in
// the rara-addon module against a fake store.
//
// drainWork runs a (capability, provider) worker once over the queue (the on_demand drain-and-exit
// pattern: PollInterval 0, no poke listener) through the real adapters, and returns the SDK error.
func drainWork(ctx context.Context, db Database, capability, provider string, runner StepRunner) error {
	return addon.Run(ctx, addon.Config{
		Capability:  capability,
		Provider:    provider,
		Store:       newCoreStore(db),
		MaxAttempts: maxStepAttempts,
	}, workHandler(db, capability, runner))
}

// fakeRunner is a StepRunner that returns a canned result (or error) and records the items it was
// asked to run — so the adapter wiring is tested with zero I/O.
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

// assignTranscrever drives the reconciler far enough to leave a pending transcrever step. The
// metadata gate now precedes transcription, so we keep gate_barato (as its worker would) before the
// reconciler reaches transcrever.
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

// TestWorkerClaimsAndCompletes: the worker claims its pending step, runs it, and writes the domain
// row id back as output_ref with status done (heartbeat preserved from the claim).
func TestWorkerClaimsAndCompletes(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{result: out("transcript-7")}

	if err := drainWork(ctx, db, capTranscrever, provASRYouTube, runner); err != nil {
		t.Fatalf("drainWork: %v", err)
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
		t.Error("claim should stamp a heartbeat (preserved on done)")
	}
}

// TestWorkerNoWork: an empty queue drains cleanly with no error and the runner is never called.
func TestWorkerNoWork(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if err := drainWork(ctx, db, capTranscrever, provASRYouTube, runner); err != nil {
		t.Errorf("empty queue should drain cleanly, got %v", err)
	}
	if len(runner.ran) != 0 {
		t.Errorf("runner should not run on an empty queue, ran %v", runner.ran)
	}
}

// TestWorkerRunFailureMarksFailed: a runner error marks the step failed with the message, so the
// reconciler can fail the item next pass.
func TestWorkerRunFailureMarksFailed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)

	if err := drainWork(ctx, db, capTranscrever, provASRYouTube, &fakeRunner{err: errors.New("asr exploded")}); err != nil {
		t.Fatalf("drainWork: %v", err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepFailed || s.Error != "asr exploded" {
		t.Errorf("step = %+v, want failed with the error recorded", s)
	}
}

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

// TestWorkerDrainsWholeQueue processes every pending step of a (capability, provider) in one drain.
func TestWorkerDrainsWholeQueue(t *testing.T) {
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
	if err := drainWork(ctx, db, capTranscrever, provASRYouTube, runner); err != nil {
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

// TestEndToEndYouTubeFlow drives a single video from discovery to done through alternating reconcile
// passes and worker drains — the whole control loop end to end, WITH the two curation gates as real
// workers (both keep). The only seams are the fake worker runners and a fake activator; the workers
// run through the rara-addon SDK and rara-core's adapters.
func TestEndToEndYouTubeFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1")
	act := &fakeActivator{}
	r := NewReconciler(db, act)
	workers := []struct {
		capability, provider string
		runner               StepRunner
	}{
		{capGateBarato, provGateBarato, gateKeepRunner()},
		{capTranscrever, provASRYouTube, &fakeRunner{result: out("transcript-7")}},
		{capGateRico, provGateRico, gateKeepRunner()},
		{capDestilar, provDistill, &fakeRunner{result: out("distill-9")}},
	}

	// Run to a fixed point: reconcile, then let every worker drain, until the item is done.
	for pass := 0; pass < 10; pass++ {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d reconcile: %v", pass, err)
		}
		for _, w := range workers {
			if err := drainWork(ctx, db, w.capability, w.provider, w.runner); err != nil {
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
	// Both gates kept (the handlers recorded the decisions).
	if len(db.gateDecisions) != 2 {
		t.Errorf("expected 2 keep decisions, got %d", len(db.gateDecisions))
	}
	// on_demand providers (gate-barato, gate-rico, distill) were woken; the resident scribe was not.
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

// TestWorkerFiltersEmptyTranscript: a benign no-content result marks the step done with its
// output_ref AND curates the item out (terminal `filtered`, not failed), so it is never driven into
// a distill that must fail.
func TestWorkerFiltersEmptyTranscript(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{result: RunResult{OutputRef: "transcript-empty", Filtered: true}}

	if err := drainWork(ctx, db, capTranscrever, provASRYouTube, runner); err != nil {
		t.Fatalf("drainWork: %v", err)
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

// TestGateWorkerRecordsDecision: a gate worker claims a pending gate step, and for each cascade
// verdict (keep/drop/defer) the handler appends a gate_decisions row with the decision + score +
// decided_by + reason and marks the step DONE — but does NOT touch item status (the reconciler routes
// from the decision).
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
			// A pending gate_barato step at the metadata gate's seq (2), as the reconciler would
			// assign it (provider set so the provider-aware claim matches).
			mustStep(t, db, ItemStep{ItemID: itemID, Seq: 2, Capability: capGateBarato, Status: stepPending, AssignedProvider: provGateBarato})

			verdict := &GateVerdict{
				Decision: tc.decision, Score: gateScore(0.71),
				DecidedBy: tc.decidedBy, Reason: "unit-test verdict",
			}
			runner := &fakeRunner{result: RunResult{Gate: verdict}}
			if err := drainWork(ctx, db, capGateBarato, provGateBarato, runner); err != nil {
				t.Fatalf("drainWork: %v", err)
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

// TestWorkerRetriesTransientThenFails: a retryable miss (distill's batch hasn't produced the row
// yet) is re-queued instead of failing the item — until the attempt ceiling, after which it fails
// for good. One drain drives the full retry chain; if the errRetryable -> addon.ErrRetryable mapping
// were wrong the step would fail at attempt 1 instead of the ceiling.
func TestWorkerRetriesTransientThenFails(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)
	runner := &fakeRunner{err: errRetryable}

	if err := drainWork(ctx, db, capTranscrever, provASRYouTube, runner); err != nil {
		t.Fatal(err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepFailed {
		t.Errorf("after %d attempts the step should fail, got %q", maxStepAttempts, s.Status)
	}
	if s.Attempt != maxStepAttempts {
		t.Errorf("attempt = %d, want the ceiling %d", s.Attempt, maxStepAttempts)
	}
}

// itemGoneDB makes GetItem report not-found while delegating everything else to the mock, to
// exercise the orphan-step path without an (un-)deletable item in the mock.
type itemGoneDB struct{ *MockDatabase }

func (itemGoneDB) GetItem(context.Context, int) (Item, bool, error) { return Item{}, false, nil }

// TestWorkerItemNotFound: if the item vanished between claim and read, the orphan step is marked
// failed so it leaves the running set.
func TestWorkerItemNotFound(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := assignTranscrever(t, db)

	if err := drainWork(ctx, itemGoneDB{db}, capTranscrever, provASRYouTube, &fakeRunner{result: out("x")}); err != nil {
		t.Fatalf("drainWork: %v", err)
	}
	s := db.itemSteps[itemStepKey{itemID, 3}]
	if s.Status != stepFailed || s.Error != "item not found" {
		t.Errorf("orphan step = %+v, want failed 'item not found'", s)
	}
}

// TestWorkerStopsOnContextCancel: a cancelled context stops the drain promptly — the basis for
// graceful SIGTERM shutdown of the resident/on_demand workers.
func TestWorkerStopsOnContextCancel(t *testing.T) {
	db := newMockDatabase()
	assignTranscrever(t, db) // a pending step is waiting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := drainWork(ctx, db, capTranscrever, provASRYouTube, &fakeRunner{result: out("x")})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("drain should stop with context.Canceled, got %v", err)
	}
}
