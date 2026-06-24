-- migrations/002_email_sources.sql
-- net-new: email_sources — one row per Gmail reading rule (CONSOLE-FONTES #0).
-- Each row is a pausable, taggable rule that the courier iterates on every run.
-- The courier composes a Gmail search query from (gmail_query, label, from_filter);
-- all three can coexist and are ANDed together in Gmail syntax.
-- Idempotent (IF NOT EXISTS, ON CONFLICT DO NOTHING): safe to re-apply.

CREATE TABLE IF NOT EXISTS email_sources (
    id           SERIAL  PRIMARY KEY,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    tags         text[]  NOT NULL DEFAULT '{}',
    display_name text,
    gmail_query  text,          -- free-form Gmail search, e.g. 'newer_than:30d'
    label        text,          -- Gmail label name, e.g. 'Newsletters'
    from_filter  text,          -- sender filter, e.g. '*@substack.com'
    created_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

DROP TRIGGER IF EXISTS trg_email_sources_updated_at ON email_sources;
CREATE TRIGGER trg_email_sources_updated_at
    BEFORE UPDATE ON email_sources
    FOR EACH ROW EXECUTE FUNCTION mail_set_updated_at();

-- Seed: one default rule reproducing the previous single-query behaviour so
-- collection continues without interruption after the cutover.
INSERT INTO email_sources (display_name, gmail_query)
VALUES ('Default (all recent mail)', 'newer_than:30d')
ON CONFLICT DO NOTHING;

COMMENT ON TABLE email_sources IS 'Gmail reading rules for rara-courier; each enabled row is a pausable collection target';
COMMENT ON COLUMN email_sources.gmail_query IS 'Free-form Gmail search syntax; combined with label/from_filter if both present';
COMMENT ON COLUMN email_sources.label       IS 'Gmail label filter (label:<name> in search syntax)';
COMMENT ON COLUMN email_sources.from_filter IS 'Sender filter (from:<pattern> in search syntax)';
