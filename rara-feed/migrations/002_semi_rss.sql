-- migrations/002_semi_rss.sql
-- Switch SemiAnalysis from html to rss.
-- SemiAnalysis publishes a standard WordPress RSS feed at /feed/ with full article
-- content. The html source_type returned 0 distillable items (index page has no
-- Article JSON-LD); the RSS feed has 10 recent articles including full body via
-- <content:encoded>. No code change required — the rss path already handles this.
BEGIN;

UPDATE feed_sources
SET source_type = 'rss',
    endpoint    = 'https://semianalysis.com/feed/'
WHERE name = 'SemiAnalysis'
  AND source_type = 'html';

-- Verify: both source_type and endpoint must reflect the new values.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM feed_sources
        WHERE name        = 'SemiAnalysis'
          AND source_type = 'rss'
          AND endpoint    = 'https://semianalysis.com/feed/'
    ) THEN
        RAISE EXCEPTION 'Migration 002: SemiAnalysis rss update not applied or endpoint mismatch';
    END IF;
END $$;

COMMIT;
