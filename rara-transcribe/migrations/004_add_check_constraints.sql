-- migrations/004_add_check_constraints.sql
-- Retroactively add CHECK constraints missing from the initial schema.
-- Migration 001 now includes these in CREATE TABLE (for greenfield); this migration
-- adds them to existing databases. Each block is idempotent via pg_constraint check.
--
-- All constraints use NOT VALID: they apply to new writes without scanning existing rows.
-- This avoids failures from legacy data written before the constraints existed.

DO $$
BEGIN
    -- transcripts: source_type enum ('youtube' | 'podcast'; legacy rows may have 'local'/'url')
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_transcripts_source_type') THEN
        ALTER TABLE transcripts ADD CONSTRAINT chk_transcripts_source_type
            CHECK (source_type IN ('youtube', 'podcast')) NOT VALID;
    END IF;

    -- transcripts: status enum ('done' | 'failed' | 'empty')
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_transcripts_status') THEN
        ALTER TABLE transcripts ADD CONSTRAINT chk_transcripts_status
            CHECK (status IN ('done', 'failed', 'empty')) NOT VALID;
    END IF;

    -- transcripts: non-negative duration (nullable column)
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_transcripts_duration') THEN
        ALTER TABLE transcripts ADD CONSTRAINT chk_transcripts_duration
            CHECK (duration_seconds IS NULL OR duration_seconds >= 0) NOT VALID;
    END IF;

    -- transcripts: non-negative attempt count
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_transcripts_attempt_count') THEN
        ALTER TABLE transcripts ADD CONSTRAINT chk_transcripts_attempt_count
            CHECK (attempt_count >= 0) NOT VALID;
    END IF;

    -- transcript_segments: non-negative seq
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_segments_seq') THEN
        ALTER TABLE transcript_segments ADD CONSTRAINT chk_segments_seq
            CHECK (seq >= 0) NOT VALID;
    END IF;

    -- transcript_segments: non-negative start offset
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_segments_start') THEN
        ALTER TABLE transcript_segments ADD CONSTRAINT chk_segments_start
            CHECK (start_seconds >= 0) NOT VALID;
    END IF;

    -- transcript_segments: end must not precede start
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_segments_time_range') THEN
        ALTER TABLE transcript_segments ADD CONSTRAINT chk_segments_time_range
            CHECK (end_seconds >= start_seconds) NOT VALID;
    END IF;
END $$;
