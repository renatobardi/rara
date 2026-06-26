-- migrations/001_initial_schema.sql
-- Initial schema for rara-transcribe
-- Description: Store high-quality transcripts (native language) for the videos
--   collected by rara-harvest (channel_videos) and rara-shelf (playlist_videos).
--   Isolated from the other agents: its own tables in the same Neon database.

-- One row per transcribed source (video). For YouTube sources, youtube_video_id
-- is the global idempotency key (matching rara-harvest's contract). For local
-- files or arbitrary URLs it is NULL — each is stored as a distinct row.
CREATE TABLE IF NOT EXISTS transcripts (
    id               SERIAL PRIMARY KEY,
    source_type      VARCHAR(16)  NOT NULL CHECK (source_type IN ('youtube', 'podcast', 'email', 'linkedin', 'news')),  -- youtube | podcast | email | linkedin | news
    youtube_video_id VARCHAR(50)  UNIQUE,                   -- set only for youtube sources
    source_ref       TEXT         NOT NULL,                 -- watch url, page url or file path
    language         VARCHAR(10),                           -- native language detected (e.g. 'pt', 'en')
    engine           VARCHAR(48)  NOT NULL,                 -- e.g. 'groq/whisper-large-v3'
    transcript       TEXT,                                  -- full text, native language
    duration_seconds INT          CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
    status           VARCHAR(16)  NOT NULL DEFAULT 'done' CHECK (status IN ('done', 'failed', 'empty')),  -- 'done' | 'failed' | 'empty'
    error            TEXT,                                  -- failure reason when status = 'failed'
    created_at       TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_transcripts_status ON transcripts(status);

-- Timestamped segments for each transcript (verbatim Whisper segments, or the
-- approximate segments returned by Gemini). Enables time-based search and
-- jump-to-timestamp without re-transcribing. start/end are GLOBAL offsets
-- (already reindexed across the 10-minute audio chunks).
CREATE TABLE IF NOT EXISTS transcript_segments (
    id            SERIAL PRIMARY KEY,
    transcript_id INT           NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
    seq           INT           NOT NULL CHECK (seq >= 0),          -- order of the segment in the video
    start_seconds NUMERIC(10,3) NOT NULL CHECK (start_seconds >= 0),      -- global start offset (seconds)
    end_seconds   NUMERIC(10,3) NOT NULL CHECK (end_seconds >= start_seconds),  -- global end offset (seconds)
    text          TEXT          NOT NULL,
    UNIQUE (transcript_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_segments_transcript ON transcript_segments(transcript_id);

-- Keep transcripts.updated_at current on every UPDATE. Dedicated function name
-- to avoid colliding with rara-harvest's set_updated_at() / rara-shelf's
-- shelf_set_updated_at() in the same database.
CREATE OR REPLACE FUNCTION scribe_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_transcripts_updated_at ON transcripts;
CREATE TRIGGER trg_transcripts_updated_at
    BEFORE UPDATE ON transcripts
    FOR EACH ROW
    EXECUTE FUNCTION scribe_set_updated_at();

-- Documentation
COMMENT ON TABLE transcripts IS 'High-quality transcripts (native language) for collected videos';
COMMENT ON TABLE transcript_segments IS 'Timestamped segments per transcript (global offsets)';
COMMENT ON COLUMN transcripts.source_type IS 'youtube | podcast | email | linkedin | news';
COMMENT ON COLUMN transcripts.youtube_video_id IS 'YouTube video id; global idempotency key, NULL for non-youtube sources';
COMMENT ON COLUMN transcripts.engine IS 'ASR engine used, e.g. groq/whisper-large-v3 or gemini/gemini-2.5-flash';
COMMENT ON COLUMN transcripts.status IS 'done | failed | empty';
COMMENT ON COLUMN transcript_segments.start_seconds IS 'Global start offset in seconds (reindexed across chunks)';
