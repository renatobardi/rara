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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

// items.sensitivity — how freely an item's content may be sent to a model. `private`
// content (email) may only be processed by local/self-host providers; the router excludes
// any provider tagged third-party for it (constraintsSatisfied). Defaults to `public`.
const (
	sensitivityPublic  = "public"
	sensitivityPrivate = "private"
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

// gate_decisions.decided_by — which cascade layer reached the decision (the audit trail
// distinguishes the cheap deterministic layers from the paid LLM-judge).
const (
	decidedByRules   = "rules"   // deterministic allow/deny gate_rules
	decidedByProfile = "profile" // interest_profile match
	decidedByLLM     = "llm"     // LLM-judge via LiteLLM (the borderline middle only)
)

// gate_rules.action
const (
	ruleAllow = "allow"
	ruleDeny  = "deny"
)

// gate_rules.match_type
const (
	matchChannel       = "channel"        // exact channel/author name, case-insensitive
	matchTitleContains = "title_contains" // case-insensitive substring of the title
)

// feedback.signal
const (
	signalUp   = "up"
	signalDown = "down"
)

// feedback.source — provenance of a learning signal, pinned to a CHECK enum by
// migration 005. The interest_profile revision (Phase 6) consumes them all.
const (
	sourceUserExplicit     = "user_explicit"     // explicit thumbs on a distillation
	sourceQuarantineReview = "quarantine_review" // human review of a quarantined item
	sourceKURAImplicit     = "kura_implicit"     // KURA engagement on a distillation (KURA-CONTRACT.md §2)
)

func isValidFeedbackSource(s string) bool {
	switch s {
	case sourceUserExplicit, sourceQuarantineReview, sourceKURAImplicit:
		return true
	}
	return false
}

// policyScopeGlobal is the routing_policies.scope sentinel for the catch-all policy the
// router uses when no capability-scoped policy exists.
const policyScopeGlobal = "global"

// constraintResidential is the hard-constraint requirement (providers.constraints ->
// {"requires":"residential"}): egress from a residential IP, satisfied only by
// runtime=local. YouTube blocks audio download from datacenter IPs, so asr-youtube carries
// it and the router eliminates any cloudrun/vpc candidate.
const constraintResidential = "residential"

// constraintThirdParty is the providers.constraints sensitivity tag
// ({"sensitivity":"third_party"}) marking a provider that sends content off-box to a
// third-party model. The router excludes such a provider for a `private` item (only
// local/self-host may process private content); untagged/self-host providers are unrestricted.
const constraintThirdParty = "third_party"

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
func isValidSensitivity(s string) bool {
	return s == sensitivityPublic || s == sensitivityPrivate
}
func isValidRuleAction(s string) bool { return s == ruleAllow || s == ruleDeny }
func isValidMatchType(s string) bool  { return s == matchChannel || s == matchTitleContains }

// ---------------------------------------------------------------------------
// Domain types — one struct per control table. JSONB columns are carried as
// json.RawMessage so the control plane stays agnostic about their inner shape (it is
// config, validated by the workers that consume it, not by rara-core).
// ---------------------------------------------------------------------------

// Capability is a logical task with a fixed I/O contract.
//
// The json tags give the control surface (Phase 5) a clean snake_case config-as-data wire
// shape; these structs are marshaled nowhere else, so the tags affect only the surface.
type Capability struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	IOContract  json.RawMessage `json:"io_contract,omitempty"` // "" => defaults to '{}' on write
}

// Provider is a concrete implementation of a capability.
type Provider struct {
	Name        string          `json:"name"`
	Capability  string          `json:"capability"` // must reference an existing capability (FK)
	Runtime     string          `json:"runtime"`    // local | cloudrun | vpc
	Activation  string          `json:"activation"` // resident | on_demand
	Cost        float64         `json:"cost"`
	Quality     float64         `json:"quality"` // 0..1
	LatencyMs   int             `json:"latency_ms"`
	Constraints json.RawMessage `json:"constraints,omitempty"` // "" => '{}'
	Enabled     bool            `json:"enabled"`
	HeartbeatAt *time.Time      `json:"heartbeat_at,omitempty"`
}

// Flow is one declarative pipeline per source lane.
type Flow struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"`
	Enabled    bool   `json:"enabled"`
	Version    int    `json:"version"`
}

// FlowStep is one ordered step of a flow.
type FlowStep struct {
	FlowID     int             `json:"flow_id"`
	Seq        int             `json:"seq"`
	Capability string          `json:"capability"` // FK to capabilities.name
	Options    json.RawMessage `json:"options,omitempty"`
	Enabled    bool            `json:"enabled"`
}

// RoutingPolicy is a cost<->quality weighting + ordered fallback.
type RoutingPolicy struct {
	Scope         string          `json:"scope"` // 'global' or a capability name
	CostWeight    float64         `json:"cost_weight"`
	QualityWeight float64         `json:"quality_weight"`
	Fallback      json.RawMessage `json:"fallback,omitempty"` // ordered list of provider names
}

// Item is one row of the canonical spine.
type Item struct {
	ID          int    `json:"id"`
	Lane        string `json:"lane"`
	SourceRef   string `json:"source_ref"`
	FlowID      int    `json:"flow_id"`
	FlowVersion int    `json:"flow_version"`
	Status      string `json:"status"`
	// Sensitivity is `public` (default) or `private`. Stamped at discovery (email -> private)
	// and frozen thereafter; the router reads it to exclude third-party providers for private
	// content. The reconciler preserves it on every status write (it reads the full item).
	Sensitivity string `json:"sensitivity"`
}

// ItemStep is one mutable runtime state-row.
type ItemStep struct {
	ItemID           int        `json:"item_id"`
	Seq              int        `json:"seq"`
	Capability       string     `json:"capability"`
	Status           string     `json:"status"`
	AssignedProvider string     `json:"assigned_provider,omitempty"` // "" => NULL (unassigned)
	Attempt          int        `json:"attempt"`
	HeartbeatAt      *time.Time `json:"heartbeat_at,omitempty"`
	OutputRef        string     `json:"output_ref,omitempty"` // "" => NULL; logical link to a worker domain row
	Error            string     `json:"error,omitempty"`
}

// GateDecision is one append-only curation-gate audit row.
type GateDecision struct {
	ItemID    int      `json:"item_id"`
	Gate      string   `json:"gate"`
	Decision  string   `json:"decision"`
	Score     *float64 `json:"score,omitempty"` // confidence in [0,1]; nil for the rules layer (which needs none)
	Rank      *int     `json:"rank,omitempty"`  // gate_rico ordering (1 = top); nil outside gate_rico / when unranked
	DecidedBy string   `json:"decided_by"`
	Reason    string   `json:"reason,omitempty"`
}

// Feedback is one append-only learning signal.
type Feedback struct {
	TargetType string `json:"target_type"`
	TargetRef  string `json:"target_ref"`
	Signal     string `json:"signal"`
	Source     string `json:"source"`
}

// InterestProfile is one immutable version of the living preferences document.
type InterestProfile struct {
	Version    int             `json:"version"`
	Topics     json.RawMessage `json:"topics,omitempty"`
	Authors    json.RawMessage `json:"authors,omitempty"`
	AntiTopics json.RawMessage `json:"anti_topics,omitempty"`
	Weights    json.RawMessage `json:"weights,omitempty"`
}

// GateRule is one deterministic allow/deny rule — the cheapest layer of the gate cascade.
// A deny match drops the item (deny precedence); an allow match keeps it; no match
// escalates to the profile/LLM layers.
type GateRule struct {
	Action    string `json:"action"`     // allow | deny
	MatchType string `json:"match_type"` // channel | title_contains
	Value     string `json:"value"`
	Enabled   bool   `json:"enabled"`
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
	// UpsertGateRule writes one deterministic allow/deny rule (Phase 3 gate cascade),
	// idempotent on (action, match_type, value).
	UpsertGateRule(ctx context.Context, r GateRule) error

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

	// --- Curation reads (Phase 3) --------------------------------------------
	// The gate cascade reads the live profile + rules; the reconciler reads the gate's
	// decision to route keep/drop/defer; the quarantine surface lists deferred items.

	// ListGateRules returns the enabled allow/deny rules for the cascade's rules layer.
	ListGateRules(ctx context.Context) ([]GateRule, error)
	// GetLatestInterestProfile returns the highest-version interest_profile row (the live
	// preferences document), found=false when none has been seeded yet.
	GetLatestInterestProfile(ctx context.Context) (InterestProfile, bool, error)
	// LatestGateDecision returns the most recent gate_decisions row for (item, gate),
	// found=false if the gate has not run for the item. The reconciler reads it to route
	// a completed gate step (keep -> advance, drop -> filtered, defer -> quarantine).
	LatestGateDecision(ctx context.Context, itemID int, gate string) (GateDecision, bool, error)
	// ListQuarantinedItems returns items in terminal `quarantine` (the cold-start review
	// sample), ordered by id.
	ListQuarantinedItems(ctx context.Context) ([]Item, error)

	// --- Surface reads (Phase 5) ---------------------------------------------
	// The control surface (HTTP core + MCP adapter) reads state and config as data so an
	// operator/agent can observe and edit the running system. All pure reads — the surface
	// never decides; it exposes what the reconciler/gates already wrote and lets config be
	// edited through the existing idempotent upserts.

	// ListItemsByStatus returns the items in a given lifecycle status, ordered by id (the
	// surface's "list items by status" view). The status is validated by the caller.
	ListItemsByStatus(ctx context.Context, status string) ([]Item, error)
	// ListGateDecisions returns ALL gate_decisions for an item, oldest first — the full
	// curation audit trail (LatestGateDecision returns only the most recent per gate).
	ListGateDecisions(ctx context.Context, itemID int) ([]GateDecision, error)
	// ListFlows returns every flow (config-as-data), ordered by id.
	ListFlows(ctx context.Context) ([]Flow, error)
	// ListProviders returns every provider, enabled or not (config-as-data), ordered by name.
	// (ListProvidersForCapability is the router's enabled-only, per-capability view.)
	ListProviders(ctx context.Context) ([]Provider, error)
	// ListRoutingPolicies returns every routing policy (config-as-data), ordered by scope.
	ListRoutingPolicies(ctx context.Context) ([]RoutingPolicy, error)
	// ListAllGateRules returns every gate rule, enabled or not (config-as-data), ordered
	// (action, match_type, value). (ListGateRules is the cascade's enabled-only view.)
	ListAllGateRules(ctx context.Context) ([]GateRule, error)

	// --- Health feed (Phase 2) -----------------------------------------------
	// TouchProviderHeartbeat stamps providers.heartbeat_at = now for a live provider,
	// so the router's health gate keeps it eligible. A worker calls it when it pulls
	// work (proof of life); unknown names are a no-op. Best-effort liveness, never a
	// full-record upsert (it must not clobber the provider's config columns).
	TouchProviderHeartbeat(ctx context.Context, name string) error

	// --- Claim (Phase 1) -----------------------------------------------------
	// ClaimPendingStep is the worker's pull: it atomically claims the frontmost pending step
	// of a capability ASSIGNED TO the given provider with
	//   SELECT ... WHERE capability=$1 AND assigned_provider=$2 AND status='pending'
	//   ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
	// then transitions it pending->running, bumps attempt and stamps the heartbeat — so no
	// two workers ever claim the same row. The provider filter matters once a capability has
	// MORE THAN ONE provider with different runners (transcrever -> asr-youtube on the Mac vs
	// asr-direct-audio on Cloud Run): each worker pulls only the steps the reconciler routed
	// to it, never the other provider's. Returns (nil, nil) when the queue is empty.
	ClaimPendingStep(ctx context.Context, capability, provider string) (*ItemStep, error)
}

// ---------------------------------------------------------------------------
// Real database: Neon PostgreSQL via pgx
// ---------------------------------------------------------------------------

// pgConn is the subset of the pgx query API the store uses, satisfied by BOTH a single
// *pgx.Conn (the single-threaded commands: seed/ingest/reconcile/work) and a *pgxpool.Pool
// (the concurrent control surface — pgx.Conn is NOT safe for concurrent use, so the always-on
// HTTP/MCP surface runs over a pool while the reconciler keeps its own single conn).
type pgConn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

type pgxDatabase struct{ conn pgConn }

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

func (d *pgxDatabase) UpsertGateRule(ctx context.Context, r GateRule) error {
	if !isValidRuleAction(r.Action) {
		return fmt.Errorf("invalid gate rule action %q", r.Action)
	}
	if !isValidMatchType(r.MatchType) {
		return fmt.Errorf("invalid gate rule match_type %q", r.MatchType)
	}
	const q = `
		INSERT INTO gate_rules (action, match_type, value, enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (action, match_type, value) DO UPDATE SET
			enabled = EXCLUDED.enabled`
	_, err := d.conn.Exec(ctx, q, r.Action, r.MatchType, r.Value, r.Enabled)
	return err
}

func (d *pgxDatabase) UpsertItem(ctx context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, fmt.Errorf("invalid item status %q", it.Status)
	}
	sens := sensitivityOr(it.Sensitivity)
	if !isValidSensitivity(sens) {
		return 0, fmt.Errorf("invalid item sensitivity %q", it.Sensitivity)
	}
	const q = `
		INSERT INTO items (lane, source_ref, flow_id, flow_version, status, sensitivity)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (lane, source_ref) DO UPDATE SET
			flow_id      = EXCLUDED.flow_id,
			flow_version = EXCLUDED.flow_version,
			status       = EXCLUDED.status,
			sensitivity  = EXCLUDED.sensitivity
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, it.Lane, it.SourceRef, it.FlowID, it.FlowVersion, it.Status, sens).Scan(&id)
	return id, err
}

func (d *pgxDatabase) DiscoverItem(ctx context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, fmt.Errorf("invalid item status %q", it.Status)
	}
	sens := sensitivityOr(it.Sensitivity)
	if !isValidSensitivity(sens) {
		return 0, fmt.Errorf("invalid item sensitivity %q", it.Sensitivity)
	}
	// The flow stamp (flow_id + flow_version), status AND sensitivity are frozen at INSERT:
	// the conflict path re-stamps NOTHING. An in-flight item finishes on the flow shape it was
	// discovered with — re-discovery after a flow edit must neither regress its runtime
	// status (the reconciler owns that) nor re-stamp its flow_version (new items pick up the
	// new version; in-flight ones keep the old). The DO UPDATE is a deliberate no-op keep
	// (flow_id = its own current value) purely so RETURNING yields the existing row's id.
	const q = `
		INSERT INTO items (lane, source_ref, flow_id, flow_version, status, sensitivity)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (lane, source_ref) DO UPDATE SET
			flow_id = items.flow_id
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, it.Lane, it.SourceRef, it.FlowID, it.FlowVersion, it.Status, sens).Scan(&id)
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
	if !isValidFeedbackSource(f.Source) {
		return fmt.Errorf("invalid feedback source %q", f.Source)
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

// sensitivityOr defaults an empty sensitivity to `public`, mirroring the items.sensitivity
// SQL DEFAULT so a write that omits it never violates the NOT NULL / CHECK.
func sensitivityOr(s string) string {
	if s == "" {
		return sensitivityPublic
	}
	return s
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
  collect --lane linkedin    Run an automated collector (Bright Data) that writes the lane's
                             domain table and discovers items (manual inbox stays a fallback)
  reconcile [--loop]         Run the reconciler: one pass, or always-on with --loop
                             (--loop also mounts the surface if SURFACE_ADDR is set)
  surface [--addr :8080]     Serve the control surface (HTTP núcleo + MCP adapter) standalone
                             (SURFACE_TOKEN required; --addr defaults to SURFACE_ADDR/:8080)
  work --capability <cap> --provider <name>
                             Run a worker shim that pulls and processes its assignments
                             (cap: transcrever | extrair | destilar | gate_barato | gate_rico)
  feedback --distillation <id> --signal <up|down>
                             Record explicit thumbs on a distillation
  quarantine list            List items deferred to quarantine (the cold-start review sample)
  quarantine review --item <id> --signal <up|down>
                             Review a quarantined item: up rescues it, down confirms the drop
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
		for _, seed := range []struct {
			name string
			fn   func(context.Context, Database) error
		}{
			{"youtube", SeedYouTubeLane},
			{"podcast", SeedPodcastLane},
			{"email", SeedEmailLane},
			{"linkedin", SeedLinkedInLane},
		} {
			if err := seed.fn(ctx, db); err != nil {
				log.Fatalf("seed %s: %v", seed.name, err)
			}
		}
		log.Println("rara-core: lane config seeded (youtube, podcast, email, linkedin)")

	case "ingest":
		runIngest(ctx, db, conn, os.Args[2:])

	case "collect":
		runCollect(ctx, db, conn, os.Args[2:])

	case "reconcile":
		runReconcile(ctx, db, dbURL, os.Args[2:])

	case "surface":
		runSurface(ctx, dbURL, os.Args[2:])

	case "work":
		runWork(ctx, db, conn, os.Args[2:])

	case "feedback":
		runFeedback(ctx, db, os.Args[2:])

	case "quarantine":
		runQuarantine(ctx, db, os.Args[2:])

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

// runIngest populates the items spine for a lane from its collector's domain tables. The lane
// selects which SpineSource to read (youtube: channel_videos ∪ playlist_videos; podcast:
// podcast_episodes). Each is idempotent — re-ingesting converges.
func runIngest(ctx context.Context, db Database, conn *pgx.Conn, argv []string) {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	lane := fs.String("lane", laneYouTube, "lane to ingest: youtube | podcast | email")
	_ = fs.Parse(argv)

	var (
		n   int
		err error
	)
	switch *lane {
	case laneYouTube:
		n, err = IngestYouTube(ctx, db, &pgxSpineSource{conn: conn})
	case lanePodcast:
		n, err = IngestPodcast(ctx, db, &pgxPodcastSource{conn: conn})
	case laneEmail:
		n, err = IngestEmail(ctx, db, &pgxEmailSource{conn: conn})
	default:
		log.Fatalf("ingest: --lane must be one of youtube|podcast|email, got %q", *lane)
	}
	if err != nil {
		log.Fatalf("ingest %s: %v", *lane, err)
	}
	log.Printf("rara-core: ingested %d %s items into the spine", n, *lane)
}

// runCollect runs an automated collector for a lane: it fetches from the external source and
// writes the lane's domain table + discovers spine items, behind the SAME contract the manual
// path uses. Today only the LinkedIn lane has an automated collector (Bright Data); the manual
// inbox stays available as a fallback for posts the crawl misses.
func runCollect(ctx context.Context, db Database, conn *pgx.Conn, argv []string) {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	lane := fs.String("lane", laneLinkedIn, "lane to collect: linkedin")
	_ = fs.Parse(argv)

	switch *lane {
	case laneLinkedIn:
		n, err := CollectLinkedIn(ctx, db, newPgxLinkedInInbox(conn), newBrightDataLinkedInSource())
		if err != nil {
			log.Fatalf("collect linkedin: %v", err)
		}
		log.Printf("rara-core: collected %d linkedin post(s) into the spine", n)
	default:
		log.Fatalf("collect: --lane must be linkedin, got %q", *lane)
	}
}

// runReconcile runs one reconcile pass, or an always-on loop with --loop. The loop is the
// VPC deployment: it must stay awake while the Mac sleeps and Cloud Run scales to zero. When
// looping, it also mounts the control surface in the SAME process (alongside the ticker) if
// SURFACE_ADDR is set — the always-on HTTP/MCP core the architecture puts next to the reconciler.
func runReconcile(ctx context.Context, db Database, dbURL string, argv []string) {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	loop := fs.Bool("loop", false, "run continuously on RECONCILE_INTERVAL_SECONDS (default 30s)")
	_ = fs.Parse(argv)

	if *loop {
		if addr := os.Getenv("SURFACE_ADDR"); addr != "" {
			// The surface runs over its OWN pool (concurrency-safe), independent of the
			// reconciler's single conn. A failure to mount it is logged, not fatal — the
			// reconciler must keep running.
			go func() {
				if err := serveSurfacePool(ctx, dbURL, addr, os.Getenv("SURFACE_TOKEN")); err != nil {
					log.Printf("surface: %v", err)
				}
			}()
		}
	}

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

// runSurface serves ONLY the control surface (HTTP núcleo + MCP adapter), standalone — useful
// to run the surface apart from the reconciler. The always-on VPC deployment normally co-hosts
// it inside `reconcile --loop` (SURFACE_ADDR); this is the same server, alone. SURFACE_ADDR
// defaults to :8080; SURFACE_TOKEN is required (the surface refuses to serve open).
func runSurface(ctx context.Context, dbURL string, argv []string) {
	fs := flag.NewFlagSet("surface", flag.ExitOnError)
	addr := fs.String("addr", envOr("SURFACE_ADDR", ":8080"), "listen address")
	_ = fs.Parse(argv)
	if err := serveSurfacePool(ctx, dbURL, *addr, os.Getenv("SURFACE_TOKEN")); err != nil {
		log.Fatalf("surface: %v", err)
	}
}

// serveSurfacePool opens a dedicated connection POOL for the control surface and serves it.
// The pool (not a single conn) is required because the HTTP/MCP server handles requests
// concurrently and pgx.Conn is not concurrency-safe. The token is checked before opening the
// pool so a misconfigured surface never even connects.
func serveSurfacePool(ctx context.Context, dbURL, addr, token string) error {
	if token == "" {
		return fmt.Errorf("surface: SURFACE_TOKEN is required (refusing to serve without auth)")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("surface: connection pool: %w", err)
	}
	defer pool.Close()
	core := NewCore(&pgxDatabase{conn: pool}, newPgxLinkedInInbox(pool))
	return ServeSurface(ctx, core, addr, token)
}

// runWork runs a (capability, provider) pull loop until its queue drains. A worker serves
// exactly one provider so it claims only the steps the reconciler routed to it — required
// once a capability has several providers with different runners (transcrever -> asr-youtube
// on the Mac vs asr-direct-audio on Cloud Run).
func runWork(ctx context.Context, db Database, conn *pgx.Conn, argv []string) {
	fs := flag.NewFlagSet("work", flag.ExitOnError)
	capability := fs.String("capability", "", "capability to serve: transcrever | extrair | destilar | gate_barato | gate_rico")
	provider := fs.String("provider", "", "the provider this worker serves (the reconciler assigns steps to it)")
	_ = fs.Parse(argv)
	if *provider == "" {
		log.Fatalf("work: --provider is required (a capability may have several providers with different runners)")
	}

	runner := selectRunner(db, conn, *capability, *provider)
	if err := NewWorker(db, *capability, *provider, runner).RunUntilDrained(ctx); err != nil {
		log.Fatalf("work %s/%s: %v", *capability, *provider, err)
	}
	log.Printf("rara-core worker %s/%s: queue drained", *capability, *provider)
}

// selectRunner maps a (capability, provider) pair to its StepRunner shim. transcrever has two
// providers with different entrypoints (asr-youtube builds a watch URL; asr-direct-audio reads
// a direct enclosure URL), so the provider — not just the capability — selects the runner.
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
		// extrair has a provider per text lane (email vs linkedin); each reads a different
		// domain table, so the provider — not just the capability — selects the runner.
		switch provider {
		case provExtrairEmail:
			return newExtractRunner(conn)
		case provExtrairLinked:
			return newLinkedInExtractRunner(conn)
		default:
			log.Fatalf("work extrair: unknown provider %q", provider)
		}
	case capDestilar:
		return newDistillRunner(conn)
	case capGateBarato, capGateRico:
		judge, err := newLiteLLMJudge()
		if err != nil {
			log.Fatalf("work %s: %v", capability, err)
		}
		return newGateRunner(db, conn, capability, judge)
	default:
		log.Fatalf("work: --capability must be one of transcrever|extrair|destilar|gate_barato|gate_rico, got %q", capability)
	}
	return nil // unreachable: every branch above either returns or log.Fatalf-exits
}

// runFeedback records explicit thumbs on a distillation (deliverable #4).
func runFeedback(ctx context.Context, db Database, argv []string) {
	fs := flag.NewFlagSet("feedback", flag.ExitOnError)
	distillation := fs.String("distillation", "", "distillation id to rate")
	signal := fs.String("signal", "", "up | down")
	_ = fs.Parse(argv)
	if *distillation == "" || *signal == "" {
		log.Fatalf("feedback: --distillation and --signal are required")
	}
	if err := CaptureDistillationFeedback(ctx, db, *distillation, *signal); err != nil {
		log.Fatalf("feedback: %v", err)
	}
	log.Printf("rara-core: recorded %q feedback on distillation %s", *signal, *distillation)
}

// runQuarantine lists or reviews the quarantine sample (deliverable #5). `list` prints the
// deferred items; `review --item N --signal up|down` resolves one (up rescues, down drops).
func runQuarantine(ctx context.Context, db Database, argv []string) {
	if len(argv) == 0 {
		log.Fatalf("quarantine: expected subcommand 'list' or 'review'")
	}
	switch argv[0] {
	case "list":
		items, err := db.ListQuarantinedItems(ctx)
		if err != nil {
			log.Fatalf("quarantine list: %v", err)
		}
		log.Printf("rara-core: %d item(s) in quarantine", len(items))
		for _, it := range items {
			fmt.Printf("  item %d\t%s\t%s\n", it.ID, it.Lane, it.SourceRef)
		}
	case "review":
		fs := flag.NewFlagSet("review", flag.ExitOnError)
		item := fs.Int("item", 0, "quarantined item id")
		signal := fs.String("signal", "", "up (rescue) | down (confirm drop)")
		_ = fs.Parse(argv[1:])
		if *item == 0 || *signal == "" {
			log.Fatalf("quarantine review: --item and --signal are required")
		}
		if err := ReviewQuarantine(ctx, db, *item, *signal); err != nil {
			log.Fatalf("quarantine review: %v", err)
		}
		log.Printf("rara-core: reviewed quarantined item %d (%q)", *item, *signal)
	default:
		log.Fatalf("quarantine: unknown subcommand %q (want 'list' or 'review')", argv[0])
	}
}
