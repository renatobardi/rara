package main

import (
	"context"
	"strconv"
	"testing"
)

// TestCaptureDistillationFeedback: valid thumbs append a feedback row
// (target=distillation, source=user_explicit); bad input is rejected without a write.
func TestCaptureDistillationFeedback(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()

	if err := CaptureDistillationFeedback(ctx, db, "42", signalUp); err != nil {
		t.Fatalf("capture up: %v", err)
	}
	if err := CaptureDistillationFeedback(ctx, db, "42", signalDown); err != nil {
		t.Fatalf("capture down: %v", err)
	}
	if len(db.feedback) != 2 {
		t.Fatalf("expected 2 feedback rows, got %d", len(db.feedback))
	}
	f := db.feedback[0]
	if f.TargetType != targetDistillation || f.TargetRef != "42" || f.Signal != signalUp || f.Source != sourceUserExplicit {
		t.Errorf("feedback = %+v, want distillation/42/up/user_explicit", f)
	}

	// Bad signal and empty id are rejected, no extra rows.
	if err := CaptureDistillationFeedback(ctx, db, "42", "meh"); err == nil {
		t.Error("invalid signal should error")
	}
	if err := CaptureDistillationFeedback(ctx, db, "", signalUp); err == nil {
		t.Error("empty distillation id should error")
	}
	if len(db.feedback) != 2 {
		t.Errorf("rejected calls must not write: %d rows", len(db.feedback))
	}
}

// quarantineOne drives a video into terminal quarantine via the real reconciler path: the
// metadata gate defers it.
func quarantineOne(t *testing.T, db *MockDatabase) int {
	t.Helper()
	ctx := context.Background()
	itemID := seedAndIngestOne(t, db, "vid1")
	r := NewReconciler(db)
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionDefer)
	if err := r.ReconcileOnce(ctx); err != nil { // route defer -> quarantine
		t.Fatal(err)
	}
	if db.itemByID[itemID].Status != itemQuarantine {
		t.Fatalf("setup: item not quarantined, got %q", db.itemByID[itemID].Status)
	}
	return itemID
}

// TestReviewQuarantineDownConfirmsDrop: a down review records the signal and makes the item
// terminal `filtered`.
func TestReviewQuarantineDownConfirmsDrop(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := quarantineOne(t, db)

	if err := ReviewQuarantine(ctx, db, itemID, signalDown); err != nil {
		t.Fatalf("review down: %v", err)
	}
	if got := db.itemByID[itemID].Status; got != itemFiltered {
		t.Errorf("item status = %q, want filtered after a down review", got)
	}
	// Feedback captured with the quarantine_review source.
	if len(db.feedback) != 1 {
		t.Fatalf("expected 1 feedback row, got %d", len(db.feedback))
	}
	f := db.feedback[0]
	if f.TargetType != targetItem || f.TargetRef != strconv.Itoa(itemID) ||
		f.Signal != signalDown || f.Source != sourceQuarantineReview {
		t.Errorf("feedback = %+v, want item/%d/down/quarantine_review", f, itemID)
	}
	// No longer in the quarantine sample.
	q, _ := db.ListQuarantinedItems(ctx)
	if len(q) != 0 {
		t.Errorf("dropped item should leave quarantine, got %d", len(q))
	}
}

// TestReviewQuarantineUpRescues: an up review records the signal, overrides the deferring
// gate with a keep, and returns the item to the active set — so the reconciler resumes it
// (the cold-start false-negative recovery).
func TestReviewQuarantineUpRescues(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := quarantineOne(t, db)

	if err := ReviewQuarantine(ctx, db, itemID, signalUp); err != nil {
		t.Fatalf("review up: %v", err)
	}
	// Feedback captured.
	if len(db.feedback) != 1 || db.feedback[0].Source != sourceQuarantineReview || db.feedback[0].Signal != signalUp {
		t.Errorf("feedback = %+v, want one up/quarantine_review", db.feedback)
	}
	// The latest gate_barato decision is now a keep (overriding the defer).
	d, found, _ := db.LatestGateDecision(ctx, itemID, gateBarato)
	if !found || d.Decision != decisionKeep || d.DecidedBy != sourceQuarantineReview {
		t.Errorf("latest gate decision = %+v, want keep by quarantine_review", d)
	}
	// Item is back in the active set at `discovered` (gate_barato rescue: pre-transcription).
	if got := db.itemByID[itemID].Status; got != itemDiscovered {
		t.Errorf("rescued item status = %q, want discovered", got)
	}
	q, _ := db.ListQuarantinedItems(ctx)
	if len(q) != 0 {
		t.Errorf("rescued item should leave quarantine, got %d", len(q))
	}

	// And the reconciler resumes it: the next pass advances past the now-kept gate and
	// assigns transcrever.
	if err := NewReconciler(db).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 3); !ok || s.Status != stepPending || s.AssignedProvider != provASRYouTube {
		t.Errorf("after rescue, transcrever step = %+v, want pending+asr-youtube", s)
	}
}

// TestReviewQuarantineRejectsNonQuarantine: reviewing an item that is not in quarantine is an
// error (and writes nothing).
func TestReviewQuarantineRejectsNonQuarantine(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1") // status discovered, not quarantine

	if err := ReviewQuarantine(ctx, db, itemID, signalUp); err == nil {
		t.Error("reviewing a non-quarantined item should error")
	}
	if err := ReviewQuarantine(ctx, db, 9999, signalUp); err == nil {
		t.Error("reviewing an unknown item should error")
	}
	if len(db.feedback) != 0 {
		t.Errorf("rejected reviews must not write feedback: %d rows", len(db.feedback))
	}
	// Bad signal rejected too.
	if err := ReviewQuarantine(ctx, db, itemID, "meh"); err == nil {
		t.Error("invalid signal should error")
	}
}
