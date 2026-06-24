-- migrations/011_sources_v.sql
-- Read-only view that normalises all source tables into one unified shape.
-- Used by GET /v1/sources (fatia #1) for listing, filtering, and counting.
-- Lives in rara-core because the core reads all collector tables in the same Neon DB.
-- Re-creatable (CREATE OR REPLACE): idempotent, safe to re-apply.
--
-- Shape: api_id, kind, lane, display_name, tags, status, config_summary,
--        created_at, updated_at
--
-- Columns added by migration 003_sources_common_columns in each collector repo
-- (tags, display_name) and 002_email_sources in rara-courier must be applied first.

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

UNION ALL

-- News/RSS/HTML feed sources — rara-feed
-- kind = source_type ('rss' | 'html' | 'hn'); updated_at added by migration 003.
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

UNION ALL

-- Email reading rules — rara-courier (net-new, migration 002_email_sources)
SELECT
    format('email:%s', id)                                          AS api_id,
    'email'                                                          AS kind,
    'email'                                                          AS lane,
    COALESCE(display_name, 'Email rule ' || id::text)               AS display_name,
    tags,
    CASE WHEN enabled THEN 'active' ELSE 'paused' END               AS status,
    COALESCE(label, gmail_query, from_filter)                        AS config_summary,
    created_at,
    updated_at
FROM email_sources;

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources across collector tables; status=active|paused derived from active/enabled flags';
