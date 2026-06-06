-- migrations/002_schema_refinements.sql
-- Created: 2026-06-06
-- Description:
--   1. Drop indexes that duplicate UNIQUE-constraint-backed indexes.
--   2. Keep target_channels.updated_at current via a BEFORE UPDATE trigger.
-- Idempotent: safe to run multiple times.

-- 1. Redundant indexes: youtube_video_id and youtube_channel_id already have
--    indexes created automatically by their UNIQUE constraints. These explicit
--    ones duplicated them, wasting writes and storage. Drop them where present.
DROP INDEX IF EXISTS idx_videos_youtube_id;
DROP INDEX IF EXISTS idx_channels_youtube_id;

-- 2. Maintain updated_at automatically on every UPDATE.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_target_channels_updated_at ON target_channels;
CREATE TRIGGER trg_target_channels_updated_at
    BEFORE UPDATE ON target_channels
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
