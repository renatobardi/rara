-- migrations/002_semi_rss.sql
-- Switch SemiAnalysis from html to rss.
-- SemiAnalysis publishes a standard WordPress RSS feed at /feed/ with full article
-- content. The html source_type returned 0 distillable items (index page has no
-- Article JSON-LD); the RSS feed has 10 recent articles including full body via
-- <content:encoded>. No code change required — the rss path already handles this.
--
-- NOTE: no explicit BEGIN/COMMIT — the workflow validate step wraps each file in
-- its own BEGIN/ROLLBACK, and the apply step runs via psql -f (autocommit per
-- statement). An explicit COMMIT inside a validation run commits before ROLLBACK
-- fires, persisting changes during dry-run.
--
-- Fully idempotent: handles three states —
--   (a) fresh apply: only (html, semianalysis.com) row exists
--   (b) partial apply: both (html, semianalysis.com) and (rss, /feed/) rows exist
--   (c) already applied: only (rss, /feed/) row exists

-- Remove any leftover html row (no-op in state c).
DELETE FROM feed_sources
WHERE name        = 'SemiAnalysis'
  AND source_type = 'html';

-- Ensure the rss row exists. ON CONFLICT covers states b and c where the rss
-- row was already created by a prior run.
INSERT INTO feed_sources (name, source_type, endpoint, cls, fetch_strategy, enabled)
VALUES ('SemiAnalysis', 'rss', 'https://semianalysis.com/feed/', 'b-semi', 'http', true)
ON CONFLICT (name, endpoint) DO UPDATE
    SET source_type = EXCLUDED.source_type;

-- Verify both source_type and endpoint reflect the intended values.
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
