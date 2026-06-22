-- migrations/004_add_check_constraints.sql
-- Retroactively add CHECK constraints that were missing from the initial schema.
-- Migration 001 now includes these in CREATE TABLE (for greenfield); this migration
-- adds them to existing databases. All use IF NOT EXISTS to be idempotent.

-- transcripts: source_type enum
ALTER TABLE transcripts
    ADD CONSTRAINT IF NOT EXISTS chk_transcripts_source_type
    CHECK (source_type IN ('youtube', 'podcast'));

-- transcripts: status enum
ALTER TABLE transcripts
    ADD CONSTRAINT IF NOT EXISTS chk_transcripts_status
    CHECK (status IN ('done', 'failed'));

-- transcripts: non-negative duration
ALTER TABLE transcripts
    ADD CONSTRAINT IF NOT EXISTS chk_transcripts_duration
    CHECK (duration_seconds IS NULL OR duration_seconds >= 0);

-- transcripts: non-negative attempt count
ALTER TABLE transcripts
    ADD CONSTRAINT IF NOT EXISTS chk_transcripts_attempt_count
    CHECK (attempt_count >= 0);

-- transcript_segments: non-negative seq and valid time range
ALTER TABLE transcript_segments
    ADD CONSTRAINT IF NOT EXISTS chk_segments_seq
    CHECK (seq >= 0);

ALTER TABLE transcript_segments
    ADD CONSTRAINT IF NOT EXISTS chk_segments_start
    CHECK (start_seconds >= 0);

ALTER TABLE transcript_segments
    ADD CONSTRAINT IF NOT EXISTS chk_segments_time_range
    CHECK (end_seconds >= start_seconds);
