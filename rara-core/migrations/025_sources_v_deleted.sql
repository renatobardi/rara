-- migrations/025_sources_v_deleted.sql
-- Soft-delete for sources: a deleted source DISAPPEARS from sources_v (and thus from the
-- Console list) while its collected content (channel_videos, podcast_episodes, distillations…)
-- is preserved — DELETE is "hide", not "destroy". Pausing (active/enabled=false) is distinct:
-- a paused source stays in the list but is skipped by the collector.
--
-- Self-contained (same pattern as 024): re-declares deleted_at IF NOT EXISTS on every source
-- table before redefining the view, so it succeeds regardless of the order in which the owner
-- collector migrations (harvest/shelf/dial/feed/courier) run. Idempotent — safe to re-apply.

-- ---------------------------------------------------------------------------
-- 1. Ensure deleted_at exists on each source table (idempotent).
-- ---------------------------------------------------------------------------
ALTER TABLE target_channels ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE playlists       ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE podcast_feeds   ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE feed_sources    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE email_sources   ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Partial indexes over the live set (every SELECT below filters deleted_at IS NULL, and
-- soft-deleted rows accumulate forever). Owned by the collector migrations; mirrored here
-- IF NOT EXISTS so the view's scans are covered regardless of migration order.
CREATE INDEX IF NOT EXISTS target_channels_live_idx ON target_channels (id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS playlists_live_idx       ON playlists       (id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS podcast_feeds_live_idx   ON podcast_feeds   (id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS feed_sources_live_idx    ON feed_sources    (id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS email_sources_live_idx   ON email_sources   (id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- 2. sources_v — same shape as 024, now filtering out soft-deleted rows.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW sources_v AS

-- YouTube channels — rara-harvest
SELECT
    format('youtube_channel:%s', id)           AS api_id,
    'youtube_channel'                           AS kind,
    'youtube'                                   AS lane,
    COALESCE(display_name, channel_name)        AS display_name,
    tags,
    CASE WHEN active  THEN 'active' ELSE 'paused' END AS status,
    channel_name                                AS config_summary,
    created_at,
    updated_at
FROM target_channels
WHERE deleted_at IS NULL

UNION ALL

-- YouTube playlists — rara-shelf
SELECT
    format('youtube_playlist:%s', id)           AS api_id,
    'youtube_playlist'                           AS kind,
    'youtube'                                    AS lane,
    COALESCE(display_name, title)                AS display_name,
    tags,
    CASE WHEN active  THEN 'active' ELSE 'paused' END AS status,
    title                                        AS config_summary,
    created_at,
    updated_at
FROM playlists
WHERE deleted_at IS NULL

UNION ALL

-- Podcast feeds — rara-dial
SELECT
    format('podcast:%s', id)                    AS api_id,
    'podcast'                                    AS kind,
    'podcast'                                    AS lane,
    COALESCE(display_name, title, feed_url)      AS display_name,
    tags,
    CASE WHEN active  THEN 'active' ELSE 'paused' END AS status,
    feed_url                                     AS config_summary,
    created_at,
    updated_at
FROM podcast_feeds
WHERE deleted_at IS NULL

UNION ALL

-- News/RSS/HTML/HN sources — rara-feed
SELECT
    format('%s:%s', source_type, id)            AS api_id,
    source_type                                  AS kind,
    'news'                                       AS lane,
    COALESCE(display_name, name)                 AS display_name,
    tags,
    CASE WHEN enabled THEN 'active' ELSE 'paused' END AS status,
    endpoint                                     AS config_summary,
    created_at,
    updated_at
FROM feed_sources
WHERE deleted_at IS NULL

UNION ALL

-- Email reading rules — rara-courier
SELECT
    format('email:%s', id)                                     AS api_id,
    'email'                                                     AS kind,
    'email'                                                     AS lane,
    COALESCE(display_name, 'Email rule ' || id::text)          AS display_name,
    tags,
    CASE WHEN enabled THEN 'active' ELSE 'paused' END          AS status,
    COALESCE(label, gmail_query, from_filter)                   AS config_summary,
    created_at,
    updated_at
FROM email_sources
WHERE deleted_at IS NULL;

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; deleted_at IS NULL (soft-deleted sources are hidden); status=active|paused derived from active/enabled flags';
