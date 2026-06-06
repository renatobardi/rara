-- seed.sql
-- Seed data for rara-harvest (test/development)
-- Usage: psql $DATABASE_URL -f seed.sql

-- Insert sample YouTube channels
INSERT INTO target_channels (youtube_channel_id, channel_name, active) VALUES
    ('UCBJyCSXCurz-IsQaRuHZDHw', 'TED-Ed', true),
    ('UCkRfArvrzheW2E7b6SVV2vA', 'Kurzgesagt', true),
    ('UC1-mCCDPeKLwP1zrKOZzuXQ', 'VSauce', true),
    ('UC8v2FQL9QSiNQXD_26D6RPw', 'The Great War', false),
    ('UCVHFbqXqoYvEWM1Ddxl0QDg', 'CrashCourse', true)
ON CONFLICT (youtube_channel_id) DO NOTHING;

-- Insert sample videos
INSERT INTO channel_videos (channel_id, youtube_video_id, title, url, published_at) VALUES
    (1, 'dQw4w9WgXcQ', 'Why Do We Dream?', 'https://www.youtube.com/watch?v=dQw4w9WgXcQ', NOW() - INTERVAL '7 days'),
    (1, 'N7MjXS0sOv8', 'How Do Memories Form?', 'https://www.youtube.com/watch?v=N7MjXS0sOv8', NOW() - INTERVAL '5 days'),
    (2, 'JTxXXzKmDdA', 'The Universe in a Nutshell', 'https://www.youtube.com/watch?v=JTxXXzKmDdA', NOW() - INTERVAL '3 days'),
    (3, 'z5bXoFIkjuo', 'What Is DNA?', 'https://www.youtube.com/watch?v=z5bXoFIkjuo', NOW() - INTERVAL '10 days'),
    (5, 'RrC82hJ0F4Q', 'Revolutions', 'https://www.youtube.com/watch?v=RrC82hJ0F4Q', NOW() - INTERVAL '2 days')
ON CONFLICT (youtube_video_id) DO NOTHING;

-- Display summary
SELECT
    (SELECT COUNT(*) FROM target_channels) as total_channels,
    (SELECT COUNT(*) FROM target_channels WHERE active = true) as active_channels,
    (SELECT COUNT(*) FROM channel_videos) as total_videos;
