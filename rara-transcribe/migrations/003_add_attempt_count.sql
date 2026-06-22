-- migrations/003_add_attempt_count.sql
-- Track how many times a video has failed transcription, so permanently-failing
-- videos (deleted, private, age-gated) stop being retried every run and burning
-- yt-dlp/ASR calls. PendingVideos excludes failed rows past a retry cap.
--
-- Counting: incremented on each 'failed' save, reset to 0 on a 'done' save
-- (handled in SaveTranscript's ON CONFLICT clause). Existing rows start at 0.
ALTER TABLE transcripts ADD COLUMN IF NOT EXISTS attempt_count INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN transcripts.attempt_count IS 'Failed transcription attempts; reset to 0 on success. Rows past the retry cap are skipped by PendingVideos.';
