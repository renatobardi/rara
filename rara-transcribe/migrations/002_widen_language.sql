-- Widen language column from VARCHAR(10) to TEXT.
-- Whisper returns full language names (e.g. "azerbaijani" = 11 chars) that
-- overflow the original VARCHAR(10) limit. TEXT removes the cap entirely.
DO $$
BEGIN
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_name = 'transcripts' AND column_name = 'language') != 'text' THEN
        ALTER TABLE transcripts ALTER COLUMN language TYPE TEXT;
    END IF;
END $$;
