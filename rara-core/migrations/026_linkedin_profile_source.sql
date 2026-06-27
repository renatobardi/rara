-- migrations/026_linkedin_profile_source.sql
-- Adds target_linkedin_profiles (rara-clip) to the unified sources_v so the Fontes UI
-- can list, create, pause/resume, tag, and delete LinkedIn profile sources.
--
-- target_linkedin_profiles is owned by rara-clip (002_target_profiles.sql). This migration
-- adds the common source columns (tags, display_name, deleted_at) IF NOT EXISTS so it is safe
-- to apply regardless of whether rara-clip's migration ran before or after this one.
-- Idempotent: ALTER TABLE … IF NOT EXISTS + CREATE OR REPLACE VIEW.

-- ---------------------------------------------------------------------------
-- 1. Ensure common source columns exist on target_linkedin_profiles.
-- ---------------------------------------------------------------------------
ALTER TABLE target_linkedin_profiles
    ADD COLUMN IF NOT EXISTS tags         text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text,
    ADD COLUMN IF NOT EXISTS deleted_at   TIMESTAMPTZ;

-- Partial index over the live set (mirrors the other source tables in migration 025).
CREATE INDEX IF NOT EXISTS target_linkedin_profiles_live_idx
    ON target_linkedin_profiles (id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- 2. Rebuild sources_v — same shape as 025, now including linkedin_profile.
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

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; deleted_at IS NULL (soft-deleted sources are hidden); status=active|paused derived from active/enabled flags';
