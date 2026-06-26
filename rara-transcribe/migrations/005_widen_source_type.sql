-- migrations/005_widen_source_type.sql
-- Widen transcripts.source_type from ('youtube','podcast') to the full set of lanes.
--
-- The `extrair` lanes (rara-extract: winnow/email, scrub/linkedin, glean/news) write their
-- cleaned to-text into the shared `transcripts` table with source_type = lane. The original
-- schema only allowed 'youtube' and 'podcast', so those writes failed with
-- chk_transcripts_source_type (SQLSTATE 23514) and the email/linkedin/news lanes never
-- completed end-to-end. This migration drops BOTH legacy constraints — the inline CHECK from
-- 001 (auto-named transcripts_source_type_check) and the named NOT VALID one from 004
-- (chk_transcripts_source_type) — and re-adds the widened set.
--
-- NOT VALID (matching 004): apply to new writes without scanning existing rows, so legacy
-- rows that may carry 'local'/'url' don't fail the migration. Idempotent: drop-if-exists then
-- add, so re-running converges.

DO $$
BEGIN
    -- Drop the inline CHECK created by 001's CREATE TABLE (Postgres default name).
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'transcripts_source_type_check') THEN
        ALTER TABLE transcripts DROP CONSTRAINT IF EXISTS transcripts_source_type_check;
    END IF;

    -- Drop the named NOT VALID constraint added by 004 (or a prior run of this migration).
    ALTER TABLE transcripts DROP CONSTRAINT IF EXISTS chk_transcripts_source_type;

    -- Re-add the widened constraint covering all five lanes.
    ALTER TABLE transcripts ADD CONSTRAINT chk_transcripts_source_type
        CHECK (source_type IN ('youtube', 'podcast', 'email', 'linkedin', 'news')) NOT VALID;
END $$;

COMMENT ON COLUMN transcripts.source_type IS 'youtube | podcast | email | linkedin | news';
