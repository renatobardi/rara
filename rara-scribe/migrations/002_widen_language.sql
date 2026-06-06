-- Widen language column from VARCHAR(10) to VARCHAR(50).
-- Whisper returns full language names (e.g. "azerbaijani" = 11 chars) that
-- overflow the original VARCHAR(10) limit.
ALTER TABLE transcripts ALTER COLUMN language TYPE VARCHAR(50);
