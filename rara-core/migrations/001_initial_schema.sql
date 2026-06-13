-- migrations/001_initial_schema.sql
-- Initial schema for rara-core (the 2.0 orchestrated control plane).
-- Description: rara-core is a new, isolated agent that owns the *control* tables. It
--   does no domain work — the existing collectors/workers keep their domain tables
--   (channel_videos, transcripts, distillations, news_items, ...). There are NO foreign
--   keys across the agent boundary (the 1.0 isolation convention holds); item_steps
--   point back at worker-owned rows logically via output_ref. The FKs below are all
--   internal to rara-core's own tables.
--
--   Phase 0 scope: schema + scaffold only. No reconciler, no router, no gate logic —
--   just the tables, their constraints, the claim indexes, and the namespaced
--   updated_at trigger. Behavior lands in later phases (see ARCHITECTURE-2.0.md).
--
-- Config-as-data: flows / flow_steps / capabilities / providers / routing_policies are
-- configuration tables (a new worker or a re-wired pipeline is a row, not a redeploy).
-- items / item_steps are the runtime spine. gate_decisions / feedback / interest_profile
-- are the curation + learning substrate.

-- ---------------------------------------------------------------------------
-- updated_at trigger function — namespaced to avoid colliding with the other
-- agents' set_updated_at() variants (set_updated_at / shelf_ / scribe_ / distill_ /
-- feed_) in the shared Neon database.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION core_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- capabilities — logical tasks with a fixed I/O contract. The set is fixed by the
-- architecture; providers and flow_steps reference a capability by name.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS capabilities (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(32)  NOT NULL,                          -- coletar, transcrever, extrair, gate_barato, gate_rico, destilar
    io_contract JSONB        NOT NULL DEFAULT '{}'::jsonb,      -- declared input/output shape (reserved; filled in later phases)
    description TEXT,
    created_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (name)
);

-- ---------------------------------------------------------------------------
-- providers — concrete implementations of a capability. Adding a worker = inserting
-- a provider row. The router (later phase) picks one per step by policy + constraints.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS providers (
    id           SERIAL PRIMARY KEY,
    name         VARCHAR(48)   NOT NULL,                        -- asr-youtube, asr-direct-audio, manual-inbox, ...
    capability   VARCHAR(32)   NOT NULL REFERENCES capabilities(name),
    runtime      VARCHAR(8)    NOT NULL,                        -- local | cloudrun | vpc
    activation   VARCHAR(10)   NOT NULL,                        -- resident | on_demand
    cost         NUMERIC(10,4) NOT NULL DEFAULT 0,              -- relative cost weight for the router
    quality      NUMERIC(4,3)  NOT NULL DEFAULT 0,              -- 0..1 quality weight for the router
    latency_ms   INT           NOT NULL DEFAULT 0,              -- typical latency hint
    constraints  JSONB         NOT NULL DEFAULT '{}'::jsonb,    -- hard constraints, e.g. {"requires":"residential"}
    enabled      BOOLEAN       NOT NULL DEFAULT true,
    heartbeat_at TIMESTAMPTZ,                                   -- last seen alive; NULL = never (reconciler liveness)
    created_at   TIMESTAMPTZ   DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ   DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (name),
    CHECK (runtime IN ('local', 'cloudrun', 'vpc')),
    CHECK (activation IN ('resident', 'on_demand'))
);

CREATE INDEX IF NOT EXISTS idx_providers_capability ON providers(capability) WHERE enabled = true;

-- ---------------------------------------------------------------------------
-- flows — one declarative pipeline per source lane. `version` is bumped whenever a
-- flow's steps change and is stamped onto items at discovery, so in-flight items
-- finish on their old version while new items pick up the new shape.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS flows (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(48)  NOT NULL,                          -- youtube_channels, youtube_playlists, podcast, email, linkedin, news
    source_type VARCHAR(16)  NOT NULL,                          -- youtube | podcast | email | linkedin | news
    enabled     BOOLEAN      NOT NULL DEFAULT true,
    version     INT          NOT NULL DEFAULT 1,                -- stamped onto items.flow_version
    created_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (name)
);

-- ---------------------------------------------------------------------------
-- flow_steps — ordered steps of a flow. Each step carries per-step `options`
-- (e.g. {"gate":"skip"}); toggling curation off for a lane is a flow_steps edit.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS flow_steps (
    id         SERIAL PRIMARY KEY,
    flow_id    INT          NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    seq        INT          NOT NULL,                           -- 1-based step ordinal within the flow
    capability VARCHAR(32)  NOT NULL REFERENCES capabilities(name),
    options    JSONB        NOT NULL DEFAULT '{}'::jsonb,       -- per-step toggles, e.g. {"gate":"skip"}
    enabled    BOOLEAN      NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (flow_id, seq)
);

-- ---------------------------------------------------------------------------
-- routing_policies — cost<->quality weighting + ordered fallback, global or scoped
-- to a single capability. Hard constraints live on the providers; the policy decides
-- among the providers that satisfy them.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS routing_policies (
    id             SERIAL PRIMARY KEY,
    scope          VARCHAR(32)  NOT NULL,                       -- 'global' or a capability name
    cost_weight    NUMERIC(4,3) NOT NULL DEFAULT 0.5,
    quality_weight NUMERIC(4,3) NOT NULL DEFAULT 0.5,
    fallback       JSONB        NOT NULL DEFAULT '[]'::jsonb,   -- ordered list of provider names
    created_at     TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (scope)
);

-- ---------------------------------------------------------------------------
-- items — the canonical spine. One lightweight materialized row per discovered work
-- item, upserted at discovery (idempotent on the source's natural key). flow_version
-- is stamped here so the item finishes on the flow shape it started with.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS items (
    id           SERIAL PRIMARY KEY,
    lane         VARCHAR(16)  NOT NULL,                         -- youtube | podcast | email | linkedin | news
    source_ref   TEXT         NOT NULL,                         -- natural key in the worker domain (youtube_video_id, url, ...)
    flow_id      INT          NOT NULL REFERENCES flows(id),
    flow_version INT          NOT NULL,                         -- copied from flows.version at discovery
    status       VARCHAR(16)  NOT NULL DEFAULT 'discovered',
    created_at   TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (lane, source_ref),
    CHECK (status IN ('discovered', 'to_text', 'distilled', 'done', 'filtered', 'quarantine', 'failed'))
);

CREATE INDEX IF NOT EXISTS idx_items_status ON items(status);

-- ---------------------------------------------------------------------------
-- item_steps — the mutable runtime state-rows (one per item step). Updated in place;
-- retries bump `attempt`. output_ref points back at the worker-owned domain row
-- (transcripts.id, distillations.id, ...) — a logical cross-agent link, not an FK.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS item_steps (
    id                SERIAL PRIMARY KEY,
    item_id           INT          NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    seq               INT          NOT NULL,                    -- mirrors flow_steps.seq for this item's flow
    capability        VARCHAR(32)  NOT NULL REFERENCES capabilities(name),
    status            VARCHAR(12)  NOT NULL DEFAULT 'pending',  -- pending | assigned | running | done | failed | skipped
    assigned_provider VARCHAR(48)  REFERENCES providers(name),  -- NULL until the router assigns one
    attempt           INT          NOT NULL DEFAULT 0,          -- bumped on each retry
    heartbeat_at      TIMESTAMPTZ,                              -- worker liveness while assigned/running
    output_ref        TEXT,                                    -- worker-owned domain row id (logical link, no FK)
    error             TEXT,
    created_at        TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (item_id, seq),
    CHECK (status IN ('pending', 'assigned', 'running', 'done', 'failed', 'skipped'))
);

-- Claim index: workers pull their work with
--   SELECT ... FROM item_steps WHERE capability = $1 AND status = 'pending'
--   ORDER BY id FOR UPDATE SKIP LOCKED
-- (Postgres' own work-queue primitive — no broker, no double-claim, NAT-friendly).
-- A partial index keyed on (capability, id) keeps it tight to the pending frontier
-- and gives FIFO ordering by the monotonic id.
CREATE INDEX IF NOT EXISTS idx_item_steps_claim
    ON item_steps (capability, id)
    WHERE status = 'pending';

-- Stale-heartbeat sweep: the reconciler scans assigned/running rows whose worker may
-- have died (heartbeat_at older than a threshold) to re-fire activation or fall back.
CREATE INDEX IF NOT EXISTS idx_item_steps_heartbeat
    ON item_steps (status, heartbeat_at)
    WHERE status IN ('assigned', 'running');

-- ---------------------------------------------------------------------------
-- gate_decisions — audit + training substrate. Append-only by design: each gate run
-- writes a row (keep / drop / defer), and the defer/quarantine sample is retained for
-- calibration (false-negative recovery). This is the one table that deliberately does
-- NOT follow the upsert convention — history is the point, so there is no updated_at
-- trigger and no unique key to upsert on.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gate_decisions (
    id         SERIAL PRIMARY KEY,
    item_id    INT          NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    gate       VARCHAR(16)  NOT NULL,                           -- gate_barato | gate_rico
    decision   VARCHAR(8)   NOT NULL,                           -- keep | drop | defer
    score      NUMERIC(4,3),                                    -- confidence / rank (nullable: rules layer needs none)
    decided_by VARCHAR(32)  NOT NULL,                           -- rules | profile | llm-judge
    reason     TEXT,
    created_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    CHECK (gate IN ('gate_barato', 'gate_rico')),
    CHECK (decision IN ('keep', 'drop', 'defer'))
);

CREATE INDEX IF NOT EXISTS idx_gate_decisions_item ON gate_decisions(item_id);
-- The quarantine review sample (low-confidence defers) is read for calibration.
CREATE INDEX IF NOT EXISTS idx_gate_decisions_defer ON gate_decisions(gate, decision) WHERE decision = 'defer';

-- ---------------------------------------------------------------------------
-- feedback — the learning signal. Append-only. Tunes the gates (it is not a gate
-- layer itself). Sources: explicit thumbs on distillations, periodic defer review,
-- and (deferred) implicit KURA usage.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS feedback (
    id          SERIAL PRIMARY KEY,
    target_type VARCHAR(16)  NOT NULL,                          -- item | distillation
    target_ref  TEXT         NOT NULL,                          -- items.id or distillations.id (logical link)
    signal      VARCHAR(16)  NOT NULL,                          -- up | down | usage | ...
    source      VARCHAR(24)  NOT NULL,                          -- explicit | kura-usage | defer-review
    created_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    CHECK (target_type IN ('item', 'distillation'))
);

CREATE INDEX IF NOT EXISTS idx_feedback_target ON feedback(target_type, target_ref);

-- ---------------------------------------------------------------------------
-- interest_profile — the living, versioned preferences document used by the profile
-- gate layer and as LLM-judge context. Each revision is a new immutable row (a new
-- version), so the profile's history is auditable; UNIQUE(version) enforces that.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS interest_profile (
    id          SERIAL PRIMARY KEY,
    version     INT          NOT NULL,
    topics      JSONB        NOT NULL DEFAULT '[]'::jsonb,
    authors     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    anti_topics JSONB        NOT NULL DEFAULT '[]'::jsonb,
    weights     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (version)
);

-- ---------------------------------------------------------------------------
-- updated_at triggers — only on the mutable tables. The append-only tables
-- (gate_decisions, feedback, interest_profile) have no updated_at by design.
-- ---------------------------------------------------------------------------
DROP TRIGGER IF EXISTS trg_capabilities_updated_at ON capabilities;
CREATE TRIGGER trg_capabilities_updated_at
    BEFORE UPDATE ON capabilities
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

DROP TRIGGER IF EXISTS trg_providers_updated_at ON providers;
CREATE TRIGGER trg_providers_updated_at
    BEFORE UPDATE ON providers
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

DROP TRIGGER IF EXISTS trg_flows_updated_at ON flows;
CREATE TRIGGER trg_flows_updated_at
    BEFORE UPDATE ON flows
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

DROP TRIGGER IF EXISTS trg_flow_steps_updated_at ON flow_steps;
CREATE TRIGGER trg_flow_steps_updated_at
    BEFORE UPDATE ON flow_steps
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

DROP TRIGGER IF EXISTS trg_routing_policies_updated_at ON routing_policies;
CREATE TRIGGER trg_routing_policies_updated_at
    BEFORE UPDATE ON routing_policies
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

DROP TRIGGER IF EXISTS trg_items_updated_at ON items;
CREATE TRIGGER trg_items_updated_at
    BEFORE UPDATE ON items
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

DROP TRIGGER IF EXISTS trg_item_steps_updated_at ON item_steps;
CREATE TRIGGER trg_item_steps_updated_at
    BEFORE UPDATE ON item_steps
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

-- ---------------------------------------------------------------------------
-- SEED — the capability catalog only. The set of logical tasks is fixed by the
-- architecture, so it is structural config (not behavior). Providers, flows and
-- policies are intentionally left empty: wiring the existing agents as providers and
-- declaring the lane flows is Phase 1 work. Idempotent via ON CONFLICT.
-- ---------------------------------------------------------------------------
INSERT INTO capabilities (name, description) VALUES
('coletar',     'Discover work items from a source (collector)'),
('transcrever', 'Audio -> text (ASR)'),
('extrair',     'Already-text source -> normalized text'),
('gate_barato', 'Cheap curation gate on metadata, before paying for to-text'),
('gate_rico',   'Rich curation gate on full text, before paying for distillation'),
('destilar',    'Curate text into a RAG-ready knowledge document')
ON CONFLICT (name) DO NOTHING;

-- Documentation
COMMENT ON TABLE capabilities     IS 'Logical tasks with a fixed I/O contract; referenced by providers and flow_steps';
COMMENT ON TABLE providers        IS 'Concrete implementations of a capability; adding a worker = a row here';
COMMENT ON TABLE flows            IS 'One declarative pipeline per source lane; version stamped onto items';
COMMENT ON TABLE flow_steps       IS 'Ordered steps of a flow with per-step options (e.g. gate:skip)';
COMMENT ON TABLE routing_policies IS 'cost<->quality weighting + ordered fallback, global or per-capability';
COMMENT ON TABLE items            IS 'Canonical materialized spine: one row per discovered work item';
COMMENT ON TABLE item_steps       IS 'Mutable runtime state-rows; output_ref is a logical link to worker domain rows';
COMMENT ON TABLE gate_decisions   IS 'Append-only audit + training substrate for the curation gates';
COMMENT ON TABLE feedback         IS 'Append-only learning signal that tunes the gates';
COMMENT ON TABLE interest_profile IS 'Living, versioned preferences document; each revision is a new immutable row';
COMMENT ON COLUMN items.flow_version       IS 'Copied from flows.version at discovery; in-flight items finish on their version';
COMMENT ON COLUMN item_steps.output_ref    IS 'Worker-owned domain row id (transcripts.id, distillations.id, ...); logical link, no FK';
COMMENT ON COLUMN providers.constraints    IS 'Hard routing constraints, e.g. {"requires":"residential"} for asr-youtube';
