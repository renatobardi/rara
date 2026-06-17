// reconciler.go — the level-triggered reconciler (Phase 1) plus Phase 3 gate routing.
//
// The reconciler is a boring, auditable controller in the Kubernetes sense: it observes
// desired state (a flow's steps) versus actual state (an item's item_steps) and takes the
// single next action that closes the gap. It holds no state between passes, makes no
// network calls of its own, and is idempotent — running it twice on the same item changes
// nothing the second time. All JUDGEMENT lives in the gate workers (the cascade in
// gates.go); the control plane only ROUTES the decision they record — deterministic, never
// model-driven.
//
// What the reconciler does NOT do: domain work. It never transcribes, distills, or judges.
// It writes an assignment (a pending item_step with a chosen provider) and waits; the worker
// pulls that assignment (worker.go) and writes the result back. The contract between them
// is the table, never a call.
//
// Per-item state machine driven here, walking flow_steps by seq:
//
//	coletar     -> auto-done   (the item EXISTS, so collection already happened)
//	gate_barato -> assign      (a real worker now; the gate judges metadata, records a
//	                            gate_decision, marks the step done)
//	transcrever -> assign      (pending + provider; wait for the scribe app, rara-scribe)
//	gate_rico   -> assign      (a real worker; the gate judges the full text)
//	destilar    -> assign      (pending + provider; wait for the distill app, rara-distill)
//
// A completed gate is ROUTED from its recorded decision (gateTerminalStatus): keep advances,
// drop -> terminal filtered, defer -> terminal quarantine. One pass advances an item through
// every auto-satisfiable step until it assigns a work step (then waits), routes a gate to a
// terminal, or completes. item status is recomputed from completed steps.
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

// defaultReactivateBackoff is the anti-stampede window for self-healing re-activation
// (reactivateStalled). After a SUCCESSFUL activation, a single woken on_demand worker claims
// until its queue drains (one Cloud Run execution pulling via SKIP LOCKED), so the reconciler
// must NOT re-fire activation while pending work remains within this window — doing so would
// spawn concurrent executions (the "swarm" bug). Tunable via REACTIVATE_BACKOFF_SECONDS.
const defaultReactivateBackoff = 3 * time.Minute

// Activator wakes a provider so it starts pulling its assignment NOW instead of waiting for its
// next poll tick (symmetric activation, architecture §4). The reconciler calls it for EVERY
// assignment; the concrete Activator dispatches by provider shape — runtime=cloudrun via Cloud Run
// Jobs `run`, activation=resident via a tailnet poke (see activator.go). It is best-effort: a failed
// activation is logged, not fatal — the pending row remains and the worker's own poll (the safety
// net) still drains it. Tests inject a fake to assert the right path fires per provider type.
type Activator interface {
	Activate(ctx context.Context, p Provider) error
}

// Reconciler is the control loop. now, staleAfter, healthTTL and reactivateBackoff are
// injectable so the heartbeat-liveness backstop, the router's health gate and the
// anti-stampede re-activation window are deterministic in tests.
type Reconciler struct {
	db                Database
	activator         Activator
	router            *Router
	now               func() time.Time
	staleAfter        time.Duration
	healthTTL         time.Duration
	reactivateBackoff time.Duration

	// activatedThisPass coalesces wakes WITHIN a single ReconcileOnce: a provider is activated at
	// most once per pass, however many items were assigned to it this pass. One wake drains the
	// whole queue for that provider (a poke coalesces on the worker; a Cloud Run `run` execution
	// drains via SKIP LOCKED), so N items assigning the same provider must not fan out into N job
	// executions / N pokes to a sleeping Mac. It is reset at the start of every pass — no state is
	// carried BETWEEN passes (ReconcileOnce is never called concurrently on one Reconciler).
	activatedThisPass map[string]bool

	// pendingThisPass is the set of providers that still hold a pending (assigned-but-unclaimed)
	// step at the frontier this pass. It feeds the self-healing re-activation (reactivateStalled):
	// an on_demand cloudrun provider with pending work that did not wake (its activation failed or
	// timed out) has no poll safety net, so the reconciler re-fires its activation. Reset every pass.
	pendingThisPass map[string]bool

	// lastActivatedAt records the wall time of each provider's last SUCCESSFUL activation and,
	// unlike activatedThisPass, PERSISTS BETWEEN passes. It anchors the anti-stampede backoff: a
	// recently-woken on_demand worker is still draining, so reactivateStalled must not re-fire
	// within reactivateBackoff of the recorded success. A FAILED activation never writes here, so
	// the next pass retries it (no execution was started -> no swarm, just a legitimate retry).
	lastActivatedAt map[string]time.Time

	// Auto-ingest: sources are injected at wiring time; ingestEveryN controls cadence.
	// 0 = disabled (single-pass mode and tests that don't set sources).
	// N > 0 = ingest every Nth pass. Set by runReconcile when --loop is active.
	yt           SpineSource
	pod          PodcastSource
	email        EmailSource
	news         NewsSource
	li           LinkedInSource
	ingestEveryN int
	passCount    int
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
		reactivateBackoff: defaultReactivateBackoff,
		lastActivatedAt:   make(map[string]time.Time),
	}
}

// ingestOnce discovers new items from each configured source lane. Best-effort: a lane whose
// flow is not seeded is skipped silently (no log); a DB error on GetFlow or an ingest source
// error is logged but does not block the remaining lanes. Called from ReconcileOnce per the
// ingestEveryN cadence.
//
// The pre-check GetFlow is intentional: IngestYouTube/Podcast/Email also call GetFlow
// internally, producing two reads per lane per pass (1 round-trip wasted). The alternative
// — calling Ingest* unconditionally and treating the "not seeded" error as a skip — would
// log a spurious error instead of skipping silently, violating the spec. Two reads at a 30s
// cadence on a low-traffic control plane are acceptable.
//
// Disabled lanes are also skipped silently here (same as !found): checking f.Enabled before
// calling IngestX avoids both the redundant second GetFlow and a log line every 30s.
//
func (r *Reconciler) ingestOnce(ctx context.Context) {
	if r.yt != nil {
		if f, found, err := r.db.GetFlow(ctx, youtubeFlowName); err != nil {
			log.Printf("auto-ingest youtube: %v", err)
		} else if found && f.Enabled {
			if _, err := IngestYouTube(ctx, r.db, r.yt); err != nil {
				log.Printf("auto-ingest youtube: %v", err)
			}
		}
	}
	if r.pod != nil {
		if f, found, err := r.db.GetFlow(ctx, podcastFlowName); err != nil {
			log.Printf("auto-ingest podcast: %v", err)
		} else if found && f.Enabled {
			if _, err := IngestPodcast(ctx, r.db, r.pod); err != nil {
				log.Printf("auto-ingest podcast: %v", err)
			}
		}
	}
	if r.email != nil {
		if f, found, err := r.db.GetFlow(ctx, emailFlowName); err != nil {
			log.Printf("auto-ingest email: %v", err)
		} else if found && f.Enabled {
			if _, err := IngestEmail(ctx, r.db, r.email); err != nil {
				log.Printf("auto-ingest email: %v", err)
			}
		}
	}
	if r.news != nil {
		if f, found, err := r.db.GetFlow(ctx, newsFlowName); err != nil {
			log.Printf("auto-ingest news: %v", err)
		} else if found && f.Enabled {
			if _, err := IngestFeed(ctx, r.db, r.news); err != nil {
				log.Printf("auto-ingest news: %v", err)
			}
		}
	}
	if r.li != nil {
		if f, found, err := r.db.GetFlow(ctx, linkedinFlowName); err != nil {
			log.Printf("auto-ingest linkedin: %v", err)
		} else if found && f.Enabled {
			if _, err := IngestLinkedIn(ctx, r.db, r.li); err != nil {
				log.Printf("auto-ingest linkedin: %v", err)
			}
		}
	}
}

// ReconcileOnce runs a single pass over every active item. The always-on loop in main()
// calls this on a ticker; tests call it directly. A per-item error is logged and the pass
// continues — one stuck item must not stall the others (level-triggered: the next pass
// retries it).
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	if r.ingestEveryN > 0 {
		r.passCount++
		if r.passCount%r.ingestEveryN == 0 {
			r.ingestOnce(ctx)
		}
	}
	r.activatedThisPass = make(map[string]bool) // coalesce wakes within this pass (see activate)
	r.pendingThisPass = make(map[string]bool)   // providers with pending work this pass (see reactivateStalled)
	items, err := r.db.ListActiveItems(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := r.reconcileItem(ctx, it); err != nil {
			log.Printf("reconcile item %d (%s/%s): %v", it.ID, it.Lane, it.SourceRef, err)
		}
	}
	// Self-healing backstop: re-fire activation for on_demand cloudrun providers whose pending
	// work never woke (assignment-time activation failed/timed out). Best-effort, anti-stampede.
	r.reactivateStalled(ctx)
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

		// A completed gate routes the item from its recorded decision (deliverable #6): the
		// gate worker judged and marked the step done; the control plane reads the decision
		// and acts — drop -> terminal filtered, defer -> terminal quarantine. A keep falls
		// through to nextAction, which advances past the done gate like any other step.
		if status, terminal, err := r.gateTerminalStatus(ctx, item, flowSteps, bySeq); err != nil {
			return err
		} else if terminal {
			return r.setItemStatus(ctx, item, status)
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
			st := bySeq[fs.Seq]
			switch {
			case st.Status == stepRunning && r.isStale(st):
				if err := r.handleStaleStep(ctx, item, st); err != nil {
					return err
				}
			case st.Status == stepPending && st.AssignedProvider != "":
				// Pending-but-unclaimed: the assignment is on the frontier but no worker has
				// picked it up. Note the provider so reactivateStalled can re-fire its wake if
				// it is an on_demand cloudrun job with no poll safety net (self-healing, A2).
				r.pendingThisPass[st.AssignedProvider] = true
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

	default:
		// A real work step — transcrever, destilar, OR a curation gate (gate_barato /
		// gate_rico are workers now, no longer pass-through). Route to a provider by policy
		// (cost<->quality + constraints + health + fallback) and write a pending assignment
		// for the worker to pull; wake on_demand providers.
		prov, ok, err := r.router.Select(ctx, fs.Capability, item, r.now(), r.healthTTL)
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
		r.activate(ctx, item, prov)
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
	next, ok, err := r.router.Select(ctx, st.Capability, item, r.now(), r.healthTTL, dead)
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
	r.activate(ctx, item, chosen)
	return nil
}

// activate wakes the provider an assignment was just routed to (symmetric activation): it is called
// for EVERY assignment, and the Activator decides per provider HOW to wake it (Cloud Run `run` for
// cloudrun, tailnet poke for residents, no-op otherwise — see activator.go). Only the empty
// (unassigned) provider is skipped, there being nothing to wake. Best-effort: a failed wake is
// logged, not fatal — the pending assignment stands and the worker's own poll still drains it.
func (r *Reconciler) activate(ctx context.Context, item Item, prov Provider) {
	if prov.Name == "" || r.activatedThisPass[prov.Name] {
		return // nothing to wake, or already woken this pass (one wake drains the whole queue)
	}
	r.activatedThisPass[prov.Name] = true
	if err := r.activator.Activate(ctx, prov); err != nil {
		log.Printf("activate %s for item %d: %v", prov.Name, item.ID, err)
		return // a failed wake does NOT anchor the backoff — self-healing retries next pass
	}
	// A successful wake anchors the anti-stampede window: the woken worker now drains the queue,
	// so reactivateStalled must not re-fire within reactivateBackoff (see reactivateStalled).
	r.lastActivatedAt[prov.Name] = r.now()
}

// reactivateStalled is the self-healing backstop for on_demand cloudrun providers (A2). After the
// per-item pass, it re-fires activation for every provider that still holds pending work but did
// NOT wake — the typical cause being an assignment-time activation that failed or timed out on a
// cold start. A scale-to-zero Cloud Run Job has no poll safety net, so without this a stuck step
// stays pending forever until someone runs `gcloud run jobs execute` by hand.
//
// The anti-stampede invariant is the whole point:
//   - A SUCCESSFUL activation anchors lastActivatedAt; while now-lastActivatedAt < reactivateBackoff
//     we do NOT re-fire. One woken on_demand worker claims until its queue drains (one execution
//     pulling via SKIP LOCKED), so re-firing would spawn CONCURRENT executions — the swarm bug.
//   - A FAILED activation leaves lastActivatedAt untouched, so the next pass retries it. No
//     execution was started on a failure, so a retry cannot swarm; it is legitimate recovery.
//   - First-ever activation (no recorded timestamp) fires immediately.
//
// Scope: only on_demand cloudrun providers. Residents (Mac/VPC) already have a poll + poke safety
// net and must NOT be turned into a swarm, so they are skipped. Best-effort throughout: a failure
// is logged, never fatal, and never stalls the reconciler.
func (r *Reconciler) reactivateStalled(ctx context.Context) {
	for name := range r.pendingThisPass {
		if r.activatedThisPass[name] {
			continue // already woken this pass (assignment-time) — one wake drains the queue
		}
		p, found, err := r.db.GetProvider(ctx, name)
		if err != nil {
			log.Printf("reactivate %s: get provider: %v", name, err)
			continue
		}
		// Only on_demand cloudrun has no poll safety net. Residents (and a provider since removed
		// from config) are out of scope — re-firing them risks the very swarm we are avoiding.
		if !found || p.Runtime != runtimeCloudRun || p.Activation != activationOnDemand {
			continue
		}
		if last, seen := r.lastActivatedAt[name]; seen && r.now().Sub(last) < r.reactivateBackoff {
			continue // a recent success is still draining — re-firing now would swarm
		}
		r.activatedThisPass[name] = true
		if err := r.activator.Activate(ctx, p); err != nil {
			log.Printf("reactivate %s: %v", name, err) // failure: do not anchor; retry next pass
			continue
		}
		log.Printf("reactivate %s: re-fired activation for stuck pending work (self-healing)", name)
		r.lastActivatedAt[name] = r.now() // success anchors the anti-stampede backoff
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
//	all steps done                 -> done
//	destilar done                  -> distilled
//	transcrever OR extrair done    -> to_text
//	otherwise (early steps)        -> discovered
//
// The to_text milestone is reached by EITHER to-text capability: `transcrever` (audio lanes:
// youtube, podcast) or `extrair` (text lanes: email). The architecture's lane template is
// `... -> (transcrever | extrair) -> ...`, so both produce the same milestone.
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
	case doneCaps[capTranscrever] || doneCaps[capExtrair]:
		return itemToText
	default:
		return itemDiscovered
	}
}

// hasLaterStep reports whether any item_step with a seq greater than the given one has been
// materialized — i.e. the flow has advanced past that point.
func hasLaterStep(seq int, bySeq map[int]ItemStep) bool {
	for s := range bySeq {
		if s > seq {
			return true
		}
	}
	return false
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

// gateTerminalStatus maps a completed gate's recorded decision onto the item's terminal
// status (deliverable #6: the reconciler routes keep/drop/defer from the gate_decision the
// worker wrote). It scans gate steps in seq order, reading the latest decision of each that
// is done: drop -> filtered, defer -> quarantine, keep -> not terminal. The first
// terminating gate wins (a drop at gate_barato ends the item before gate_rico is reached). A
// done gate with no decision (a data anomaly — the worker writes the decision before marking
// the step done) is treated as keep, so a glitch never silently drops an item.
func (r *Reconciler) gateTerminalStatus(ctx context.Context, item Item, flowSteps []FlowStep, bySeq map[int]ItemStep) (string, bool, error) {
	for _, fs := range flowSteps {
		if !isGate(fs.Capability) {
			continue
		}
		if s, ok := bySeq[fs.Seq]; !ok || s.Status != stepDone {
			continue
		}
		// Already routed `keep` if the flow advanced past this gate (a later step exists) —
		// a drop/defer would have terminated the item before any successor materialized. Skip
		// the decision read for gates we have demonstrably passed, so a long-running item does
		// not re-read every kept gate on every pass.
		if hasLaterStep(fs.Seq, bySeq) {
			continue
		}
		dec, found, err := r.db.LatestGateDecision(ctx, item.ID, fs.Capability)
		if err != nil {
			return "", false, err
		}
		if !found {
			continue
		}
		switch dec.Decision {
		case decisionDrop:
			return itemFiltered, true, nil
		case decisionDefer:
			return itemQuarantine, true, nil
		}
	}
	return "", false, nil
}

// logActivator is the default Activator: it records the wake intent without calling any
// cloud API. The real Cloud Run Jobs `run` client is wired at deploy (it needs
// run.jobs.run on the reconciler's service account); until then this keeps the loop
// runnable and the tests pure.
type logActivator struct{}

func (logActivator) Activate(_ context.Context, p Provider) error {
	log.Printf("activate (noop): would wake on_demand provider %q (runtime=%s)", p.Name, p.Runtime)
	return nil
}
