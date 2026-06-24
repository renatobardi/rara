-- migrations/003_deleted_at.sql
-- Soft-delete column for playlists (CONSOLE-FONTES #2b).
-- A deleted playlist is hidden from sources_v and skipped by shelf, but its collected
-- videos (playlist_videos) are preserved. Distinct from active=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE playlists ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
