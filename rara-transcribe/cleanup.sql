-- cleanup.sql — drop rara-scribe tables and helpers (DEV ONLY).
-- Destroys all transcripts. Never run against production data.

DROP TRIGGER IF EXISTS trg_transcripts_updated_at ON transcripts;
DROP TABLE IF EXISTS transcript_segments;
DROP TABLE IF EXISTS transcripts;
DROP FUNCTION IF EXISTS scribe_set_updated_at();
