-- migrations/027_sources_v_youtube_url.sql
-- Fix config_summary for youtube_channel and youtube_playlist rows in sources_v.
-- Previously both used the human-readable name/title (identical to display_name, no URL).
-- Now they expose a constructed YouTube URL so the Fontes UI subtitle is actionable.
--
-- DROP + CREATE (not CREATE OR REPLACE): the youtube config_summary changes from a varchar
-- column (channel_name / title) to a text expression (format(...)), and CREATE OR REPLACE
-- forbids changing a view column's data type. The view is a leaf read-model (no dependents),
-- so dropping and recreating it inside the migration transaction is safe and atomic.
-- Idempotent — safe to re-apply (DROP IF EXISTS).

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

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; deleted_at IS NULL; config_summary shows the actionable URL for URL-based kinds (youtube_channel, youtube_playlist, podcast, rss, html, hn, linkedin_profile); email uses a human-readable label/query instead';
