CREATE TABLE target_channels (
    id SERIAL PRIMARY KEY,
    youtube_channel_id VARCHAR(255) UNIQUE NOT NULL,
    channel_name VARCHAR(255) NOT NULL,
    active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE channel_videos (
    id SERIAL PRIMARY KEY,
    channel_id INT REFERENCES target_channels(id) ON DELETE CASCADE,
    youtube_video_id VARCHAR(50) UNIQUE NOT NULL,
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    collected_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_videos_published_at ON channel_videos(published_at);
CREATE INDEX idx_channels_youtube_id ON target_channels(youtube_channel_id);
