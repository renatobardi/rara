// dispatch.go — the perennial VPC service that reads desired state from Neon and wakes providers.
// The reconciler (rara-core) persists assignments: item_steps WHERE status='pending' AND
// assigned_provider IS NOT NULL. This loop reads those rows, coalesces by provider (one wake per
// pass per provider), and calls Runner.Run — the coupling is the table, never a direct call.
package main

import (
	"context"
	"log"
)

// bgCtx is the context used for best-effort observability writes (StampDispatchError /
// ClearDispatchError). These writes must not be cancelled by the pass context — if the
// pass ctx is cancelled (shutdown signal, timeout), we still want the error recorded.
var bgCtx = context.Background()

// AssignedStep is the minimal projection of item_steps the dispatcher needs: who is assigned and
// to which provider. The provider's own poll loop handles item discovery; the dispatcher only wakes.
type AssignedStep struct {
	ItemID           int
	Seq              int
	Capability       string
	AssignedProvider string
}

// DispatchProvider carries the routing fields the dispatcher needs to build a RunRequest.
type DispatchProvider struct {
	Name       string
	Runtime    string
	Activation string
	RunnerURL  string            // rara-runner agent tailnet URL; empty for Cloud Run providers
	Env        map[string]string // per-run config injected at wake (Cloud Run overrides / docker -e); may carry secrets — never log values, only len
}

// DispatchDB is the storage contract the dispatcher reads. It is the minimal subset of the full
// rara-core Database that the dispatcher needs — a separate interface keeps rara-runner independent
// of rara-core's full schema (no shared module).
type DispatchDB interface {
	ListAssignedSteps(ctx context.Context) ([]AssignedStep, error)
	GetProvider(ctx context.Context, name string) (DispatchProvider, bool, error)
	// ListDueCollectors returns enabled collector providers whose collect_cadence_seconds has
	// elapsed since last_collect_at (or have never succeeded), AND whose retry_interval_seconds
	// has elapsed since last_attempt_at (or have never been attempted). The two conditions are
	// independent: cadence gates how often a healthy collector runs; retry throttles re-wakes of
	// a failing one.
	ListDueCollectors(ctx context.Context) ([]DispatchProvider, error)
	// TouchCollectorAttempted stamps last_attempt_at = now() before each wake. Called regardless
	// of whether the runner succeeds — the collector itself stamps last_collect_at on success.
	TouchCollectorAttempted(ctx context.Context, name string) error
	// StampDispatchError records a wake failure in providers.last_error (capped to
	// maxDispatchErrorRunes runes). Called best-effort on runner.Run failure.
	StampDispatchError(ctx context.Context, name, msg string) error
	// ClearDispatchError sets providers.last_error = NULL on a successful wake so stale
	// failure messages do not persist after a placement recovers.
	ClearDispatchError(ctx context.Context, name string) error
}

// Dispatcher reads desired state from the DB and wakes providers. One wake per provider per pass
// (coalesced): a single Cloud Run `run` drains the whole queue for that provider, so fan-out is
// wasteful and can swarm scale-to-zero jobs.
type Dispatcher struct {
	db     DispatchDB
	runner Runner
}

// DispatchOnce performs a single pass: wake assigned workers (coalesced by provider) and any
// collector providers whose cadence has elapsed. Runner errors are best-effort (logged, not
// returned): one failed wake must not prevent the rest.
func (d *Dispatcher) DispatchOnce(ctx context.Context) error {
	steps, err := d.db.ListAssignedSteps(ctx)
	if err != nil {
		return err
	}

	// Collect unique provider names first (coalesce) to avoid N GetProvider calls for M steps on
	// the same provider.
	seen := make(map[string]bool, len(steps))
	for _, s := range steps {
		seen[s.AssignedProvider] = true
	}

	for name := range seen {
		prov, ok, err := d.db.GetProvider(ctx, name)
		if err != nil {
			log.Printf("dispatch: get provider %q: %v", name, err)
			continue
		}
		if !ok {
			log.Printf("dispatch: provider %q not found; skipping", name)
			continue
		}
		if err := d.runner.Run(ctx, buildRunRequest(prov)); err != nil {
			log.Printf("dispatch: wake %q: %v", name, err)
			// Use bgCtx: pass ctx may be cancelled at shutdown; the error stamp must still land.
			// runner.Run errors come from HTTP status codes / net errors — no credential values.
			if serr := d.db.StampDispatchError(bgCtx, name, err.Error()); serr != nil {
				log.Printf("dispatch: stamp error %q: %v", name, serr) // best-effort
			}
		} else {
			if cerr := d.db.ClearDispatchError(bgCtx, name); cerr != nil {
				log.Printf("dispatch: clear error %q: %v", name, cerr) // best-effort
			}
		}
	}

	// Dispatch collectors whose cadence has elapsed. Unlike workers (which are woken by
	// pending item_steps), collectors create items and have no item_step to pull — the
	// dispatcher stamps last_attempt_at before each wake; the collector itself stamps
	// last_collect_at on success.
	collectors, err := d.db.ListDueCollectors(ctx)
	if err != nil {
		return err
	}
	for _, prov := range collectors {
		if err := d.db.TouchCollectorAttempted(ctx, prov.Name); err != nil {
			log.Printf("dispatch: attempt stamp %q: %v — skipping wake (throttle protection)", prov.Name, err)
			continue // don't wake without stamping — throttle would be bypassed
		}
		if err := d.runner.Run(ctx, buildRunRequest(prov)); err != nil {
			log.Printf("dispatch: wake collector %q: %v", prov.Name, err)
			// Use bgCtx: same rationale as the worker loop above.
			if serr := d.db.StampDispatchError(bgCtx, prov.Name, err.Error()); serr != nil {
				log.Printf("dispatch: stamp error collector %q: %v", prov.Name, serr) // best-effort
			}
		} else {
			if cerr := d.db.ClearDispatchError(bgCtx, prov.Name); cerr != nil {
				log.Printf("dispatch: clear error collector %q: %v", prov.Name, cerr) // best-effort
			}
		}
	}
	return nil
}

// buildRunRequest constructs a RunRequest from a DispatchProvider, copying Env so the
// RunRequest owns it and downstream mutations cannot bleed back into the provider map.
func buildRunRequest(prov DispatchProvider) RunRequest {
	var env map[string]string
	if len(prov.Env) > 0 {
		env = make(map[string]string, len(prov.Env))
		for k, v := range prov.Env {
			env[k] = v
		}
	}
	return RunRequest{
		App:        prov.Name,
		Runtime:    prov.Runtime,
		Activation: prov.Activation,
		RunnerURL:  prov.RunnerURL,
		Env:        env,
	}
}
