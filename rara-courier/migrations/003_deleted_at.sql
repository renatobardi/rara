-- migrations/003_deleted_at.sql
-- Soft-delete column for email_sources (CONSOLE-FONTES #2b).
-- A deleted email rule is hidden from sources_v and skipped by courier, but already
-- collected emails are preserved. Distinct from enabled=false (pause).
-- Idempotent (IF NOT EXISTS): safe to re-apply.

ALTER TABLE email_sources ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Partial index over the live set (the view and courier scan WHERE deleted_at IS NULL). Idempotent.
CREATE INDEX IF NOT EXISTS email_sources_live_idx ON email_sources (id) WHERE deleted_at IS NULL;
