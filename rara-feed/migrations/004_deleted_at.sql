-- migrations/004_deleted_at.sql
-- Soft-delete column for feed_sources (CONSOLE-FONTES #2b).
-- A deleted source is hidden from sources_v and skipped by feed, but its collected
-- items are preserved. Distinct from enabled=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE feed_sources ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
