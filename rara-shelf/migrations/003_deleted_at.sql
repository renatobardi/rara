-- migrations/003_deleted_at.sql
-- Soft-delete column for playlists (CONSOLE-FONTES #2b).
-- A deleted playlist is hidden from sources_v and skipped by shelf, but its collected
-- videos (playlist_videos) are preserved. Distinct from active=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE playlists ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Partial index over the live set (sources_v scans WHERE deleted_at IS NULL). Idempotent.
CREATE INDEX IF NOT EXISTS playlists_live_idx ON playlists (id) WHERE deleted_at IS NULL;
