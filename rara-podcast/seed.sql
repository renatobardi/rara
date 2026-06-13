-- seed.sql — operator-curated podcast feeds to poll.
-- Idempotent: ON CONFLICT (feed_url) DO NOTHING. Add your feeds here and re-run.
-- The collector refreshes each feed's title from the RSS on the next run.

INSERT INTO podcast_feeds (feed_url, active) VALUES
-- ('https://feeds.example.com/your-podcast.xml', true),
('https://changelog.com/podcast/feed', true)
ON CONFLICT (feed_url) DO NOTHING;
