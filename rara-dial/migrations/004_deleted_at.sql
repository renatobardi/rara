-- migrations/004_deleted_at.sql
-- Soft-delete column for podcast_feeds (CONSOLE-FONTES #2b).
-- A deleted feed is hidden from sources_v and skipped by dial, but its collected
-- episodes (podcast_episodes) are preserved. Distinct from active=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE podcast_feeds ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
