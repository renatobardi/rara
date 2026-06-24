-- migrations/003_sources_common_columns.sql
-- Additive columns for the sources_v unified view (CONSOLE-FONTES #0).
-- Adds operator-facing metadata to feed_sources; also adds the missing updated_at
-- column (initial schema omitted it) so sources_v can expose a consistent timestamp.
-- Idempotent (IF NOT EXISTS / COALESCE backfill): safe to re-apply.

ALTER TABLE feed_sources
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text,
    ADD COLUMN IF NOT EXISTS updated_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP;

-- Backfill display_name and updated_at for existing rows.
UPDATE feed_sources SET display_name = name       WHERE display_name IS NULL;
UPDATE feed_sources SET updated_at   = created_at WHERE updated_at   IS NULL;

-- Keep updated_at current on every UPDATE. feed_set_updated_at() already exists
-- (created in migration 001 for news_items); reuse it here.
DROP TRIGGER IF EXISTS trg_feed_sources_updated_at ON feed_sources;
CREATE TRIGGER trg_feed_sources_updated_at
    BEFORE UPDATE ON feed_sources
    FOR EACH ROW EXECUTE FUNCTION feed_set_updated_at();
