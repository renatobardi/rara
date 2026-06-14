-- migrations/001_initial_schema.sql
-- Initial schema for rara-courier (the 2.0 Email lane collector).
--
-- Description: rara-courier is a new, isolated agent that collects emails from Gmail (OAuth
--   refresh-token auth, same pattern as rara-shelf) and catalogs them. Like every rara agent
--   it owns ONLY its domain table and shares nothing but the Neon database — no foreign keys
--   across the agent boundary. The control plane (rara-core) reads `emails` to build the items
--   spine (lane=email, source_ref=message_id, sensitivity=private) and the `extrair` worker
--   reads `body` to clean it into a to-text artifact; both are cross-agent SELECTs, never writes.
--
--   Email content is PRIVATE — the control plane routes it only to local/self-host models. This
--   table is just storage; the sensitivity guarantee is enforced by rara-core's router.
--
--   Idempotent (CREATE TABLE IF NOT EXISTS, ON CONFLICT upserts): re-applying never clobbers.

-- ---------------------------------------------------------------------------
-- updated_at trigger function — namespaced to avoid colliding with the other agents'
-- set_updated_at() variants in the shared Neon database.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION mail_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- emails — one row per collected message. Global uniqueness on message_id (Gmail's stable id)
-- — the same id rara-core uses as the spine's source_ref. body is the raw message text/HTML the
-- extrair worker cleans.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS emails (
    id          SERIAL PRIMARY KEY,
    message_id  TEXT         NOT NULL,                    -- Gmail message id; spine source_ref (unique)
    sender      TEXT         NOT NULL DEFAULT '',         -- the From header
    subject     TEXT         NOT NULL DEFAULT '',
    body        TEXT         NOT NULL DEFAULT '',         -- raw body (text or HTML) the extractor cleans
    received_at TIMESTAMPTZ,                              -- Gmail internalDate
    status      VARCHAR(16)  NOT NULL DEFAULT 'new',      -- collector-side status (new | ...)
    collected_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (message_id)
);

CREATE INDEX IF NOT EXISTS idx_emails_received_at ON emails(received_at DESC);

-- updated_at trigger.
DROP TRIGGER IF EXISTS trg_emails_updated_at ON emails;
CREATE TRIGGER trg_emails_updated_at
    BEFORE UPDATE ON emails
    FOR EACH ROW EXECUTE FUNCTION mail_set_updated_at();

COMMENT ON TABLE  emails            IS 'One row per collected email (Gmail); private content, routed only to local/self-host models';
COMMENT ON COLUMN emails.message_id IS 'Gmail message id; the same id rara-core uses as items.source_ref';
COMMENT ON COLUMN emails.body       IS 'Raw message body (text or HTML) cleaned by the extrair worker';
