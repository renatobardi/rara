-- migrations/003_sources_common_columns.sql
-- Additive columns for the sources_v unified view (CONSOLE-FONTES #0).
-- Adds operator-facing metadata to target_channels without touching existing data.
-- Idempotent (IF NOT EXISTS / COALESCE backfill): safe to re-apply.

ALTER TABLE target_channels
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text;

-- Backfill display_name from the existing human-readable name where not yet set.
UPDATE target_channels SET display_name = channel_name WHERE display_name IS NULL;
