-- Widen language column from VARCHAR(10) to TEXT.
-- Whisper returns full language names (e.g. "azerbaijani" = 11 chars) that
-- overflow the original VARCHAR(10) limit. TEXT removes the cap entirely.
ALTER TABLE transcripts ALTER COLUMN language TYPE TEXT;
