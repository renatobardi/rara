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
	"log"
)

// StepRunner executes one claimed step against its domain worker and returns the
// output_ref (the worker-owned domain row id, e.g. transcripts.id / distillations.id) to
// record on the item_step. An error means the step failed; the orchestration marks it
// failed and the reconciler decides what to do next pass.
type StepRunner interface {
	Run(ctx context.Context, item Item, step ItemStep) (outputRef string, err error)
}

// Worker is one capability's pull loop: claim a pending step, run it, write it back.
type Worker struct {
	db         Database
	capability string
	runner     StepRunner
}

// NewWorker wires a worker for a capability (e.g. transcrever -> the scribe shim).
func NewWorker(db Database, capability string, runner StepRunner) *Worker {
	return &Worker{db: db, capability: capability, runner: runner}
}

// RunOnce claims and processes a single step. It returns claimed=false when the queue is
// empty (the caller can sleep). One step per call keeps the unit of work small and the
// claim window short; the resident loop just calls this repeatedly.
func (w *Worker) RunOnce(ctx context.Context) (claimed bool, err error) {
	step, err := w.db.ClaimPendingStep(ctx, w.capability)
	if err != nil {
		return false, err
	}
	if step == nil {
		return false, nil // nothing pending for this capability
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

	outputRef, runErr := w.runner.Run(ctx, item, *step)
	if runErr != nil {
		log.Printf("worker %s: step item=%d seq=%d failed: %v", w.capability, step.ItemID, step.Seq, runErr)
		return true, w.finish(ctx, *step, stepFailed, "", runErr.Error())
	}
	return true, w.finish(ctx, *step, stepDone, outputRef, "")
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
