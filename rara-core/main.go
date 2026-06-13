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
	"fmt"
	"log"
	"os"
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

	// Curation + learning (append-only).
	InsertGateDecision(ctx context.Context, d GateDecision) error
	InsertFeedback(ctx context.Context, f Feedback) error
	InsertInterestProfile(ctx context.Context, p InterestProfile) error
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

func main() {
	dbURL := loadDatabaseURL()
	if dbURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := pgx.Connect(connectCtx, dbURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())
	log.Println("Connected to database successfully")

	// Phase 0: the control tables exist and the persistence seam is wired, but there
	// is no reconciler yet. Confirm the schema is reachable and exit. The always-on
	// reconciler loop (observe flows + items, route, activate) arrives in Phase 1.
	var capCount int
	if err := conn.QueryRow(context.Background(), `SELECT COUNT(*) FROM capabilities`).Scan(&capCount); err != nil {
		log.Fatalf("Control tables not reachable (did migrations run?): %v", err)
	}
	log.Printf("rara-core scaffold ready: %d capabilities registered. Reconciler not implemented yet (Phase 0).", capCount)
}
