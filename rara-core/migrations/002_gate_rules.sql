-- migrations/002_gate_rules.sql
-- Phase 3 (curation gates): the deterministic allow/deny rules layer.
--
-- Description: the cheapest layer of the curation cascade (rules -> interest_profile
--   match -> LLM-judge) is a set of operator-authored allow/deny rules evaluated against
--   an item's metadata (gate_barato: title/channel) or full text (gate_rico). They live
--   in their own table — a rule is a row, toggled with `enabled`, not code — and are read
--   by the gate worker before it ever pays for the profile match or the LLM.
--
--   Cascade contract: a matched `deny` rule DROPS the item (deny precedence — an explicit
--   deny always wins over any allow); a matched `allow` rule KEEPS it; no match ESCALATES
--   to the next cascade layer. The rules layer is purely deterministic, so gate_decisions
--   rows it produces carry decided_by='rules' and no confidence score.
--
--   Idempotent (CREATE TABLE IF NOT EXISTS): re-applying the migration never clobbers.
--   No cross-agent tables (the 1.0 isolation convention holds); this is rara-core's own.

-- ---------------------------------------------------------------------------
-- gate_rules — deterministic allow/deny rules for the gate cascade's rules layer.
-- A rule matches when its match_type's target field satisfies its value:
--   channel        — the item's channel/author name equals value (case-insensitive)
--   title_contains — the item's title contains value (case-insensitive substring)
-- The set is small and operator-curated; the partial index serves the gate worker's
-- "enabled rules only" read.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gate_rules (
    id         SERIAL PRIMARY KEY,
    action     VARCHAR(8)   NOT NULL,                          -- allow | deny
    match_type VARCHAR(16)  NOT NULL,                          -- channel | title_contains
    value      TEXT         NOT NULL,                          -- the channel name or title substring to match
    enabled    BOOLEAN      NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (action, match_type, value),
    CHECK (action IN ('allow', 'deny')),
    CHECK (match_type IN ('channel', 'title_contains'))
);

-- The gate worker reads only the enabled rules.
CREATE INDEX IF NOT EXISTS idx_gate_rules_enabled ON gate_rules(action) WHERE enabled = true;

-- updated_at trigger — reuse rara-core's namespaced function (created in 001).
DROP TRIGGER IF EXISTS trg_gate_rules_updated_at ON gate_rules;
CREATE TRIGGER trg_gate_rules_updated_at
    BEFORE UPDATE ON gate_rules
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

COMMENT ON TABLE gate_rules IS 'Deterministic allow/deny rules — the cheapest layer of the curation gate cascade';
COMMENT ON COLUMN gate_rules.action     IS 'allow keeps the item; deny drops it (deny precedence over allow)';
COMMENT ON COLUMN gate_rules.match_type IS 'channel (exact, case-insensitive) | title_contains (substring, case-insensitive)';
