-- migrations/004_deleted_at.sql
-- Soft-delete column for target_channels (CONSOLE-FONTES #2b).
-- A deleted channel is hidden from sources_v and skipped by harvest, but its collected
-- videos (channel_videos) are preserved. Distinct from active=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE target_channels ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
