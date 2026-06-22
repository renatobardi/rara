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
//	transcrever -> assign      (pending + provider; wait for the scribe app, rara-transcribe)
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

// Reconciler is the control loop. Its only job is to observe desired state (flows + items) and
// persist assignments (item_steps with an assigned_provider). It does NOT wake providers — that
// belongs to the rara-runner dispatch loop (F3). Coupling = the table.
//
// now, staleAfter, healthTTL are injectable for deterministic tests.
type Reconciler struct {
	db     Database
	router *Router
	now    func() time.Time

	staleAfter time.Duration
	healthTTL  time.Duration

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

// NewReconciler wires a reconciler. now, staleAfter and healthTTL default to wall-clock,
// defaultStaleAfter and defaultHealthTTL; tests overwrite them directly.
// The Runner parameter has been removed in F3: activation belongs to the rara-runner
// dispatch loop — the reconciler only persists assignments.
func NewReconciler(db Database) *Reconciler {
	return &Reconciler{
		db: db, router: NewRouter(db),
		now: time.Now, staleAfter: defaultStaleAfter, healthTTL: defaultHealthTTL,
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
			if st.Status == stepRunning && r.isStale(st) {
				if err := r.handleStaleStep(ctx, item, fs, st); err != nil {
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
			// stepPending with no provider means the previous pass found no eligible
			// provider and left the step unassigned; workers require assigned_provider
			// to claim, so re-route immediately rather than waiting indefinitely.
			if s.Status == stepPending && s.AssignedProvider == "" {
				return actMaterialize, fs
			}
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
		// for the worker to pull. A per-step providers list in flow_steps.options overrides
		// the policy fallback for this step.
		prov, ok, err := r.router.SelectForStep(ctx, fs.Capability, item, r.now(), r.healthTTL, stepFallbackFromOptions(fs.Options))
		if err != nil {
			return false, fmt.Errorf("select provider for item %d seq %d: %w", item.ID, fs.Seq, err)
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
			return false, fmt.Errorf("upsert item step for item %d seq %d: %w", item.ID, fs.Seq, err)
		}
		return false, nil
	}
}

// handleStaleStep recovers a running step whose worker has gone silent (heartbeat older
// than staleAfter -> the worker likely died). It asks the router for the next eligible provider
// EXCLUDING the dead one. If an alternative exists the step fails over to it; if none does,
// the step is re-queued on the same provider to be re-claimed should it revive. Either way the
// step returns to the pending frontier (heartbeat cleared). Waking the new provider is the
// dispatcher's responsibility — the reconciler only updates the assignment row.
func (r *Reconciler) handleStaleStep(ctx context.Context, item Item, fs FlowStep, st ItemStep) error {
	dead := st.AssignedProvider
	next, ok, err := r.router.SelectForStep(ctx, st.Capability, item, r.now(), r.healthTTL, stepFallbackFromOptions(fs.Options), dead)
	if err != nil {
		return fmt.Errorf("select fallback for stale item %d seq %d: %w", item.ID, st.Seq, err)
	}

	var chosen Provider
	if ok {
		log.Printf("reconcile item %d seq %d: stale heartbeat, failing over %s -> %s", item.ID, st.Seq, dead, next.Name)
		chosen = next
	} else {
		// No alternative provider: re-queue on the same one (it may come back). If it vanished
		// from config, re-queue unassigned so a later pass routes the step afresh.
		log.Printf("reconcile item %d seq %d: stale heartbeat, no fallback, re-queuing %s", item.ID, st.Seq, dead)
		p, found, gerr := r.db.GetProvider(ctx, dead)
		if gerr != nil {
			return fmt.Errorf("get stale provider %q: %w", dead, gerr)
		}
		if found {
			chosen = p
		}
		// chosen stays zero-value if not found: empty name -> NULL assigned_provider; re-route next pass
	}

	st.Status = stepPending
	st.HeartbeatAt = nil
	st.AssignedProvider = chosen.Name
	if err := r.db.UpsertItemStep(ctx, st); err != nil {
		return fmt.Errorf("requeue stale item %d seq %d: %w", item.ID, st.Seq, err)
	}
	return nil
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
