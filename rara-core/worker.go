// worker.go — Phase 1 deliverables #4 (claim contract) and #5 (worker shims).
//
// This is the pull side of the control plane. The reconciler writes an assignment (a
// pending item_step with a chosen provider); a worker PULLS it from Neon with
// SELECT ... FOR UPDATE SKIP LOCKED (the claim, in store_reads.go), runs the existing
// domain binary, and writes the result back as output_ref. Delivery is uniform (pull);
// only activation differs (resident scribe polls; on_demand distill is woken by the
// reconciler) — but both claim the same way.
//
// The SHIM is deliberately thin. It does NOT reimplement scribe/distill; it translates an
// item_steps assignment into the binary's *current* entrypoint and captures the domain row
// the binary wrote:
//
//   - scribe (transcrever) HAS a per-item entry: `--source <watch-url> --limit 1`. The
//     shim runs that one video, then reads transcripts.id for output_ref.
//   - distill (destilar) has NO per-item entry — it batch-pulls its own queue. The shim
//     triggers an idempotent batch run (which necessarily processes this transcript) and
//     then reads distillations.id for output_ref. The over-processing is harmless because
//     distill is idempotent; a per-item entrypoint is a deliberate later change to the
//     worker, out of scope for Phase 1's "don't touch domain logic".
//
// The exec + domain-row lookup live behind the StepRunner seam, so the claim/advance
// orchestration is unit-tested with a fake runner and zero I/O.
package main

import (
	"context"
	"errors"
	"log"
)

// maxStepAttempts caps how many times a transient (retryable) step is re-queued before it
// is failed for good — mirroring scribe/distill's own per-row attempt ceiling.
const maxStepAttempts = 5

// RunResult is what a StepRunner reports back. OutputRef is the worker-owned domain row id
// to record on the item_step. Filtered marks a benign, no-content outcome (e.g. an empty
// transcript with no speech): the step is legitimately done, but there is nothing to carry
// downstream, so the item is curated out (terminal `filtered`) instead of marched into a
// distill that must fail.
//
// Gate is non-nil ONLY for a curation-gate step (gate_barato / gate_rico): it carries the
// cascade's verdict. The worker records it as a gate_decisions row and marks the step done;
// it does NOT route the item — the reconciler reads the decision next pass and routes
// keep/drop/defer (deliverable #6), keeping judgement in the worker and routing in the
// control plane.
type RunResult struct {
	OutputRef string
	Filtered  bool
	Gate      *GateVerdict
}

// StepRunner executes one claimed step against its domain worker. A returned error means
// the step did not succeed; if it wraps errRetryable (e.g. distill's batch hasn't reached
// this row yet) the orchestration re-queues the step until maxStepAttempts, otherwise it
// marks it failed for the reconciler to act on next pass.
type StepRunner interface {
	Run(ctx context.Context, item Item, step ItemStep) (RunResult, error)
}

// Worker is one (capability, provider) pull loop: claim a pending step assigned to this
// provider, run it, write it back. A worker serves exactly one provider so it never claims a
// sibling provider's steps (transcrever -> asr-youtube vs asr-direct-audio).
type Worker struct {
	db         Database
	capability string
	provider   string
	runner     StepRunner
}

// NewWorker wires a worker for a (capability, provider) pair (e.g. transcrever/asr-youtube ->
// the scribe shim).
func NewWorker(db Database, capability, provider string, runner StepRunner) *Worker {
	return &Worker{db: db, capability: capability, provider: provider, runner: runner}
}

// RunOnce claims and processes a single step. It returns claimed=false when the queue is
// empty (the caller can sleep). One step per call keeps the unit of work small and the
// claim window short; the resident loop just calls this repeatedly.
func (w *Worker) RunOnce(ctx context.Context) (claimed bool, err error) {
	step, err := w.db.ClaimPendingStep(ctx, w.capability, w.provider)
	if err != nil {
		return false, err
	}
	if step == nil {
		return false, nil // nothing pending for this capability+provider
	}

	// Proof of life: this worker just pulled work as `assigned_provider`, so that provider
	// is alive — stamp its heartbeat so the router's health gate keeps routing to it.
	// Best-effort; a heartbeat write must never block processing the claimed step.
	if step.AssignedProvider != "" {
		if err := w.db.TouchProviderHeartbeat(ctx, step.AssignedProvider); err != nil {
			log.Printf("worker %s: heartbeat %s: %v", w.capability, step.AssignedProvider, err)
		}
	}

	item, found, err := w.db.GetItem(ctx, step.ItemID)
	if err != nil {
		return true, err
	}
	if !found {
		// The item vanished (cascade delete?) between claim and read. Mark the orphan
		// step failed so it leaves the running set; the row is harmless.
		return true, w.finish(ctx, *step, stepFailed, "", "item not found")
	}

	res, runErr := w.runner.Run(ctx, item, *step)
	if runErr != nil {
		// Transient (e.g. distill's batch hasn't produced this row yet): re-queue the
		// step as pending so a later claim retries it, until the attempt ceiling.
		if errors.Is(runErr, errRetryable) && step.Attempt < maxStepAttempts {
			log.Printf("worker %s: step item=%d seq=%d transient (attempt %d/%d): %v",
				w.capability, step.ItemID, step.Seq, step.Attempt, maxStepAttempts, runErr)
			return true, w.requeue(ctx, *step, runErr.Error())
		}
		log.Printf("worker %s: step item=%d seq=%d failed: %v", w.capability, step.ItemID, step.Seq, runErr)
		return true, w.finish(ctx, *step, stepFailed, "", runErr.Error())
	}

	if res.Gate != nil {
		// A curation gate judged the item. Record the decision (the audit + training
		// substrate); the gate step is legitimately done. The worker does NOT set item
		// status — the reconciler reads this decision next pass and routes the item
		// (keep -> advance, drop -> filtered, defer -> quarantine). w.capability is the
		// gate name (capGateBarato == gateBarato, capGateRico == gateRico).
		if err := w.db.InsertGateDecision(ctx, GateDecision{
			ItemID: item.ID, Gate: w.capability, Decision: res.Gate.Decision,
			Score: res.Gate.Score, Rank: res.Gate.Rank,
			DecidedBy: res.Gate.DecidedBy, Reason: res.Gate.Reason,
		}); err != nil {
			return true, err
		}
		return true, w.finish(ctx, *step, stepDone, res.OutputRef, "")
	}

	if res.Filtered {
		// Benign no-content: record the step done with its output, then curate the item
		// out. This is the one sanctioned case where a worker writes item status — a
		// terminal hand-off (the item leaves the active set; the reconciler never
		// contends), made here because emptiness is a domain fact only the worker knows.
		if err := w.finish(ctx, *step, stepDone, res.OutputRef, ""); err != nil {
			return true, err
		}
		return true, w.filterItem(ctx, item)
	}
	return true, w.finish(ctx, *step, stepDone, res.OutputRef, "")
}

// RunUntilDrained claims and processes steps until the queue is empty (the on_demand
// pattern: a woken Cloud Run job pulls its assignments, then exits). Stops on the first
// error or when ctx is done.
func (w *Worker) RunUntilDrained(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := w.RunOnce(ctx)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
	}
}

// finish writes the terminal step state back. It starts from the claimed step (preserving
// capability, assigned_provider, attempt and heartbeat that the claim stamped) and only
// flips status + output_ref/error — a full-record upsert, so the whole row is passed.
func (w *Worker) finish(ctx context.Context, step ItemStep, status, outputRef, errMsg string) error {
	step.Status = status
	step.OutputRef = outputRef
	step.Error = errMsg
	return w.db.UpsertItemStep(ctx, step)
}

// requeue returns a transiently-failed step to the pending frontier (heartbeat cleared so
// it reads as un-owned) so the next claim retries it. attempt is left as the claim bumped
// it — the ceiling in RunOnce reads it to stop eventually.
func (w *Worker) requeue(ctx context.Context, step ItemStep, errMsg string) error {
	step.Status = stepPending
	step.HeartbeatAt = nil
	step.OutputRef = ""
	step.Error = errMsg
	return w.db.UpsertItemStep(ctx, step)
}

// filterItem marks an item terminal `filtered` (curated out, not an error). See RunOnce.
func (w *Worker) filterItem(ctx context.Context, item Item) error {
	if item.Status == itemFiltered {
		return nil
	}
	item.Status = itemFiltered
	_, err := w.db.UpsertItem(ctx, item)
	return err
}
