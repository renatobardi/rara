// rara-core is the 2.0 orchestrated control plane: a new, isolated agent that owns the
// control tables (capabilities, providers, flows, flow_steps, routing_policies, items,
// item_steps, gate_decisions, feedback, interest_profile) in the shared Neon database.
//
// Phase 0 (this file): scaffold only. It defines the domain types, the persistence seam
// (idempotent upserts mirroring the SQL ON CONFLICT / FK / CHECK contract) and the pgx
// implementation — but NO reconciler, NO router, NO gate logic. main() connects, reports
// that the control tables are reachable, and exits. The always-on reconciler loop, the
// policy-driven router and the curation gates land in later phases (see ARCHITECTURE-2.0.md).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Enumerations — kept in sync with the SQL CHECK constraints in
// migrations/001_initial_schema.sql. The validators below are the single source the
// pgx layer and the in-memory mock both use to fail fast, so a value the database
// would reject never reaches it.
// ---------------------------------------------------------------------------

// providers.runtime
const (
	runtimeLocal    = "local"
	runtimeCloudRun = "cloudrun"
	runtimeVPC      = "vpc"
)

// providers.activation
const (
	activationResident = "resident"
	activationOnDemand = "on_demand"
)

// items.status
const (
	itemDiscovered = "discovered"
	itemToText     = "to_text"
	itemDistilled  = "distilled"
	itemDone       = "done"
	itemFiltered   = "filtered"
	itemQuarantine = "quarantine"
	itemFailed     = "failed"
)

// item_steps.status
const (
	stepPending  = "pending"
	stepAssigned = "assigned"
	stepRunning  = "running"
	stepDone     = "done"
	stepFailed   = "failed"
	stepSkipped  = "skipped"
)

// gate_decisions.gate
const (
	gateBarato = "gate_barato"
	gateRico   = "gate_rico"
)

// gate_decisions.decision
const (
	decisionKeep  = "keep"
	decisionDrop  = "drop"
	decisionDefer = "defer"
)

// feedback.target_type
const (
	targetItem         = "item"
	targetDistillation = "distillation"
)

// The fixed capability catalog (mirrors the seed in 001_initial_schema.sql).
const (
	capColetar     = "coletar"
	capTranscrever = "transcrever"
	capExtrair     = "extrair"
	capGateBarato  = "gate_barato"
	capGateRico    = "gate_rico"
	capDestilar    = "destilar"
)

// policyScopeGlobal is the routing_policies.scope sentinel for the catch-all policy the
// router uses when no capability-scoped policy exists.
const policyScopeGlobal = "global"

// constraintResidential is the only hard-constraint requirement the router understands
// today (providers.constraints -> {"requires":"residential"}): egress from a residential
// IP, satisfied only by runtime=local. YouTube blocks audio download from datacenter IPs,
// so asr-youtube carries it and the router eliminates any cloudrun/vpc candidate.
const constraintResidential = "residential"

func isValidRuntime(s string) bool {
	switch s {
	case runtimeLocal, runtimeCloudRun, runtimeVPC:
		return true
	}
	return false
}

func isValidActivation(s string) bool {
	switch s {
	case activationResident, activationOnDemand:
		return true
	}
	return false
}

func isValidItemStatus(s string) bool {
	switch s {
	case itemDiscovered, itemToText, itemDistilled, itemDone, itemFiltered, itemQuarantine, itemFailed:
		return true
	}
	return false
}

func isValidStepStatus(s string) bool {
	switch s {
	case stepPending, stepAssigned, stepRunning, stepDone, stepFailed, stepSkipped:
		return true
	}
	return false
}

func isValidGate(s string) bool { return s == gateBarato || s == gateRico }
func isValidDecision(s string) bool {
	return s == decisionKeep || s == decisionDrop || s == decisionDefer
}
func isValidTargetType(s string) bool { return s == targetItem || s == targetDistillation }

// ---------------------------------------------------------------------------
// Domain types — one struct per control table. JSONB columns are carried as
// json.RawMessage so the control plane stays agnostic about their inner shape (it is
// config, validated by the workers that consume it, not by rara-core).
// ---------------------------------------------------------------------------

// Capability is a logical task with a fixed I/O contract.
type Capability struct {
	Name        string
	Description string
	IOContract  json.RawMessage // "" => defaults to '{}' on write
}

// Provider is a concrete implementation of a capability.
type Provider struct {
	Name        string
	Capability  string // must reference an existing capability (FK)
	Runtime     string // local | cloudrun | vpc
	Activation  string // resident | on_demand
	Cost        float64
	Quality     float64 // 0..1
	LatencyMs   int
	Constraints json.RawMessage // "" => '{}'
	Enabled     bool
	HeartbeatAt *time.Time
}

// Flow is one declarative pipeline per source lane.
type Flow struct {
	ID         int
	Name       string
	SourceType string
	Enabled    bool
	Version    int
}

// FlowStep is one ordered step of a flow.
type FlowStep struct {
	FlowID     int
	Seq        int
	Capability string // FK to capabilities.name
	Options    json.RawMessage
	Enabled    bool
}

// RoutingPolicy is a cost<->quality weighting + ordered fallback.
type RoutingPolicy struct {
	Scope         string // 'global' or a capability name
	CostWeight    float64
	QualityWeight float64
	Fallback      json.RawMessage // ordered list of provider names
}

// Item is one row of the canonical spine.
type Item struct {
	ID          int
	Lane        string
	SourceRef   string
	FlowID      int
	FlowVersion int
	Status      string
}

// ItemStep is one mutable runtime state-row.
type ItemStep struct {
	ItemID           int
	Seq              int
	Capability       string
	Status           string
	AssignedProvider string // "" => NULL (unassigned)
	Attempt          int
	HeartbeatAt      *time.Time
	OutputRef        string // "" => NULL; logical link to a worker domain row
	Error            string
}

// GateDecision is one append-only curation-gate audit row.
type GateDecision struct {
	ItemID    int
	Gate      string
	Decision  string
	Score     *float64 // confidence in [0,1]; nil for the rules layer (which needs none)
	Rank      *int     // gate_rico ordering (1 = top); nil outside gate_rico / when unranked
	DecidedBy string
	Reason    string
}

// Feedback is one append-only learning signal.
type Feedback struct {
	TargetType string
	TargetRef  string
	Signal     string
	Source     string
}

// InterestProfile is one immutable version of the living preferences document.
type InterestProfile struct {
	Version    int
	Topics     json.RawMessage
	Authors    json.RawMessage
	AntiTopics json.RawMessage
	Weights    json.RawMessage
}

// ---------------------------------------------------------------------------
// Persistence seam
//
// Database is the only seam the control plane talks to. The real implementation talks
// to Neon via pgx; the tests use an in-memory mock that mirrors the SQL contract
// (UNIQUE keys, FK references, CHECK enums). All writes are idempotent upserts on the
// config + spine tables (ON CONFLICT), matching the 1.0 re-runnable convention; the
// three audit tables are append-only inserts.
//
// No method here makes a routing or scheduling decision — that is deferred to the
// reconciler in a later phase. This seam is pure storage.
//
// Upsert contract: the Upsert* methods are FULL-RECORD upserts — on conflict every
// non-key column is overwritten with the value from the passed struct, including
// zero-values (an empty IOContract/Constraints writes the SQL default '{}'/'[]', an
// empty Description writes NULL). Always pass the complete intended row; never a
// partial patch, or you will clobber existing columns. (The SQL seed in 001 uses
// ON CONFLICT DO NOTHING precisely so re-applying the migration never clobbers.)
// ---------------------------------------------------------------------------
type Database interface {
	// Config (idempotent, full-record upserts).
	UpsertCapability(ctx context.Context, c Capability) error
	UpsertProvider(ctx context.Context, p Provider) error
	UpsertFlow(ctx context.Context, f Flow) (int, error)
	UpsertFlowStep(ctx context.Context, s FlowStep) error
	UpsertRoutingPolicy(ctx context.Context, p RoutingPolicy) error

	// Spine (idempotent upserts).
	UpsertItem(ctx context.Context, it Item) (int, error)
	UpsertItemStep(ctx context.Context, s ItemStep) error

	// DiscoverItem is the ingest's idempotent upsert: it inserts a newly discovered
	// item (with the passed status) but on conflict (lane, source_ref) PRESERVES the
	// existing runtime status — only re-stamping flow_id/flow_version. Discovery never
	// regresses an in-flight item; runtime status is written solely by the reconciler
	// via UpsertItem.
	DiscoverItem(ctx context.Context, it Item) (int, error)

	// Curation + learning (append-only).
	InsertGateDecision(ctx context.Context, d GateDecision) error
	InsertFeedback(ctx context.Context, f Feedback) error
	InsertInterestProfile(ctx context.Context, p InterestProfile) error

	// --- Reads (Phase 1) -----------------------------------------------------
	// The reconciler is level-triggered: it observes desired state (flows +
	// items) vs actual (item_steps) and acts. These reads back that observation;
	// the ingest reads a flow to stamp flow_version; the shim reads an item to
	// recover its source_ref. All are pure reads — no decision is made here.

	// GetFlow returns the flow with the given name (found=false if absent).
	GetFlow(ctx context.Context, name string) (Flow, bool, error)
	// GetItem returns the item by id (found=false if absent).
	GetItem(ctx context.Context, id int) (Item, bool, error)
	// ListActiveItems returns items not yet in a terminal status
	// (terminal = done | filtered | failed | quarantine), ordered by id.
	ListActiveItems(ctx context.Context) ([]Item, error)
	// ListFlowSteps returns the enabled steps of a flow ordered by seq.
	ListFlowSteps(ctx context.Context, flowID int) ([]FlowStep, error)
	// ListItemSteps returns an item's runtime steps ordered by seq.
	ListItemSteps(ctx context.Context, itemID int) ([]ItemStep, error)
	// ListProvidersForCapability returns the enabled providers of a capability,
	// ordered by name for deterministic selection.
	ListProvidersForCapability(ctx context.Context, capability string) ([]Provider, error)
	// GetProvider returns a single provider by name (found=false if absent or it has
	// been removed from config). The timeout->fallback path uses it to read the
	// activation of the provider it re-queues a step onto.
	GetProvider(ctx context.Context, name string) (Provider, bool, error)
	// GetRoutingPolicy returns the policy for a scope (a capability name or
	// policyScopeGlobal), found=false if absent. The router reads the capability-scoped
	// policy first, then falls back to the global one.
	GetRoutingPolicy(ctx context.Context, scope string) (RoutingPolicy, bool, error)

	// --- Health feed (Phase 2) -----------------------------------------------
	// TouchProviderHeartbeat stamps providers.heartbeat_at = now for a live provider,
	// so the router's health gate keeps it eligible. A worker calls it when it pulls
	// work (proof of life); unknown names are a no-op. Best-effort liveness, never a
	// full-record upsert (it must not clobber the provider's config columns).
	TouchProviderHeartbeat(ctx context.Context, name string) error

	// --- Claim (Phase 1) -----------------------------------------------------
	// ClaimPendingStep is the worker's pull: it atomically claims the
	// frontmost pending step of a capability with
	//   SELECT ... WHERE capability=$1 AND status='pending'
	//   ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
	// then transitions it pending->running, bumps attempt and stamps the
	// heartbeat — so no two workers ever claim the same row. Returns
	// (nil, nil) when the queue is empty for that capability.
	ClaimPendingStep(ctx context.Context, capability string) (*ItemStep, error)
}

// ---------------------------------------------------------------------------
// Real database: Neon PostgreSQL via pgx
// ---------------------------------------------------------------------------

type pgxDatabase struct{ conn *pgx.Conn }

func jsonOrEmpty(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	return string(raw)
}

func (d *pgxDatabase) UpsertCapability(ctx context.Context, c Capability) error {
	const q = `
		INSERT INTO capabilities (name, io_contract, description)
		VALUES ($1, $2::jsonb, $3)
		ON CONFLICT (name) DO UPDATE SET
			io_contract = EXCLUDED.io_contract,
			description = EXCLUDED.description`
	_, err := d.conn.Exec(ctx, q, c.Name, jsonOrEmpty(c.IOContract, "{}"), nullStr(c.Description))
	return err
}

func (d *pgxDatabase) UpsertProvider(ctx context.Context, p Provider) error {
	if !isValidRuntime(p.Runtime) {
		return fmt.Errorf("invalid runtime %q", p.Runtime)
	}
	if !isValidActivation(p.Activation) {
		return fmt.Errorf("invalid activation %q", p.Activation)
	}
	const q = `
		INSERT INTO providers
			(name, capability, runtime, activation, cost, quality, latency_ms, constraints, enabled, heartbeat_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)
		ON CONFLICT (name) DO UPDATE SET
			capability   = EXCLUDED.capability,
			runtime      = EXCLUDED.runtime,
			activation   = EXCLUDED.activation,
			cost         = EXCLUDED.cost,
			quality      = EXCLUDED.quality,
			latency_ms   = EXCLUDED.latency_ms,
			constraints  = EXCLUDED.constraints,
			enabled      = EXCLUDED.enabled,
			heartbeat_at = EXCLUDED.heartbeat_at`
	_, err := d.conn.Exec(ctx, q,
		p.Name, p.Capability, p.Runtime, p.Activation, p.Cost, p.Quality, p.LatencyMs,
		jsonOrEmpty(p.Constraints, "{}"), p.Enabled, p.HeartbeatAt)
	return err
}

func (d *pgxDatabase) UpsertFlow(ctx context.Context, f Flow) (int, error) {
	const q = `
		INSERT INTO flows (name, source_type, enabled, version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET
			source_type = EXCLUDED.source_type,
			enabled     = EXCLUDED.enabled,
			version     = EXCLUDED.version
		RETURNING id`
	version := f.Version
	if version == 0 {
		version = 1
	}
	var id int
	err := d.conn.QueryRow(ctx, q, f.Name, f.SourceType, f.Enabled, version).Scan(&id)
	return id, err
}

func (d *pgxDatabase) UpsertFlowStep(ctx context.Context, s FlowStep) error {
	const q = `
		INSERT INTO flow_steps (flow_id, seq, capability, options, enabled)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (flow_id, seq) DO UPDATE SET
			capability = EXCLUDED.capability,
			options    = EXCLUDED.options,
			enabled    = EXCLUDED.enabled`
	_, err := d.conn.Exec(ctx, q, s.FlowID, s.Seq, s.Capability, jsonOrEmpty(s.Options, "{}"), s.Enabled)
	return err
}

func (d *pgxDatabase) UpsertRoutingPolicy(ctx context.Context, p RoutingPolicy) error {
	const q = `
		INSERT INTO routing_policies (scope, cost_weight, quality_weight, fallback)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (scope) DO UPDATE SET
			cost_weight    = EXCLUDED.cost_weight,
			quality_weight = EXCLUDED.quality_weight,
			fallback       = EXCLUDED.fallback`
	_, err := d.conn.Exec(ctx, q, p.Scope, p.CostWeight, p.QualityWeight, jsonOrEmpty(p.Fallback, "[]"))
	return err
}

func (d *pgxDatabase) UpsertItem(ctx context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, fmt.Errorf("invalid item status %q", it.Status)
	}
	const q = `
		INSERT INTO items (lane, source_ref, flow_id, flow_version, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (lane, source_ref) DO UPDATE SET
			flow_id      = EXCLUDED.flow_id,
			flow_version = EXCLUDED.flow_version,
			status       = EXCLUDED.status
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, it.Lane, it.SourceRef, it.FlowID, it.FlowVersion, it.Status).Scan(&id)
	return id, err
}

func (d *pgxDatabase) DiscoverItem(ctx context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, fmt.Errorf("invalid item status %q", it.Status)
	}
	// The flow stamp (flow_id + flow_version) and status are frozen at INSERT: the
	// conflict path re-stamps NOTHING. An in-flight item finishes on the flow shape it was
	// discovered with — re-discovery after a flow edit must neither regress its runtime
	// status (the reconciler owns that) nor re-stamp its flow_version (new items pick up the
	// new version; in-flight ones keep the old). The DO UPDATE is a deliberate no-op keep
	// (flow_id = its own current value) purely so RETURNING yields the existing row's id.
	const q = `
		INSERT INTO items (lane, source_ref, flow_id, flow_version, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (lane, source_ref) DO UPDATE SET
			flow_id = items.flow_id
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, it.Lane, it.SourceRef, it.FlowID, it.FlowVersion, it.Status).Scan(&id)
	return id, err
}

func (d *pgxDatabase) UpsertItemStep(ctx context.Context, s ItemStep) error {
	if !isValidStepStatus(s.Status) {
		return fmt.Errorf("invalid item_step status %q", s.Status)
	}
	const q = `
		INSERT INTO item_steps
			(item_id, seq, capability, status, assigned_provider, attempt, heartbeat_at, output_ref, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (item_id, seq) DO UPDATE SET
			capability        = EXCLUDED.capability,
			status            = EXCLUDED.status,
			assigned_provider = EXCLUDED.assigned_provider,
			attempt           = EXCLUDED.attempt,
			heartbeat_at      = EXCLUDED.heartbeat_at,
			output_ref        = EXCLUDED.output_ref,
			error             = EXCLUDED.error`
	_, err := d.conn.Exec(ctx, q,
		s.ItemID, s.Seq, s.Capability, s.Status, nullStr(s.AssignedProvider),
		s.Attempt, s.HeartbeatAt, nullStr(s.OutputRef), nullStr(s.Error))
	return err
}

func (d *pgxDatabase) InsertGateDecision(ctx context.Context, dec GateDecision) error {
	if !isValidGate(dec.Gate) {
		return fmt.Errorf("invalid gate %q", dec.Gate)
	}
	if !isValidDecision(dec.Decision) {
		return fmt.Errorf("invalid decision %q", dec.Decision)
	}
	const q = `
		INSERT INTO gate_decisions (item_id, gate, decision, score, rank, decided_by, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := d.conn.Exec(ctx, q, dec.ItemID, dec.Gate, dec.Decision, dec.Score, dec.Rank, dec.DecidedBy, nullStr(dec.Reason))
	return err
}

func (d *pgxDatabase) InsertFeedback(ctx context.Context, f Feedback) error {
	if !isValidTargetType(f.TargetType) {
		return fmt.Errorf("invalid feedback target_type %q", f.TargetType)
	}
	const q = `
		INSERT INTO feedback (target_type, target_ref, signal, source)
		VALUES ($1, $2, $3, $4)`
	_, err := d.conn.Exec(ctx, q, f.TargetType, f.TargetRef, f.Signal, f.Source)
	return err
}

func (d *pgxDatabase) InsertInterestProfile(ctx context.Context, p InterestProfile) error {
	const q = `
		INSERT INTO interest_profile (version, topics, authors, anti_topics, weights)
		VALUES ($1, $2::jsonb, $3::jsonb, $4::jsonb, $5::jsonb)`
	_, err := d.conn.Exec(ctx, q, p.Version,
		jsonOrEmpty(p.Topics, "[]"), jsonOrEmpty(p.Authors, "[]"),
		jsonOrEmpty(p.AntiTopics, "[]"), jsonOrEmpty(p.Weights, "{}"))
	return err
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// Entrypoint
// ---------------------------------------------------------------------------

func loadDatabaseURL() string { return os.Getenv("DATABASE_URL") }

// usage documents the Phase 1 subcommands. rara-core is one binary with several roles,
// each deployed where the architecture puts it: `reconcile` runs always-on in the VPC;
// `work --capability transcrever` runs alongside scribe on the Mac; `work --capability
// destilar` is the woken Cloud Run job; `seed`/`ingest` are operational one-shots.
const usage = `rara-core — 2.0 control plane

Usage: core-job <command> [flags]

Commands:
  seed                       Seed the YouTube lane config (capabilities, providers, flow)
  ingest                     Populate the items spine from channel_videos ∪ playlist_videos
  reconcile [--loop]         Run the reconciler: one pass, or always-on with --loop
  work --capability <cap>    Run a worker shim that pulls and processes its assignments
                             (cap: transcrever | destilar)
  status                     Phase 0 health check: confirm the control tables are reachable
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]

	dbURL := loadDatabaseURL()
	if dbURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}
	// Signal-aware context: SIGINT/SIGTERM cancel it, so the always-on reconcile loop and
	// the worker drain stop gracefully (the VPC/Cloud Run lifecycle delivers SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := pgx.Connect(connectCtx, dbURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(ctx)
	db := &pgxDatabase{conn: conn}

	switch cmd {
	case "seed":
		if err := SeedYouTubeLane(ctx, db); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Println("rara-core: YouTube lane config seeded")

	case "ingest":
		n, err := IngestYouTube(ctx, db, &pgxSpineSource{conn: conn})
		if err != nil {
			log.Fatalf("ingest: %v", err)
		}
		log.Printf("rara-core: ingested %d YouTube videos into the items spine", n)

	case "reconcile":
		runReconcile(ctx, db, os.Args[2:])

	case "work":
		runWork(ctx, db, conn, os.Args[2:])

	case "status":
		var capCount int
		if err := conn.QueryRow(ctx, `SELECT COUNT(*) FROM capabilities`).Scan(&capCount); err != nil {
			log.Fatalf("Control tables not reachable (did migrations run?): %v", err)
		}
		log.Printf("rara-core ready: %d capabilities registered", capCount)

	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

// runReconcile runs one reconcile pass, or an always-on loop with --loop. The loop is the
// VPC deployment: it must stay awake while the Mac sleeps and Cloud Run scales to zero.
func runReconcile(ctx context.Context, db Database, argv []string) {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	loop := fs.Bool("loop", false, "run continuously on RECONCILE_INTERVAL_SECONDS (default 30s)")
	_ = fs.Parse(argv)

	r := NewReconciler(db, logActivator{}) // real Cloud Run activator is Phase 2
	if v := os.Getenv("RECONCILE_STALE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.staleAfter = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("ROUTE_HEALTH_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.healthTTL = time.Duration(n) * time.Second
		}
	}
	if !*loop {
		if err := r.ReconcileOnce(ctx); err != nil {
			log.Fatalf("reconcile: %v", err)
		}
		return
	}
	interval := 30 * time.Second
	if v := os.Getenv("RECONCILE_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("rara-core reconciler: always-on, interval=%s", interval)
	for {
		if err := r.ReconcileOnce(ctx); err != nil {
			log.Printf("reconcile pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runWork runs a capability's pull loop until its queue drains. It selects the shim by
// capability: transcrever -> scribe, destilar -> distill.
func runWork(ctx context.Context, db Database, conn *pgx.Conn, argv []string) {
	fs := flag.NewFlagSet("work", flag.ExitOnError)
	capability := fs.String("capability", "", "capability to serve: transcrever | destilar")
	_ = fs.Parse(argv)

	var runner StepRunner
	switch *capability {
	case capTranscrever:
		runner = newScribeRunner(conn)
	case capDestilar:
		runner = newDistillRunner(conn)
	default:
		log.Fatalf("work: --capability must be %q or %q, got %q", capTranscrever, capDestilar, *capability)
	}
	if err := NewWorker(db, *capability, runner).RunUntilDrained(ctx); err != nil {
		log.Fatalf("work %s: %v", *capability, err)
	}
	log.Printf("rara-core worker %s: queue drained", *capability)
}
