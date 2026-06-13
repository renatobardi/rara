-- migrations/004_linkedin_posts.sql
-- Phase 5 (surface & LinkedIn): the manual-inbox domain table for the LinkedIn lane.
--
-- Description: LinkedIn is the one lane whose collector — the MANUAL INBOX — lives inside
--   rara-core's surface (a person pastes a post's URL + text through an MCP tool / HTTP
--   endpoint), so rara-core owns this domain table. It is NOT a control table; it is the
--   lane's "to-text source", read by the gate (metadata) and the extrair-linkedin worker
--   exactly as the email lane reads `emails` and the podcast lane reads `podcast_episodes`.
--
--   Swappable collector: when the Bright Data collector takes over (Phase 6) it writes this
--   SAME table behind the SAME contract — the flow, the extractor and the gates never change.
--
--   Keyed on the canonical post URL (the spine's source_ref for lane=linkedin). Additive and
--   idempotent (CREATE TABLE IF NOT EXISTS); re-applying never clobbers. Reuses rara-core's
--   namespaced trigger (core_set_updated_at), never a foreign agent's.

CREATE TABLE IF NOT EXISTS linkedin_posts (
    id         SERIAL PRIMARY KEY,
    url        TEXT         NOT NULL,                          -- canonical post URL -> items.source_ref
    author     TEXT,                                          -- post author (the gate's "channel" signal); optional
    body       TEXT         NOT NULL,                          -- the raw post text (the extrair-linkedin worker cleans it)
    created_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (url)
);

DROP TRIGGER IF EXISTS trg_linkedin_posts_updated_at ON linkedin_posts;
CREATE TRIGGER trg_linkedin_posts_updated_at
    BEFORE UPDATE ON linkedin_posts
    FOR EACH ROW
    EXECUTE FUNCTION core_set_updated_at();
