-- migrations/002_sources_common_columns.sql
-- Additive columns for the sources_v unified view (CONSOLE-FONTES #0).
-- Adds operator-facing metadata to playlists without touching existing data.
-- Idempotent (IF NOT EXISTS / COALESCE backfill): safe to re-apply.

ALTER TABLE playlists
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text;

-- Backfill display_name from the existing title where not yet set.
UPDATE playlists SET display_name = title WHERE display_name IS NULL;
