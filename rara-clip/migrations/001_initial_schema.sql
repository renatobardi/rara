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
-- linkedin_posts — one row per collected post. Global uniqueness on the canonical post URL — the
-- same value rara-core uses as the spine's source_ref. body is the raw post text the
-- extrair-linkedin worker cleans; author is optional (the gate's "channel" signal).
--
-- This is the additive twin of rara-core/migrations/004_linkedin_posts.sql: it only GUARANTEES the
-- shared contract table exists (CREATE TABLE IF NOT EXISTS) so rara-clip can be applied/deployed on
-- its own. The updated_at trigger is a SHARED mutable object, so it has a single owner — rara-core,
-- which originated the table for the manual inbox — and is deliberately NOT (re)defined here. If
-- both agents installed a trigger of the same name bound to their own function, the apply order
-- would silently rebind it (and a later DROP FUNCTION ... CASCADE could drop the trigger out from
-- under the other producer). created_at/updated_at default to now() on insert, so rows are correct
-- even if rara-clip's migration runs first; rara-core's migration owns ON-UPDATE bumping.
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

COMMENT ON TABLE  linkedin_posts        IS 'One row per collected LinkedIn post; public content. Contract table written by rara-clip (Bright Data) and rara-core (manual inbox), idempotent on url';
COMMENT ON COLUMN linkedin_posts.url    IS 'Canonical post URL; the same value rara-core uses as items.source_ref';
COMMENT ON COLUMN linkedin_posts.body   IS 'Raw post text cleaned by the extrair-linkedin worker (rara-glean)';
