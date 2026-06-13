// reconciler.go — Phase 1 deliverables #3 and #6: the level-triggered reconciler and
// the pass-through gates.
//
// The reconciler is a boring, auditable controller in the Kubernetes sense: it observes
// desired state (a flow's steps) versus actual state (an item's item_steps) and takes the
// single next action that closes the gap. It holds no state between passes, makes no
// network calls of its own, and is idempotent — running it twice on the same item changes
// nothing the second time. All judgement (the curation gates) is config-driven and, in
// Phase 1, a no-op keep.
//
// What the reconciler does NOT do: domain work. It never transcribes or distills. It
// writes an assignment (a pending item_step with a chosen provider) and waits; the worker
// pulls that assignment (worker.go) and writes the result back. The contract between them
// is the table, never a call.
//
// Per-item state machine driven here, walking flow_steps by seq:
//
//	coletar     -> auto-done   (the item EXISTS, so collection already happened)
//	gate_barato -> auto-keep   (pass-through: record a keep decision, mark done)
//	transcrever -> assign      (pending + provider; wait for the scribe shim)
//	gate_rico   -> auto-keep   (pass-through)
//	destilar    -> assign      (pending + provider; wait for the distill shim)
//
// One pass advances an item through every auto-satisfiable step until it either assigns a
// real work step (then waits) or completes. item status is recomputed from completed steps.
package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// defaultStaleAfter is how long a claimed (running) step may go without a heartbeat before
// the reconciler assumes its worker died and re-routes the step (timeout->fallback, see
// handleStaleStep). A conservative default; tunable via RECONCILE_STALE_SECONDS.
const defaultStaleAfter = 10 * time.Minute

// Activator wakes an on_demand provider so it starts pulling its assignment. For resident
// providers it is a no-op (they are always awake). The concrete implementation calls
// Cloud Run Jobs `run`; tests inject a fake to assert it fires for on_demand and not for
// resident. Phase 1 keeps it best-effort: a failed activation is logged, not fatal — the
// pending row remains and a later pass (or the worker's own cron) still drains it.
type Activator interface {
	Activate(ctx context.Context, p Provider) error
}

// Reconciler is the control loop. now, staleAfter and healthTTL are injectable so the
// heartbeat-liveness backstop and the router's health gate are deterministic in tests.
type Reconciler struct {
	db         Database
	activator  Activator
	router     *Router
	now        func() time.Time
	staleAfter time.Duration
	healthTTL  time.Duration
}

// NewReconciler wires a reconciler. A nil activator falls back to a logging no-op. now,
// staleAfter and healthTTL default to wall-clock, defaultStaleAfter and defaultHealthTTL;
// tests overwrite them directly.
func NewReconciler(db Database, activator Activator) *Reconciler {
	if activator == nil {
		activator = logActivator{}
	}
	return &Reconciler{
		db: db, activator: activator, router: NewRouter(db),
		now: time.Now, staleAfter: defaultStaleAfter, healthTTL: defaultHealthTTL,
	}
}

// ReconcileOnce runs a single pass over every active item. The always-on loop in main()
// calls this on a ticker; tests call it directly. A per-item error is logged and the pass
// continues — one stuck item must not stall the others (level-triggered: the next pass
// retries it).
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	items, err := r.db.ListActiveItems(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := r.reconcileItem(ctx, it); err != nil {
			log.Printf("reconcile item %d (%s/%s): %v", it.ID, it.Lane, it.SourceRef, err)
		}
	}
	return nil
}

// stepAction is the single decision the reconciler reaches for an item each pass.
type stepAction int

const (
	actComplete    stepAction = iota // every step done -> item terminal (done)
	actFail                          // a step failed   -> item terminal (failed)
	actWait                          // a work step is in flight -> nothing to do
	actMaterialize                   // the next step has no item_step yet -> create it
)

// reconcileItem walks the item's flow once, applying auto-satisfiable steps inline and
// stopping at the first real work step (which it assigns, then waits) or at completion.
func (r *Reconciler) reconcileItem(ctx context.Context, item Item) error {
	flowSteps, err := r.db.ListFlowSteps(ctx, item.FlowID)
	if err != nil {
		return err
	}
	if len(flowSteps) == 0 {
		return fmt.Errorf("flow %d has no steps", item.FlowID)
	}

	for {
		steps, err := r.db.ListItemSteps(ctx, item.ID)
		if err != nil {
			return err
		}
		bySeq := make(map[int]ItemStep, len(steps))
		for _, s := range steps {
			bySeq[s.Seq] = s
		}

		act, fs := nextAction(flowSteps, bySeq)
		switch act {
		case actComplete:
			return r.setItemStatus(ctx, item, itemDone)
		case actFail:
			return r.setItemStatus(ctx, item, itemFailed)
		case actWait:
			// A work step is pending/assigned/running — the worker owns it now. If it is
			// running but its heartbeat has gone stale, the worker likely died: re-route it
			// (timeout->fallback), then continue.
			if st := bySeq[fs.Seq]; st.Status == stepRunning && r.isStale(st) {
				if err := r.handleStaleStep(ctx, item, st); err != nil {
					return err
				}
			}
			// Keep the item's status in sync with what has completed and return.
			return r.syncItemStatus(ctx, item, flowSteps, bySeq)
		case actMaterialize:
			done, err := r.materialize(ctx, item, fs)
			if err != nil {
				return err
			}
			if !done {
				// A real work step was assigned; wait for the worker. Status reflects
				// only the steps completed so far (the just-assigned one is pending).
				return r.syncItemStatus(ctx, item, flowSteps, bySeq)
			}
			// An auto step (coletar/gate) was satisfied inline; loop to advance further.
		}
	}
}

// nextAction picks the single next thing to do, scanning steps in seq order:
//   - a missing step  -> materialize it
//   - a failed step   -> fail the item
//   - an in-flight step (pending/assigned/running) -> wait
//   - done/skipped    -> advance to the next step
//
// If every step is done/skipped, the item is complete.
func nextAction(flowSteps []FlowStep, bySeq map[int]ItemStep) (stepAction, FlowStep) {
	for _, fs := range flowSteps {
		s, ok := bySeq[fs.Seq]
		if !ok {
			return actMaterialize, fs
		}
		switch s.Status {
		case stepDone, stepSkipped:
			continue
		case stepFailed:
			return actFail, fs
		default: // pending | assigned | running
			return actWait, fs
		}
	}
	return actComplete, FlowStep{}
}

// materialize creates the item_step for a not-yet-started flow step. It returns done=true
// when the step was satisfied inline (coletar, pass-through gate) so the caller can keep
// advancing, and done=false when a real work step was assigned (caller must wait).
func (r *Reconciler) materialize(ctx context.Context, item Item, fs FlowStep) (done bool, err error) {
	switch {
	case fs.Capability == capColetar:
		// The item exists, so collection already produced it. Record coletar as done,
		// linking back to the source video (the spine's natural key).
		return true, r.db.UpsertItemStep(ctx, ItemStep{
			ItemID: item.ID, Seq: fs.Seq, Capability: fs.Capability,
			Status: stepDone, OutputRef: item.SourceRef,
		})

	case isGate(fs.Capability):
		// Pass-through gate: always keep (real curation is Phase 3). Record the audit
		// decision, then mark the step done so the flow advances.
		if err := r.db.InsertGateDecision(ctx, GateDecision{
			ItemID: item.ID, Gate: fs.Capability, Decision: decisionKeep,
			DecidedBy: gateDecidedByPassthrough, Reason: "phase-1 pass-through gate",
		}); err != nil {
			return false, err
		}
		return true, r.db.UpsertItemStep(ctx, ItemStep{
			ItemID: item.ID, Seq: fs.Seq, Capability: fs.Capability, Status: stepDone,
		})

	default:
		// A real work step (transcrever, destilar). Route to a provider by policy
		// (cost<->quality + constraints + health + fallback) and write a pending assignment
		// for the worker to pull; wake on_demand providers.
		prov, ok, err := r.router.Select(ctx, fs.Capability, r.now(), r.healthTTL)
		if err != nil {
			return false, err
		}
		if !ok {
			// No eligible provider (none registered, all constraint-violating, or all
			// unhealthy) — leave the step unmaterialized; a later pass assigns it once one
			// becomes eligible. Level-triggered: nothing to undo.
			return false, fmt.Errorf("no eligible provider for capability %q (constraints/health unmet)", fs.Capability)
		}
		if err := r.db.UpsertItemStep(ctx, ItemStep{
			ItemID: item.ID, Seq: fs.Seq, Capability: fs.Capability,
			Status: stepPending, AssignedProvider: prov.Name,
		}); err != nil {
			return false, err
		}
		r.activateIfOnDemand(ctx, item, prov)
		return false, nil
	}
}

// handleStaleStep recovers a running step whose worker has gone silent (heartbeat older
// than staleAfter -> the worker likely died). Per the architecture's timeout policy it
// "re-fire[s] activation OR fall[s] back to the next provider": it asks the router for the
// next eligible provider EXCLUDING the dead one (so it honours constraints + health and
// never picks the same dead worker). If an alternative exists the step fails over to it; if
// none does, the step is re-queued on the same provider to be re-claimed should it revive.
// Either way the step returns to the pending frontier (heartbeat cleared) and, when the
// chosen provider is on_demand, activation is re-fired.
func (r *Reconciler) handleStaleStep(ctx context.Context, item Item, st ItemStep) error {
	dead := st.AssignedProvider
	next, ok, err := r.router.Select(ctx, st.Capability, r.now(), r.healthTTL, dead)
	if err != nil {
		return err
	}

	chosen := next
	if ok {
		log.Printf("reconcile item %d seq %d: stale heartbeat, failing over %s -> %s", item.ID, st.Seq, dead, next.Name)
	} else {
		// No alternative provider: re-queue on the same one (it may come back). Read it to
		// decide whether to re-fire activation; if it vanished from config, re-queue
		// unassigned and let a later pass route the step afresh.
		log.Printf("reconcile item %d seq %d: stale heartbeat, no fallback, re-queuing %s", item.ID, st.Seq, dead)
		p, found, gerr := r.db.GetProvider(ctx, dead)
		if gerr != nil {
			return gerr
		}
		if found {
			chosen = p
		} else {
			chosen = Provider{} // empty name -> NULL assigned_provider; re-route next pass
		}
	}

	st.Status = stepPending
	st.HeartbeatAt = nil
	st.AssignedProvider = chosen.Name
	if err := r.db.UpsertItemStep(ctx, st); err != nil {
		return err
	}
	r.activateIfOnDemand(ctx, item, chosen)
	return nil
}

// activateIfOnDemand wakes an on_demand provider so it starts pulling. Resident providers
// and the empty (unassigned) provider are skipped. Best-effort: a failed wake is logged,
// not fatal — the pending assignment stands and a later pass (or the worker's own schedule)
// still drains it.
func (r *Reconciler) activateIfOnDemand(ctx context.Context, item Item, prov Provider) {
	if prov.Name == "" || prov.Activation != activationOnDemand {
		return
	}
	if err := r.activator.Activate(ctx, prov); err != nil {
		log.Printf("activate %s for item %d: %v", prov.Name, item.ID, err)
	}
}

// syncItemStatus recomputes the item's status from its completed steps and persists it if
// it changed (avoids a needless write + updated_at churn each pass).
func (r *Reconciler) syncItemStatus(ctx context.Context, item Item, flowSteps []FlowStep, bySeq map[int]ItemStep) error {
	want := computeItemStatus(flowSteps, bySeq)
	if want == item.Status {
		return nil
	}
	return r.setItemStatus(ctx, item, want)
}

func (r *Reconciler) setItemStatus(ctx context.Context, item Item, status string) error {
	if item.Status == status {
		return nil
	}
	item.Status = status
	_, err := r.db.UpsertItem(ctx, item)
	return err
}

// computeItemStatus maps step completion to the item lifecycle status. The to_text /
// distilled / done milestones are driven by which capability has completed:
//
//	all steps done            -> done
//	destilar done             -> distilled
//	transcrever done          -> to_text
//	otherwise (early steps)   -> discovered
func computeItemStatus(flowSteps []FlowStep, bySeq map[int]ItemStep) string {
	doneCaps := make(map[string]bool)
	allDone := true
	for _, fs := range flowSteps {
		s, ok := bySeq[fs.Seq]
		if ok && (s.Status == stepDone || s.Status == stepSkipped) {
			doneCaps[fs.Capability] = true
			continue
		}
		allDone = false
	}
	switch {
	case allDone:
		return itemDone
	case doneCaps[capDestilar]:
		return itemDistilled
	case doneCaps[capTranscrever]:
		return itemToText
	default:
		return itemDiscovered
	}
}

// isStale reports whether a running step's worker has likely died: it has a heartbeat
// (stamped at claim) older than staleAfter. A step with no heartbeat is never stale.
func (r *Reconciler) isStale(s ItemStep) bool {
	return s.HeartbeatAt != nil && r.now().Sub(*s.HeartbeatAt) > r.staleAfter
}

// isGate reports whether a capability is a curation gate.
func isGate(capability string) bool {
	return capability == capGateBarato || capability == capGateRico
}

// gateDecidedByPassthrough is the gate_decisions.decided_by tag for Phase 1's no-op gates,
// distinguishing them in the audit trail from the Phase 3 rules/profile/llm-judge layers.
const gateDecidedByPassthrough = "passthrough"

// logActivator is the default Activator: it records the wake intent without calling any
// cloud API. The real Cloud Run Jobs `run` client is wired at deploy (it needs
// run.jobs.run on the reconciler's service account); until then this keeps the loop
// runnable and the tests pure.
type logActivator struct{}

func (logActivator) Activate(_ context.Context, p Provider) error {
	log.Printf("activate (noop): would wake on_demand provider %q (runtime=%s)", p.Name, p.Runtime)
	return nil
}
