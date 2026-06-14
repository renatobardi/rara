// Package addon is the shared SDK for rara worker/agent "addons" — sovereign processes that
// attach to rara-core ONLY through the Neon contract: a providers row (registration) plus the
// item_steps protocol (work). Nothing else. Each addon has its own process, deploy and version
// and can be written in any language; this module is the Go batteries-included implementation so
// a worker writes only its domain logic and never reimplements the queue.
//
// The wire protocol the SDK owns:
//
//   - claim     — atomically pull the frontmost pending step assigned to this provider
//     (SELECT ... FOR UPDATE SKIP LOCKED, pending->running, attempt++, heartbeat);
//   - heartbeat — stamp providers.heartbeat_at so the router's health gate keeps routing here
//     (per-claim proof of life + a periodic tick for residents);
//   - result    — write the step terminal (done with output_ref, or failed with the error);
//   - requeue   — return a transiently-failed step to the pending frontier, up to MaxAttempts;
//   - poke      — a tailnet HTTP listener that drains the queue on demand (symmetric activation),
//     with the slow poll as the safety net.
//
// The claim is by **(capability, assigned_provider)**, never capability alone. With several
// providers per capability (the *-local vs third-party split), filtering on the assigned provider
// is what keeps a PRIVATE item routed to, say, distill-local from being pulled by a third-party
// worker. That provider-to-provider isolation is the contract's whole point; it is enforced in
// Store.Claim and exercised by the SDK tests.
//
// The SDK owns only the CONTRACT tables (item_steps, providers) and reads the items spine; the
// DOMAIN tables (transcripts, distillations, gate_decisions, ...) belong to the worker, which the
// SDK never touches — it reaches persistence only through the Store the caller passes.
package addon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// DefaultMaxAttempts caps how many times a transient (retryable) step is re-queued before it is
// failed for good. Untyped so callers can use it in const expressions.
const DefaultMaxAttempts = 5

// defaultHeartbeatInterval is the resident heartbeat cadence when neither HeartbeatInterval nor a
// derivable PollInterval is set.
const defaultHeartbeatInterval = 30 * time.Second

// Step status values written back on the item_steps contract table.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// ErrRetryable marks a TRANSIENT miss: the work is not done but a later attempt may succeed
// (e.g. a batch worker hasn't produced this row yet). A handler returns an error wrapping it to
// request a requeue (up to MaxAttempts) instead of a terminal failure. Any other error fails the
// step for good.
var ErrRetryable = errors.New("addon: retryable: output not yet available")

// Item is the minimal view of a spine item the SDK reads and a handler needs. It is NOT the
// worker's domain row — the handler reads that itself, keyed by SourceRef.
type Item struct {
	ID          int
	Lane        string
	SourceRef   string
	Status      string
	Sensitivity string // public | private; the worker may need it to pick a model/host
	FlowID      int
	FlowVersion int
}

// Step is one claimed item_step (a contract-table row). The SDK fills it from the claim and hands
// it to the handler; the handler treats it as read-only context. HeartbeatAt carries the value the
// claim stamped so a Store can preserve it on a full-record write.
type Step struct {
	ItemID           int
	Seq              int
	Capability       string
	AssignedProvider string
	Attempt          int
	Status           string
	HeartbeatAt      *time.Time
}

// Result is what a handler reports for a successful run. OutputRef is the worker-owned domain row
// id to record on the step (the logical cross-table link). Filtered marks a benign, no-content
// outcome (e.g. an empty transcript): the step is legitimately done, but there is nothing to carry
// downstream, so the SDK curates the item out (terminal `filtered`) instead of letting it march
// into a step that must fail.
type Result struct {
	OutputRef string
	Filtered  bool
}

// Handler is the domain logic: given the item and the claimed step, do the work and report the
// result. Return an error wrapping ErrRetryable for a transient miss; any other error fails the
// step. The SDK does claim/heartbeat/result/requeue/poke around it.
type Handler func(ctx context.Context, item Item, step Step) (Result, error)

// Config configures one (capability, provider) worker. Store is the persistence seam; the rest
// tune the loop and the symmetric-activation listener.
type Config struct {
	Capability string // logical task (transcrever, destilar, gate_barato, ...)
	Provider   string // the concrete provider this worker serves (the reconciler assigns to it)
	Store      Store  // contract-table operations + item read (pgx impl or a fake)

	// PollInterval drives the resident safety-net loop. Zero means on_demand: Run drains the queue
	// once and returns (the woken Cloud Run job pattern). Positive means resident: Run drains, then
	// re-drains on each tick (and on each poke) until ctx is cancelled.
	PollInterval time.Duration

	// MaxAttempts is the retry ceiling for ErrRetryable; <= 0 uses DefaultMaxAttempts.
	MaxAttempts int

	// HeartbeatInterval is the resident periodic-heartbeat cadence (proof of life while idle). <= 0
	// derives it from PollInterval (or defaultHeartbeatInterval). Ignored in on_demand mode, where
	// the per-claim heartbeat suffices (on_demand providers are exempt from liveness).
	HeartbeatInterval time.Duration

	// PokeAddr, if set, mounts a minimal HTTP listener (tailnet only) that drains the queue on
	// POST /poke. PokeToken authenticates it (Bearer, constant-time, fail-closed). A listener makes
	// the worker resident even when PollInterval is zero.
	PokeAddr  string
	PokeToken string
}

func (c Config) validate(h Handler) error {
	switch {
	case c.Capability == "":
		return errors.New("addon: Config.Capability is required")
	case c.Provider == "":
		return errors.New("addon: Config.Provider is required")
	case c.Store == nil:
		return errors.New("addon: Config.Store is required")
	case h == nil:
		return errors.New("addon: handler is required")
	}
	return nil
}

// worker holds the resolved config + handler for one pull loop.
type worker struct {
	cfg Config
	h   Handler
}

// Run is the SDK entrypoint. It drains the worker's queue immediately (so a woken on_demand job
// does its work right away), then — if the worker is resident (PollInterval > 0 or a poke listener
// is mounted) — keeps draining on each poll tick and each poke until ctx is cancelled. It returns
// nil on a clean on_demand drain, or ctx.Err()/the first store error otherwise.
func Run(ctx context.Context, cfg Config, h Handler) error {
	if err := cfg.validate(h); err != nil {
		return err
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	w := &worker{cfg: cfg, h: h}

	resident := cfg.PollInterval > 0 || cfg.PokeAddr != ""

	// Symmetric activation: a poke drains on demand; the buffered channel coalesces bursts (one
	// pending drain is enough — the drain loop empties whatever accumulated).
	var pokeCh chan struct{}
	if cfg.PokeAddr != "" {
		pokeCh = make(chan struct{}, 1)
		srv, _, err := startPokeListener(cfg.PokeAddr, cfg.PokeToken, pokeCh)
		if err != nil {
			return fmt.Errorf("addon: poke listener: %w", err)
		}
		// Shut the listener down on EVERY Run exit — clean ctx cancel, a drain error, or a poll
		// error — not just on ctx.Done. A deferred shutdown (Run blocks until return in resident
		// mode) avoids leaking the goroutine + socket if Run returns early on an error.
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	// Residents announce liveness even while idle so the router's health gate keeps them eligible.
	if resident {
		go w.heartbeatLoop(ctx, cfg.heartbeatInterval())
	}

	// Drain once up front (on_demand does its work and returns here).
	if err := w.drain(ctx); err != nil {
		return err
	}
	if !resident {
		return nil
	}

	var pollC <-chan time.Time
	if cfg.PollInterval > 0 {
		t := time.NewTicker(cfg.PollInterval)
		defer t.Stop()
		pollC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pollC: // safety net (nil when PollInterval == 0: a poke-only resident)
			if err := w.drain(ctx); err != nil {
				return err
			}
		case <-pokeCh:
			if err := w.drain(ctx); err != nil {
				return err
			}
		}
	}
}

// heartbeatInterval resolves the resident heartbeat cadence: explicit, else the poll interval,
// else the default.
func (c Config) heartbeatInterval() time.Duration {
	switch {
	case c.HeartbeatInterval > 0:
		return c.HeartbeatInterval
	case c.PollInterval > 0:
		return c.PollInterval
	default:
		return defaultHeartbeatInterval
	}
}

// heartbeatLoop stamps the provider's heartbeat on a fixed cadence so an idle resident still reads
// as alive. Best-effort: a failed stamp is logged, never fatal.
func (w *worker) heartbeatLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.cfg.Store.Heartbeat(ctx, w.cfg.Provider); err != nil {
				log.Printf("addon %s/%s: periodic heartbeat: %v", w.cfg.Capability, w.cfg.Provider, err)
			}
		}
	}
}

// drain claims and processes steps until the queue is empty (or ctx is done / a store error).
func (w *worker) drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := w.runOnce(ctx)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
	}
}

// runOnce claims and processes a single step. claimed=false means the queue was empty.
func (w *worker) runOnce(ctx context.Context) (claimed bool, err error) {
	step, err := w.cfg.Store.Claim(ctx, w.cfg.Capability, w.cfg.Provider)
	if err != nil {
		return false, err
	}
	if step == nil {
		return false, nil // nothing pending for this (capability, provider)
	}

	// Proof of life: this worker just pulled work as assigned_provider, so stamp its heartbeat.
	// Best-effort — a heartbeat write must never block processing the claimed step.
	if step.AssignedProvider != "" {
		if err := w.cfg.Store.Heartbeat(ctx, step.AssignedProvider); err != nil {
			log.Printf("addon %s/%s: heartbeat %s: %v", w.cfg.Capability, w.cfg.Provider, step.AssignedProvider, err)
		}
	}

	item, found, err := w.cfg.Store.GetItem(ctx, step.ItemID)
	if err != nil {
		return true, err
	}
	if !found {
		// The item vanished (cascade delete?) between claim and read. Fail the orphan step so it
		// leaves the running set.
		return true, w.cfg.Store.Mark(ctx, *step, StatusFailed, "", "item not found")
	}

	res, runErr := w.h(ctx, item, *step)
	if runErr != nil {
		if errors.Is(runErr, ErrRetryable) && step.Attempt < w.cfg.MaxAttempts {
			log.Printf("addon %s/%s: step item=%d seq=%d transient (attempt %d/%d): %v",
				w.cfg.Capability, w.cfg.Provider, step.ItemID, step.Seq, step.Attempt, w.cfg.MaxAttempts, runErr)
			return true, w.cfg.Store.Requeue(ctx, *step, runErr.Error())
		}
		log.Printf("addon %s/%s: step item=%d seq=%d failed: %v",
			w.cfg.Capability, w.cfg.Provider, step.ItemID, step.Seq, runErr)
		return true, w.cfg.Store.Mark(ctx, *step, StatusFailed, "", runErr.Error())
	}

	if res.Filtered {
		// Benign no-content: record the step done with its output, then curate the item out. This
		// is the one sanctioned case where the worker side writes item status — a terminal hand-off
		// (the item leaves the active set; the reconciler never contends).
		if err := w.cfg.Store.Mark(ctx, *step, StatusDone, res.OutputRef, ""); err != nil {
			return true, err
		}
		return true, w.cfg.Store.FilterItem(ctx, item)
	}
	return true, w.cfg.Store.Mark(ctx, *step, StatusDone, res.OutputRef, "")
}
