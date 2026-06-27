-- migrations/029_sources_v_hn_endpoint.sql
-- Allow HN sources to store a custom feed URL in the endpoint column.
-- config_summary now uses the stored endpoint when non-empty, falling back
-- to the canonical top-stories URL for rows created before this change.
--
-- DROP + CREATE (not CREATE OR REPLACE): idempotent. Safe — sources_v is a leaf read-model.

DROP VIEW IF EXISTS sources_v;

CREATE VIEW sources_v AS

-- YouTube channels — rara-harvest
SELECT
    format('youtube_channel:%s', id)                                       AS api_id,
    'youtube_channel'                                                       AS kind,
    'youtube'                                                               AS lane,
    COALESCE(display_name, channel_name)                                    AS display_name,
    tags,
    CASE WHEN active THEN 'active' ELSE 'paused' END                        AS status,
    format('https://www.youtube.com/channel/%s', youtube_channel_id)        AS config_summary,
    created_at,
    updated_at
FROM target_channels
WHERE deleted_at IS NULL

UNION ALL

-- YouTube playlists — rara-shelf
SELECT
    format('youtube_playlist:%s', id)                                       AS api_id,
    'youtube_playlist'                                                      AS kind,
    'youtube'                                                               AS lane,
    COALESCE(display_name, title)                                           AS display_name,
    tags,
    CASE WHEN active THEN 'active' ELSE 'paused' END                        AS status,
    format('https://www.youtube.com/playlist?list=%s', youtube_playlist_id) AS config_summary,
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
-- HN: use stored endpoint when non-empty; fall back to the canonical top-stories URL.
SELECT
    format('%s:%s', source_type, id)                                                              AS api_id,
    source_type                                                                                    AS kind,
    'news'                                                                                         AS lane,
    COALESCE(display_name, name)                                                                   AS display_name,
    tags,
    CASE WHEN enabled THEN 'active' ELSE 'paused' END                                              AS status,
    CASE WHEN source_type = 'hn'
         THEN COALESCE(NULLIF(endpoint, ''), 'https://news.ycombinator.com/rss')
         ELSE endpoint END                                                                          AS config_summary,
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
WHERE deleted_at IS NULL

UNION ALL

-- LinkedIn profiles — rara-clip (target_linkedin_profiles)
SELECT
    format('linkedin_profile:%s', id)                         AS api_id,
    'linkedin_profile'                                         AS kind,
    'linkedin'                                                 AS lane,
    COALESCE(display_name, profile_url)                       AS display_name,
    tags,
    CASE WHEN active THEN 'active' ELSE 'paused' END           AS status,
    profile_url                                               AS config_summary,
    created_at,
    updated_at
FROM target_linkedin_profiles
WHERE deleted_at IS NULL;

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; deleted_at IS NULL; config_summary shows the actionable URL/identifier for each kind; for HN, uses stored endpoint when set, else https://news.ycombinator.com/rss';
