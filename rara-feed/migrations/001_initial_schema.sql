-- migrations/001_initial_schema.sql
-- Initial schema for rara-feed
-- Description: Collect AI/ML news items from RSS feeds, Hacker News (Algolia) and
--   HTML pages into news_items, an upstream source the rara-distill work-queue reads
--   (status='ready'). Isolated from the other agents: its own tables in the same Neon
--   database. The work queue is the set of enabled rows in feed_sources.

-- feed_sources is the work queue: one row per place to discover items from.
CREATE TABLE IF NOT EXISTS feed_sources (
    id             SERIAL PRIMARY KEY,
    name           VARCHAR(64)  NOT NULL,                    -- 'OpenAI','Anthropic','Hacker News'...
    source_type    VARCHAR(8)   NOT NULL,                    -- 'rss' | 'html' | 'hn'
    endpoint       TEXT         NOT NULL,                    -- feed/page url, OR HN search term
    cls            VARCHAR(24)  NOT NULL,                    -- badge carried onto the item, e.g. 'b-openai'
    fetch_strategy VARCHAR(12)  NOT NULL DEFAULT 'http',     -- 'http' | 'unlocker' (v1 honours only http)
    parser         VARCHAR(24),                              -- NULL = generic extractor; name = bespoke (reserved)
    enabled        BOOLEAN      NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (name, endpoint)
);

-- news_items is one row per discovered item; url is the natural dedup key.
CREATE TABLE IF NOT EXISTS news_items (
    id             SERIAL PRIMARY KEY,
    source         VARCHAR(64)  NOT NULL,                    -- readable source name
    cls            VARCHAR(24)  NOT NULL,                    -- badge (carried to publish/artifact)
    source_type    VARCHAR(8)   NOT NULL,                    -- 'rss' | 'html' | 'hn'
    url            TEXT         NOT NULL,                    -- natural key (HN: permalink when no external url)
    title          TEXT,
    published_at   TIMESTAMPTZ,                              -- pubDate / created_at_i / parsed date
    excerpt        TEXT,                                     -- source-provided excerpt (RSS desc / snippet)
    body           TEXT,                                     -- extracted full text (NULL = not captured)
    fetch_status   VARCHAR(8)   NOT NULL DEFAULT 'excerpt',  -- 'full' | 'excerpt' | 'failed' (coverage observability)
    content_sha256 VARCHAR(64)  NOT NULL,                    -- hash of title + COALESCE(body,excerpt) -> staleness
    status         VARCHAR(8)   NOT NULL DEFAULT 'ready',    -- 'ready' (for distill) | 'failed' (no usable text)
    error          TEXT,                                     -- failure reason when status = 'failed'
    attempt_count  INT          NOT NULL DEFAULT 0,          -- consecutive failures (reserved; symmetry with other agents)
    created_at     TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (url)
);

CREATE INDEX IF NOT EXISTS idx_news_items_status ON news_items(status);
CREATE INDEX IF NOT EXISTS idx_news_items_published ON news_items(published_at);

-- Keep news_items.updated_at current on every UPDATE. Dedicated function name to
-- avoid colliding with the other agents' set_updated_at() variants in the same DB.
CREATE OR REPLACE FUNCTION feed_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_news_items_updated_at ON news_items;
CREATE TRIGGER trg_news_items_updated_at
    BEFORE UPDATE ON news_items
    FOR EACH ROW
    EXECUTE FUNCTION feed_set_updated_at();

-- SEED (RSS audit re-verified jun/2026): HF, GitHub, Import AI have official RSS →
-- migrated from html to rss. Anthropic, Mistral, Cognition, Cursor, SemiAnalysis
-- stay html (no official RSS). html sources start 'http' (cheap attempt); the
-- runtime-block check flips blocked ones to 'unlocker' via a data UPDATE (no redeploy).
INSERT INTO feed_sources (name, source_type, endpoint, cls, fetch_strategy, enabled) VALUES
('OpenAI',          'rss',  'https://openai.com/news/rss.xml',      'b-openai',    'http', true),
('Google DeepMind', 'rss',  'https://deepmind.google/blog/rss.xml', 'b-deepmind',  'http', true),
('Hugging Face',    'rss',  'https://huggingface.co/blog/feed.xml', 'b-hf',        'http', true),
('GitHub',          'rss',  'https://github.blog/ai-and-ml/feed/',  'b-github',    'http', true),
('Import AI',       'rss',  'https://importai.substack.com/feed',   'b-import',    'http', true),
('Anthropic',       'html', 'https://www.anthropic.com/news',       'b-anthropic', 'http', true),
('Mistral',         'html', 'https://mistral.ai/news',              'b-mistral',   'http', true),
('Cognition',       'html', 'https://cognition.ai/blog',            'b-cognition', 'http', true),
('Cursor',          'html', 'https://cursor.com/blog',              'b-cursor',    'http', true),
('SemiAnalysis',    'html', 'https://semianalysis.com',             'b-semi',      'http', true),
('Hacker News', 'hn', 'Anthropic', 'b-hn', 'http', true),
('Hacker News', 'hn', 'OpenAI',    'b-hn', 'http', true),
('Hacker News', 'hn', 'Mistral',   'b-hn', 'http', true),
('Hacker News', 'hn', 'DeepMind',  'b-hn', 'http', true),
('Hacker News', 'hn', 'Devin',     'b-hn', 'http', true),
('Hacker News', 'hn', 'xAI',       'b-hn', 'http', true),
('Hacker News', 'hn', 'Meta AI',   'b-hn', 'http', true),
('Hacker News', 'hn', 'DeepSeek',  'b-hn', 'http', true),
('Hacker News', 'hn', 'Qwen',      'b-hn', 'http', true),
('Hacker News', 'hn', 'Cursor',    'b-hn', 'http', true),
('Hacker News', 'hn', 'Kimi',      'b-hn', 'http', true),
('Hacker News', 'hn', 'Moonshot',  'b-hn', 'http', true)
ON CONFLICT (name, endpoint) DO NOTHING;

-- Documentation
COMMENT ON TABLE feed_sources IS 'Work queue for rara-feed: enabled rows are the sources to crawl';
COMMENT ON TABLE news_items IS 'AI/ML news items collected from RSS/HN/HTML; upstream for the rara-distill work-queue';
COMMENT ON COLUMN news_items.url IS 'Natural dedup key; HN stories without an external url use the item permalink';
COMMENT ON COLUMN news_items.fetch_status IS 'full | excerpt | failed — full-text coverage observability';
COMMENT ON COLUMN news_items.content_sha256 IS 'Hash of title + COALESCE(body,excerpt); staleness trigger for re-runs';
COMMENT ON COLUMN news_items.status IS 'ready (eligible for distill) | failed (no usable text)';
