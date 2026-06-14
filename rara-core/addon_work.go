// addon_work.go — the `work` role, now built on the rara-addon SDK (P1a, bridge-total).
//
// The claim/heartbeat/result/requeue/poke orchestration that used to live in worker.go is gone:
// it was extracted into the rara-addon module so every worker (this one and the future independent
// apps) shares one implementation of the contract instead of reimplementing it. rara-core proves
// the SDK end to end by running its own `work` role through addon.Run.
//
// Two thin adapters bridge rara-core to the SDK:
//
//   - coreStore adapts rara-core's existing Database (the tested pgx persistence) to addon.Store, so
//     the claim SQL keeps running through the same code path the reconciler/surface tests cover;
//   - workHandler adapts a rara-core StepRunner (the domain glue in runners.go) to addon.Handler. It
//     is the one place that knows rara-core domain facts the SDK must not: a gate verdict becomes a
//     gate_decisions row (the worker records the judgement; the reconciler still routes), and the
//     core errRetryable sentinel is mapped to addon.ErrRetryable so the SDK requeues.
//
// Behaviour is unchanged from the old worker.go: same claim isolation, same done/failed/filtered
// outcomes, same gate-decision recording, same retry-to-ceiling. The reconciler and surface are the
// orchestrator and do not change.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	addon "rara-addon"
)

// maxStepAttempts caps how many times a transient (retryable) step is re-queued before it is failed
// for good — mirroring scribe/distill's own per-row attempt ceiling. It tracks the SDK default.
const maxStepAttempts = addon.DefaultMaxAttempts

// coreStore adapts rara-core's Database to addon.Store. The SDK owns the contract tables; this just
// translates between the SDK's minimal types and core's domain types, delegating to the persistence
// the rest of rara-core already uses (so there is one claim implementation, one heartbeat, etc.).
//
// It serializes every operation behind a mutex because rara-core backs the store with a single
// *pgx.Conn (not a pool), which is NOT safe for concurrent use — and in resident mode the SDK calls
// Heartbeat from a background goroutine while the drain loop claims/marks. The mutex makes that
// correct; the ops are short, so contention is negligible (and on_demand mode is single-goroutine).
type coreStore struct {
	mu sync.Mutex
	db Database
}

func newCoreStore(db Database) *coreStore { return &coreStore{db: db} }

var _ addon.Store = (*coreStore)(nil)

func (s *coreStore) Claim(ctx context.Context, capability, provider string) (*addon.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.db.ClaimPendingStep(ctx, capability, provider)
	if err != nil || st == nil {
		return nil, err
	}
	return &addon.Step{
		ItemID: st.ItemID, Seq: st.Seq, Capability: st.Capability,
		AssignedProvider: st.AssignedProvider, Attempt: st.Attempt,
		Status: st.Status, HeartbeatAt: st.HeartbeatAt,
	}, nil
}

func (s *coreStore) Heartbeat(ctx context.Context, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.TouchProviderHeartbeat(ctx, provider)
}

func (s *coreStore) GetItem(ctx context.Context, id int) (addon.Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, found, err := s.db.GetItem(ctx, id)
	if err != nil || !found {
		return addon.Item{}, found, err
	}
	return addon.Item{
		ID: it.ID, Lane: it.Lane, SourceRef: it.SourceRef, Status: it.Status,
		Sensitivity: it.Sensitivity, FlowID: it.FlowID, FlowVersion: it.FlowVersion,
	}, true, nil
}

// Mark writes the step terminal via the full-record upsert, preserving the claim's heartbeat (the
// SDK carries it on the Step) — matching the old worker.finish exactly.
func (s *coreStore) Mark(ctx context.Context, step addon.Step, status, outputRef, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.UpsertItemStep(ctx, s.toItemStep(step, status, outputRef, errMsg))
}

// Requeue returns a transiently-failed step to the pending frontier with the heartbeat cleared (so
// it reads as un-owned) — matching the old worker.requeue exactly.
func (s *coreStore) Requeue(ctx context.Context, step addon.Step, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.toItemStep(step, stepPending, "", errMsg)
	st.HeartbeatAt = nil
	return s.db.UpsertItemStep(ctx, st)
}

func (s *coreStore) FilterItem(ctx context.Context, item addon.Item) error {
	if item.Status == itemFiltered {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, found, err := s.db.GetItem(ctx, item.ID)
	if err != nil || !found {
		return err
	}
	it.Status = itemFiltered
	_, err = s.db.UpsertItem(ctx, it)
	return err
}

// toItemStep reconstitutes a full item_step row from the claimed Step (the upsert is full-record, so
// it must carry capability / assigned_provider / attempt the claim stamped). Callers hold s.mu.
func (s *coreStore) toItemStep(step addon.Step, status, outputRef, errMsg string) ItemStep {
	return ItemStep{
		ItemID: step.ItemID, Seq: step.Seq, Capability: step.Capability,
		Status: status, AssignedProvider: step.AssignedProvider, Attempt: step.Attempt,
		HeartbeatAt: step.HeartbeatAt, OutputRef: outputRef, Error: errMsg,
	}
}

// workHandler adapts a rara-core StepRunner to an addon.Handler. It carries the only domain
// knowledge the SDK must not have: recording a gate verdict as a gate_decisions row, and mapping the
// core errRetryable sentinel onto addon.ErrRetryable.
func workHandler(db Database, capability string, runner StepRunner) addon.Handler {
	return func(ctx context.Context, item addon.Item, step addon.Step) (addon.Result, error) {
		coreItem := Item{
			ID: item.ID, Lane: item.Lane, SourceRef: item.SourceRef, Status: item.Status,
			Sensitivity: item.Sensitivity, FlowID: item.FlowID, FlowVersion: item.FlowVersion,
		}
		coreStep := ItemStep{
			ItemID: step.ItemID, Seq: step.Seq, Capability: step.Capability,
			Status: step.Status, AssignedProvider: step.AssignedProvider, Attempt: step.Attempt,
			HeartbeatAt: step.HeartbeatAt,
		}

		res, err := runner.Run(ctx, coreItem, coreStep)
		if err != nil {
			// Translate the core transient sentinel so the SDK requeues instead of failing.
			if errors.Is(err, errRetryable) {
				return addon.Result{}, fmt.Errorf("%w: %v", addon.ErrRetryable, err)
			}
			return addon.Result{}, err
		}

		if res.Gate != nil {
			// A curation gate judged the item. Record the decision (the audit + training substrate);
			// the gate step is legitimately done. The handler does NOT route the item — the
			// reconciler reads this decision next pass and routes keep/drop/defer. capability is the
			// gate name (capGateBarato == gateBarato, capGateRico == gateRico).
			if err := db.InsertGateDecision(ctx, GateDecision{
				ItemID: item.ID, Gate: capability, Decision: res.Gate.Decision,
				Score: res.Gate.Score, Rank: res.Gate.Rank,
				DecidedBy: res.Gate.DecidedBy, Reason: res.Gate.Reason,
			}); err != nil {
				return addon.Result{}, err
			}
			return addon.Result{OutputRef: res.OutputRef}, nil
		}
		return addon.Result{OutputRef: res.OutputRef, Filtered: res.Filtered}, nil
	}
}

// runWork runs a (capability, provider) pull loop on the rara-addon SDK. A worker serves exactly one
// provider so it claims only the steps the reconciler routed to it — required once a capability has
// several providers with different runners (transcrever -> asr-youtube on the Mac vs asr-direct-audio
// on Cloud Run).
//
// Default behaviour matches the old on_demand worker: drain the queue once and exit (the woken Cloud
// Run job). A resident deploy opts into the long-running loop + symmetric activation by setting
// WORK_POLL_INTERVAL (the safety-net poll) and/or POKE_ADDR + POKE_TOKEN (the tailnet poke listener).
func runWork(ctx context.Context, db Database, conn *pgx.Conn, argv []string) {
	fs := flag.NewFlagSet("work", flag.ExitOnError)
	capability := fs.String("capability", "", "capability to serve: transcrever | extrair | gate_barato | gate_rico")
	provider := fs.String("provider", "", "the provider this worker serves (the reconciler assigns steps to it)")
	_ = fs.Parse(argv)
	if *provider == "" {
		log.Fatalf("work: --provider is required (a capability may have several providers with different runners)")
	}

	runner := selectRunner(db, conn, *capability, *provider)
	cfg := addon.Config{
		Capability:   *capability,
		Provider:     *provider,
		Store:        newCoreStore(db),
		MaxAttempts:  maxStepAttempts,
		PollInterval: envDuration("WORK_POLL_INTERVAL", 0),
		PokeAddr:     os.Getenv("POKE_ADDR"),
		PokeToken:    os.Getenv("POKE_TOKEN"),
	}
	if err := addon.Run(ctx, cfg, workHandler(db, *capability, runner)); err != nil {
		log.Fatalf("work %s/%s: %v", *capability, *provider, err)
	}
	log.Printf("rara-core worker %s/%s: queue drained", *capability, *provider)
}

// envDuration reads a Go duration (e.g. "30s", "2m") from env, or returns def when unset/invalid.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 { // bare integer => seconds
		return time.Duration(n) * time.Second
	}
	log.Printf("work: ignoring invalid %s=%q", key, v)
	return def
}

// selectRunner maps a (capability, provider) pair to its StepRunner shim. transcrever has two
// providers with different entrypoints (asr-youtube builds a watch URL; asr-direct-audio reads a
// direct enclosure URL), so the provider — not just the capability — selects the runner.
func selectRunner(db Database, conn *pgx.Conn, capability, provider string) StepRunner {
	switch capability {
	case capTranscrever:
		switch provider {
		case provASRYouTube:
			return newScribeRunner(conn)
		case provASRDirectAudio:
			return newASRDirectAudioRunner(conn)
		default:
			log.Fatalf("work transcrever: unknown provider %q", provider)
		}
	case capExtrair:
		// extrair has a provider per text lane (email vs linkedin); each reads a different domain
		// table, so the provider — not just the capability — selects the runner.
		switch provider {
		case provExtrairEmail:
			return newExtractRunner(conn)
		case provExtrairLinked:
			return newLinkedInExtractRunner(conn)
		default:
			log.Fatalf("work extrair: unknown provider %q", provider)
		}
	// destilar is intentionally absent: it is its own app (rara-distill) on the SDK, not a runner
	// the core work role serves. The reconciler still routes/activates it; the core never runs it.
	case capGateBarato, capGateRico:
		judge, err := newLiteLLMJudge()
		if err != nil {
			log.Fatalf("work %s: %v", capability, err)
		}
		return newGateRunner(db, conn, capability, judge)
	default:
		log.Fatalf("work: --capability must be one of transcrever|extrair|gate_barato|gate_rico, got %q", capability)
	}
	return nil // unreachable: every branch above either returns or log.Fatalf-exits
}
