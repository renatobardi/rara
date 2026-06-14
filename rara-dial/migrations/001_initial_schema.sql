-- migrations/001_initial_schema.sql
-- Initial schema for rara-dial (the 2.0 Podcast lane collector).
--
-- Description: rara-dial is a new, isolated agent that discovers podcast episodes from RSS
--   feeds and catalogs them. Like every rara agent it owns ONLY its domain tables and shares
--   nothing but the Neon database — no foreign keys across the agent boundary. The control
--   plane (rara-core) reads podcast_episodes to build the items spine (lane=podcast,
--   source_ref=guid) and the asr-direct-audio worker reads enclosure_url to transcribe the
--   episode; both are cross-agent SELECTs, never writes.
--
--   Idempotent (CREATE TABLE IF NOT EXISTS, ON CONFLICT upserts): re-applying never clobbers.

-- ---------------------------------------------------------------------------
-- updated_at trigger function — namespaced to avoid colliding with the other agents'
-- set_updated_at() variants in the shared Neon database.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION podcast_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- podcast_feeds — the RSS feeds to poll. One row per feed, operator-curated. `active`
-- toggles a feed off without deleting its episodes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS podcast_feeds (
    id         SERIAL PRIMARY KEY,
    feed_url   TEXT         NOT NULL,                     -- the RSS feed URL (unique)
    title      TEXT,                                      -- channel title, refreshed from the feed
    active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (feed_url)
);

-- ---------------------------------------------------------------------------
-- podcast_episodes — one row per episode (an RSS <item> with an audio enclosure). Global
-- uniqueness on guid (the RSS item's stable id) — the same id rara-core uses as the spine's
-- source_ref. enclosure_url is the direct CDN audio URL the asr-direct-audio worker transcribes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS podcast_episodes (
    id            SERIAL PRIMARY KEY,
    feed_id       INT          NOT NULL REFERENCES podcast_feeds(id) ON DELETE CASCADE,
    guid          TEXT         NOT NULL,                  -- RSS item GUID; spine source_ref (unique)
    title         TEXT         NOT NULL DEFAULT '',
    enclosure_url TEXT         NOT NULL,                  -- direct audio URL (the enclosure)
    published_at  TIMESTAMPTZ,
    status        VARCHAR(16)  NOT NULL DEFAULT 'new',    -- collector-side status (new | ...)
    collected_at  TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (guid)
);

CREATE INDEX IF NOT EXISTS idx_podcast_episodes_feed_id ON podcast_episodes(feed_id);
CREATE INDEX IF NOT EXISTS idx_podcast_episodes_published_at ON podcast_episodes(published_at DESC);

-- updated_at triggers.
DROP TRIGGER IF EXISTS trg_podcast_feeds_updated_at ON podcast_feeds;
CREATE TRIGGER trg_podcast_feeds_updated_at
    BEFORE UPDATE ON podcast_feeds
    FOR EACH ROW EXECUTE FUNCTION podcast_set_updated_at();

DROP TRIGGER IF EXISTS trg_podcast_episodes_updated_at ON podcast_episodes;
CREATE TRIGGER trg_podcast_episodes_updated_at
    BEFORE UPDATE ON podcast_episodes
    FOR EACH ROW EXECUTE FUNCTION podcast_set_updated_at();

COMMENT ON TABLE  podcast_feeds          IS 'RSS feeds to poll for podcast episodes (operator-curated)';
COMMENT ON TABLE  podcast_episodes       IS 'One row per podcast episode (RSS item with an audio enclosure)';
COMMENT ON COLUMN podcast_episodes.guid  IS 'RSS item GUID; the same id rara-core uses as items.source_ref';
COMMENT ON COLUMN podcast_episodes.enclosure_url IS 'Direct CDN audio URL transcribed by the asr-direct-audio worker';
