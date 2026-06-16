package main

import (
	"context"
	"testing"
)

// seedRequeueFixture sets up a minimal environment:
// one capability, one flow, one item, and N item_steps of the given capability+status.
// Returns the item ID and the step seq values for each seeded step.
func seedRequeueFixture(t *testing.T, db *MockDatabase, capability string, stepStatus string, n int) (itemID int, seqs []int) {
	t.Helper()
	ctx := context.Background()
	_ = db.UpsertCapability(ctx, Capability{Name: capability})
	fid := seedFlow(t, db)
	// Item starts in a terminal-ish status that should be reset by requeue.
	iid, err := db.UpsertItem(ctx, Item{Lane: "test", SourceRef: "ref-requeue", FlowID: fid, FlowVersion: 1, Status: itemFailed})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	for i := 0; i < n; i++ {
		seq := i + 1
		if err := db.UpsertItemStep(ctx, ItemStep{
			ItemID: iid, Seq: seq, Capability: capability, Status: stepStatus, Attempt: 2,
		}); err != nil {
			t.Fatalf("seed step seq=%d: %v", seq, err)
		}
		seqs = append(seqs, seq)
	}
	return iid, seqs
}

// TestRequeueSteps_ResetsStepsAndItem verifies the happy path:
// failed steps reset to pending (attempt=0, heartbeat_at=nil), parent item advanced.
func TestRequeueSteps_ResetsStepsAndItem(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	iid, _ := seedRequeueFixture(t, db, capTranscrever, stepFailed, 2)

	n, err := db.RequeueSteps(ctx, capTranscrever, stepFailed, 0, itemToText)
	if err != nil {
		t.Fatalf("RequeueSteps: %v", err)
	}
	if n != 2 {
		t.Errorf("requeued %d steps, want 2", n)
	}
	for seq := 1; seq <= 2; seq++ {
		s := db.itemSteps[itemStepKey{iid, seq}]
		if s.Status != stepPending {
			t.Errorf("step seq=%d: status=%q, want pending", seq, s.Status)
		}
		if s.Attempt != 0 {
			t.Errorf("step seq=%d: attempt=%d, want 0", seq, s.Attempt)
		}
		if s.HeartbeatAt != nil {
			t.Errorf("step seq=%d: heartbeat_at should be nil after requeue", seq)
		}
	}
	if got := db.itemByID[iid].Status; got != itemToText {
		t.Errorf("item status = %q, want %q", got, itemToText)
	}
}

// TestRequeueSteps_LimitHonored verifies that --limit N resets exactly N steps.
func TestRequeueSteps_LimitHonored(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	iid, _ := seedRequeueFixture(t, db, capTranscrever, stepFailed, 3)

	n, err := db.RequeueSteps(ctx, capTranscrever, stepFailed, 1, itemToText)
	if err != nil {
		t.Fatalf("RequeueSteps: %v", err)
	}
	if n != 1 {
		t.Errorf("requeued %d steps with limit=1, want 1", n)
	}
	// Exactly 1 step should be pending; the other 2 stay failed.
	pending, failed := 0, 0
	for seq := 1; seq <= 3; seq++ {
		switch db.itemSteps[itemStepKey{iid, seq}].Status {
		case stepPending:
			pending++
		case stepFailed:
			failed++
		}
	}
	if pending != 1 || failed != 2 {
		t.Errorf("want 1 pending + 2 failed, got %d pending + %d failed", pending, failed)
	}
}

// TestRequeueSteps_OnlyFromStatus verifies that steps NOT in fromStatus are never touched.
func TestRequeueSteps_OnlyFromStatus(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	fid := seedFlow(t, db)
	iid, _ := db.UpsertItem(ctx, Item{Lane: "test", SourceRef: "ref-mix", FlowID: fid, FlowVersion: 1, Status: itemFailed})
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: iid, Seq: 1, Capability: capTranscrever, Status: stepFailed, Attempt: 1})
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: iid, Seq: 2, Capability: capTranscrever, Status: stepDone, Attempt: 1})
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: iid, Seq: 3, Capability: capTranscrever, Status: stepRunning, Attempt: 1})

	n, err := db.RequeueSteps(ctx, capTranscrever, stepFailed, 0, itemToText)
	if err != nil {
		t.Fatalf("RequeueSteps: %v", err)
	}
	if n != 1 {
		t.Errorf("requeued %d steps, want 1 (only the failed one)", n)
	}
	if db.itemSteps[itemStepKey{iid, 2}].Status != stepDone {
		t.Error("done step must not be touched")
	}
	if db.itemSteps[itemStepKey{iid, 3}].Status != stepRunning {
		t.Error("running step must not be touched")
	}
}

// TestRequeueSteps_ItemStatusOverride verifies --item-status overrides the derived status.
func TestRequeueSteps_ItemStatusOverride(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	iid, _ := seedRequeueFixture(t, db, capGateBarato, stepFailed, 1)

	// Override: gate_barato would derive discovered, but we pass quarantine explicitly.
	n, err := db.RequeueSteps(ctx, capGateBarato, stepFailed, 0, itemQuarantine)
	if err != nil {
		t.Fatalf("RequeueSteps: %v", err)
	}
	if n != 1 {
		t.Errorf("requeued %d, want 1", n)
	}
	if got := db.itemByID[iid].Status; got != itemQuarantine {
		t.Errorf("item status = %q, want quarantine (override)", got)
	}
}

// TestRequeueSteps_Idempotent verifies a second call is a no-op (0 steps reset).
func TestRequeueSteps_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	seedRequeueFixture(t, db, capTranscrever, stepFailed, 2)

	_, _ = db.RequeueSteps(ctx, capTranscrever, stepFailed, 0, itemToText)
	// Second call: steps are now pending, not failed — nothing matches.
	n, err := db.RequeueSteps(ctx, capTranscrever, stepFailed, 0, itemToText)
	if err != nil {
		t.Fatalf("second RequeueSteps: %v", err)
	}
	if n != 0 {
		t.Errorf("second call requeued %d, want 0 (idempotent)", n)
	}
}

// TestRequeueSteps_ZeroMatchesIsNoOp verifies no-op when no steps match.
func TestRequeueSteps_ZeroMatchesIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})

	n, err := db.RequeueSteps(ctx, capTranscrever, stepFailed, 0, itemToText)
	if err != nil {
		t.Fatalf("RequeueSteps on empty db: %v", err)
	}
	if n != 0 {
		t.Errorf("no steps exist, requeued %d, want 0", n)
	}
}

// TestCapabilityItemStatusMap verifies the static map covers the known capability rail.
func TestCapabilityItemStatusMap(t *testing.T) {
	cases := map[string]string{
		capGateBarato:  itemDiscovered,
		capTranscrever: itemToText,
		capGateRico:    itemToText,
		capDestilar:    itemDistilled,
	}
	for cap, want := range cases {
		got, ok := capabilityItemStatus[cap]
		if !ok {
			t.Errorf("capability %q not in capabilityItemStatus map", cap)
			continue
		}
		if got != want {
			t.Errorf("capabilityItemStatus[%q] = %q, want %q", cap, got, want)
		}
	}
}
