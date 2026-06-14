-- migrations/001_initial_schema.sql
-- Initial schema for rara-clip (the 2.0 LinkedIn lane automated collector).
--
-- Description: rara-clip is a new, isolated agent that collects LinkedIn posts via Bright Data and
--   catalogs them into `linkedin_posts`. Like every rara agent it shares nothing but the Neon
--   database and never calls another agent — the control plane (rara-core) reads `linkedin_posts`
--   to build the items spine (lane=linkedin, source_ref=url, sensitivity=public) and the
--   `extrair-linkedin` worker reads `body` to clean it into a to-text artifact; both are
--   cross-agent SELECTs, never writes.
--
--   linkedin_posts is a CONTRACT table with TWO producers: this automated Bright Data crawl AND
--   rara-core's manual inbox (a person pastes a post through the surface). Both write the SAME
--   table behind the SAME contract, idempotent on the canonical post URL — multiple producers are
--   fine. rara-core also defines this table (its manual inbox owns the lane); this migration is the
--   self-contained, additive twin so rara-clip can be applied/deployed on its own. It is purely
--   additive and idempotent (CREATE TABLE IF NOT EXISTS, ON CONFLICT upserts): whichever agent
--   applies first creates the table; the other is a no-op. Column shape and UNIQUE(url) match.
--
--   LinkedIn content is PUBLIC — world-readable posts, so (unlike email) third-party models may
--   process them. This table is just storage; the sensitivity guarantee is rara-core's router's.

-- ---------------------------------------------------------------------------
-- updated_at trigger function — namespaced to avoid colliding with the other agents'
-- set_updated_at() variants in the shared Neon database. Functionally identical to rara-core's
-- core_set_updated_at(): both set updated_at = now(), so whichever agent's trigger is installed
-- last on the shared table behaves the same.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION clip_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- linkedin_posts — one row per collected post. Global uniqueness on the canonical post URL — the
-- same value rara-core uses as the spine's source_ref. body is the raw post text the
-- extrair-linkedin worker cleans; author is optional (the gate's "channel" signal).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS linkedin_posts (
    id         SERIAL PRIMARY KEY,
    url        TEXT         NOT NULL,                     -- canonical post URL -> items.source_ref (unique)
    author     TEXT,                                      -- post author (the gate's "channel" signal); optional
    body       TEXT         NOT NULL,                     -- raw post text the extrair-linkedin worker cleans
    created_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (url)
);

-- updated_at trigger.
DROP TRIGGER IF EXISTS trg_linkedin_posts_updated_at ON linkedin_posts;
CREATE TRIGGER trg_linkedin_posts_updated_at
    BEFORE UPDATE ON linkedin_posts
    FOR EACH ROW
    EXECUTE FUNCTION clip_set_updated_at();

COMMENT ON TABLE  linkedin_posts        IS 'One row per collected LinkedIn post; public content. Contract table written by rara-clip (Bright Data) and rara-core (manual inbox), idempotent on url';
COMMENT ON COLUMN linkedin_posts.url    IS 'Canonical post URL; the same value rara-core uses as items.source_ref';
COMMENT ON COLUMN linkedin_posts.body   IS 'Raw post text cleaned by the extrair-linkedin worker (rara-glean)';
