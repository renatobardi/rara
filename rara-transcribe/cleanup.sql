-- cleanup.sql — drop rara-transcribe tables and helpers (DEV ONLY).
-- Destroys all transcripts. Never run against production data.
DO $$
BEGIN
    IF current_database() NOT LIKE '%dev%' AND current_database() NOT LIKE '%test%' THEN
        RAISE EXCEPTION 'cleanup.sql may only run against a dev/test database (current: %)', current_database();
    END IF;
END $$;

DROP TRIGGER IF EXISTS trg_transcripts_updated_at ON transcripts;
DROP TABLE IF EXISTS transcript_segments;
DROP TABLE IF EXISTS transcripts;
DROP FUNCTION IF EXISTS scribe_set_updated_at();
