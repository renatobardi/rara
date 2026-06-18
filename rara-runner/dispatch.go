// dispatch.go — the perennial VPC service that reads desired state from Neon and wakes providers.
// The reconciler (rara-core) persists assignments: item_steps WHERE status='pending' AND
// assigned_provider IS NOT NULL. This loop reads those rows, coalesces by provider (one wake per
// pass per provider), and calls Runner.Run — the coupling is the table, never a direct call.
package main

import (
	"context"
	"log"
)

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
}

// Dispatcher reads desired state from the DB and wakes providers. One wake per provider per pass
// (coalesced): a single Cloud Run `run` drains the whole queue for that provider, so fan-out is
// wasteful and can swarm scale-to-zero jobs.
type Dispatcher struct {
	db     DispatchDB
	runner Runner
}

// DispatchOnce performs a single pass: list assigned steps, coalesce by provider, wake each once.
// Runner errors are best-effort (logged, not returned): one failed wake must not prevent the rest.
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
		// Copy Env so the RunRequest owns it — prov is per-call, but the copy keeps the wake's
		// config independent of any downstream mutation.
		var env map[string]string
		if len(prov.Env) > 0 {
			env = make(map[string]string, len(prov.Env))
			for k, v := range prov.Env {
				env[k] = v
			}
		}
		req := RunRequest{
			App:        prov.Name,
			Runtime:    prov.Runtime,
			Activation: prov.Activation,
			RunnerURL:  prov.RunnerURL,
			Env:        env,
		}
		if err := d.runner.Run(ctx, req); err != nil {
			log.Printf("dispatch: wake %q: %v", name, err) // best-effort
		}
	}
	return nil
}
