-- migrations/003_sources_common_columns.sql
-- Additive columns for the sources_v unified view (CONSOLE-FONTES #0).
-- Adds operator-facing metadata to podcast_feeds without touching existing data.
-- Idempotent (IF NOT EXISTS / COALESCE backfill): safe to re-apply.

ALTER TABLE podcast_feeds
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text;

-- Backfill display_name from the existing title where not yet set.
UPDATE podcast_feeds SET display_name = title WHERE display_name IS NULL;
