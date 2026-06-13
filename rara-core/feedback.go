// feedback.go — Phase 3 deliverables #4 (explicit thumbs) and #5 (quarantine review).
//
// Feedback is NOT a fifth cascade layer; it tunes the others (ARCHITECTURE-2.0, "The learning
// loop"). This file is the minimal capture surface — two append-only signals into the
// `feedback` table — plus the quarantine review that resolves a deferred item. Turning those
// signals into a revised interest_profile is the Phase 6 learning loop (a deliberate stub
// here): this phase only RECORDS the signal and resolves the item.
//
// The functions are pure orchestration over the Database seam (no I/O of their own), so they
// are unit-tested with the MockDatabase. The CLI in main.go is a thin wrapper over them.
package main

import (
	"context"
	"fmt"
	"strconv"
)

// CaptureDistillationFeedback records explicit thumbs on a distillation: target=distillation,
// source=user_explicit (deliverable #4). The Phase 6 profile revision consumes these; this
// phase only captures them.
func CaptureDistillationFeedback(ctx context.Context, db Database, distillationID, signal string) error {
	if signal != signalUp && signal != signalDown {
		return fmt.Errorf("signal must be %q or %q, got %q", signalUp, signalDown, signal)
	}
	if distillationID == "" {
		return fmt.Errorf("distillation id is required")
	}
	return db.InsertFeedback(ctx, Feedback{
		TargetType: targetDistillation, TargetRef: distillationID,
		Signal: signal, Source: sourceUserExplicit,
	})
}

// ReviewQuarantine resolves a quarantined (deferred) item from human review, capturing the
// signal as feedback (source=quarantine_review, deliverable #5):
//
//	signal=up   -> RESCUE: override the deferring gate with a keep and return the item to the
//	               pipeline (cold-start false-negative recovery — the very reason quarantine
//	               exists), so it resumes from where the gate deferred it.
//	signal=down -> CONFIRM the drop: the item becomes terminal `filtered`.
//
// Either way the signal feeds the Phase 6 profile revision.
func ReviewQuarantine(ctx context.Context, db Database, itemID int, signal string) error {
	if signal != signalUp && signal != signalDown {
		return fmt.Errorf("signal must be %q or %q, got %q", signalUp, signalDown, signal)
	}
	it, found, err := db.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if !found || it.Status != itemQuarantine {
		return fmt.Errorf("item %d is not in quarantine", itemID)
	}
	if err := db.InsertFeedback(ctx, Feedback{
		TargetType: targetItem, TargetRef: strconv.Itoa(itemID),
		Signal: signal, Source: sourceQuarantineReview,
	}); err != nil {
		return err
	}

	if signal == signalDown {
		it.Status = itemFiltered // confirm the gate's defer as a drop
		_, err := db.UpsertItem(ctx, it)
		return err
	}

	// Rescue: append a keep decision for the deferring gate (gate_decisions is append-only, so
	// the keep — the highest id — overrides the defer the reconciler reads), then return the
	// item to the active set at the status its completed steps imply. The reconciler advances
	// it from there on the next pass.
	gate, found, err := latestDeferredGate(ctx, db, itemID)
	if err != nil {
		return err
	}
	if found {
		if err := db.InsertGateDecision(ctx, GateDecision{
			ItemID: itemID, Gate: gate, Decision: decisionKeep,
			DecidedBy: sourceQuarantineReview, Reason: "rescued by human review",
		}); err != nil {
			return err
		}
	}
	status, err := resumeStatus(ctx, db, it)
	if err != nil {
		return err
	}
	it.Status = status
	_, err = db.UpsertItem(ctx, it)
	return err
}

// latestDeferredGate finds which gate's latest decision deferred the item, so a rescue knows
// which one to override. gate_barato is checked before gate_rico (earlier in the flow).
func latestDeferredGate(ctx context.Context, db Database, itemID int) (string, bool, error) {
	for _, g := range []string{gateBarato, gateRico} {
		d, found, err := db.LatestGateDecision(ctx, itemID, g)
		if err != nil {
			return "", false, err
		}
		if found && d.Decision == decisionDefer {
			return g, true, nil
		}
	}
	return "", false, nil
}

// resumeStatus recomputes the non-terminal status a rescued item re-enters at, from its
// completed steps (the reconciler refines it on the next pass). A gate_barato rescue resumes
// at `discovered` (before transcription); a gate_rico rescue at `to_text` (already
// transcribed).
func resumeStatus(ctx context.Context, db Database, it Item) (string, error) {
	flowSteps, err := db.ListFlowSteps(ctx, it.FlowID)
	if err != nil {
		return "", err
	}
	steps, err := db.ListItemSteps(ctx, it.ID)
	if err != nil {
		return "", err
	}
	bySeq := make(map[int]ItemStep, len(steps))
	for _, s := range steps {
		bySeq[s.Seq] = s
	}
	status := computeItemStatus(flowSteps, bySeq)
	if status == itemDone {
		status = itemDistilled // never re-enter as terminal; let the reconciler finalize
	}
	return status, nil
}
