-- migrations/004_deleted_at.sql
-- Soft-delete column for target_channels (CONSOLE-FONTES #2b).
-- A deleted channel is hidden from sources_v and skipped by harvest, but its collected
-- videos (channel_videos) are preserved. Distinct from active=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE target_channels ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Partial index over the live set: the view and harvest scan WHERE deleted_at IS NULL,
-- and soft-deleted rows accumulate forever (never hard-deleted). Idempotent.
CREATE INDEX IF NOT EXISTS target_channels_live_idx ON target_channels (id) WHERE deleted_at IS NULL;
