-- migrations/001_initial_schema.sql
-- Initial schema for rara-harvest
-- Created: 2026-06-05
-- Description: Create target_channels and channel_videos tables with indexes

-- Create target_channels table
CREATE TABLE IF NOT EXISTS target_channels (
    id SERIAL PRIMARY KEY,
    youtube_channel_id VARCHAR(255) UNIQUE NOT NULL,
    channel_name VARCHAR(255) NOT NULL,
    active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- Create channel_videos table
CREATE TABLE IF NOT EXISTS channel_videos (
    id SERIAL PRIMARY KEY,
    channel_id INT NOT NULL REFERENCES target_channels(id) ON DELETE CASCADE,
    youtube_video_id VARCHAR(50) UNIQUE NOT NULL,
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    collected_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- Create indexes for performance.
-- Note: youtube_channel_id and youtube_video_id are UNIQUE, which already
-- creates a backing index for each — no explicit index is needed for them.
CREATE INDEX IF NOT EXISTS idx_videos_published_at ON channel_videos(published_at DESC);
CREATE INDEX IF NOT EXISTS idx_videos_channel_id ON channel_videos(channel_id);

-- Add comments for documentation
COMMENT ON TABLE target_channels IS 'YouTube channels to harvest videos from';
COMMENT ON TABLE channel_videos IS 'Videos harvested from YouTube channels';
COMMENT ON COLUMN target_channels.youtube_channel_id IS 'YouTube channel ID (format: UCxxxx...)';
COMMENT ON COLUMN target_channels.active IS 'Whether this channel is actively being harvested';
COMMENT ON COLUMN channel_videos.youtube_video_id IS 'YouTube video ID (format: 11 characters)';
COMMENT ON COLUMN channel_videos.published_at IS 'When the video was published on YouTube';
COMMENT ON COLUMN channel_videos.collected_at IS 'When we harvested this video record';
