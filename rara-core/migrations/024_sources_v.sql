-- migrations/024_sources_v.sql
-- Read-only view that normalises all source tables into one unified shape.
-- Used by GET /v1/sources (fatia #1) for listing, filtering, and counting.
-- Lives in rara-core because the core reads all collector tables in the same Neon DB.
-- Re-creatable (CREATE OR REPLACE): idempotent, safe to re-apply.
--
-- Self-contained: adds the required columns (IF NOT EXISTS) before creating the
-- view, so it succeeds regardless of the order in which the individual collector
-- migrations (003_sources_common_columns in each collector repo) run.
-- The collector migrations provide backfill and triggers on top of what this creates.
--
-- Shape: api_id, kind, lane, display_name, tags, status, config_summary,
--        created_at, updated_at

-- ---------------------------------------------------------------------------
-- 1. Ensure common columns exist on each collector table (idempotent).
-- ---------------------------------------------------------------------------
ALTER TABLE target_channels
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text;

ALTER TABLE playlists
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text;

ALTER TABLE podcast_feeds
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text;

-- feed_sources also needs updated_at (missing from its initial schema).
ALTER TABLE feed_sources
    ADD COLUMN IF NOT EXISTS tags         text[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text,
    ADD COLUMN IF NOT EXISTS updated_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP;

-- ---------------------------------------------------------------------------
-- 2. email_sources — owned by rara-courier (002_email_sources.sql adds the seed
--    row and the updated_at trigger). This CREATE IF NOT EXISTS ensures sources_v
--    can reference the table even if courier's migration runs after this one.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_sources (
    id           SERIAL  PRIMARY KEY,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    tags         text[]  NOT NULL DEFAULT '{}',
    display_name text,
    gmail_query  text,
    label        text,
    from_filter  text,
    created_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- ---------------------------------------------------------------------------
-- 3. sources_v — unified read-only view.
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

-- News/RSS/HTML/HN sources — rara-feed
-- kind = source_type ('rss' | 'html' | 'hn'); updated_at added above.
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
FROM email_sources;

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; status=active|paused derived from active/enabled flags';
