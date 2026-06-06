-- migrations/001_initial_schema.sql
-- Initial schema for rara-shelf
-- Description: Catalogue the user's own playlists and the videos in each one.
--   Isolated from rara-harvest (separate tables in the same Neon database).

-- Playlists owned by the authenticated user.
CREATE TABLE IF NOT EXISTS playlists (
    id SERIAL PRIMARY KEY,
    youtube_playlist_id VARCHAR(64) UNIQUE NOT NULL,
    title TEXT NOT NULL,
    description TEXT,
    privacy_status VARCHAR(20),          -- public | unlisted | private
    item_count INT,
    active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- Videos belonging to a playlist. The SAME video can appear in MANY playlists,
-- so uniqueness is the composite (playlist_id, youtube_video_id) — unlike
-- rara-harvest where youtube_video_id is globally unique.
CREATE TABLE IF NOT EXISTS playlist_videos (
    id SERIAL PRIMARY KEY,
    playlist_id INT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    youtube_video_id VARCHAR(50) NOT NULL,
    title TEXT NOT NULL DEFAULT '',   -- deleted/private items may return empty string; NOT NULL with default avoids false NULLs
    url TEXT NOT NULL,
    published_at TIMESTAMPTZ,             -- nullable: private/deleted items lack it
    position INT,                         -- order within the playlist
    collected_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (playlist_id, youtube_video_id)
);

CREATE INDEX IF NOT EXISTS idx_playlist_videos_playlist_id ON playlist_videos(playlist_id);
CREATE INDEX IF NOT EXISTS idx_playlist_videos_published_at ON playlist_videos(published_at DESC);

-- Keep playlists.updated_at current on every UPDATE. Dedicated function name
-- to avoid colliding with rara-harvest's set_updated_at() in the same database.
CREATE OR REPLACE FUNCTION shelf_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_playlists_updated_at ON playlists;
CREATE TRIGGER trg_playlists_updated_at
    BEFORE UPDATE ON playlists
    FOR EACH ROW
    EXECUTE FUNCTION shelf_set_updated_at();

-- Documentation
COMMENT ON TABLE playlists IS 'YouTube playlists owned by the authenticated user';
COMMENT ON TABLE playlist_videos IS 'Videos catalogued per playlist (composite-unique)';
COMMENT ON COLUMN playlists.privacy_status IS 'public | unlisted | private';
COMMENT ON COLUMN playlist_videos.position IS 'Order of the video within the playlist';
COMMENT ON COLUMN playlist_videos.published_at IS 'Video publish date; null for deleted/private items';
