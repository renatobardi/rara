-- migrations/001_initial_schema.sql
-- Initial schema for rara-distill
-- Description: Curate the raw transcripts produced by rara-scribe into knowledge
--   documents ready for RAG ingestion. Reads upstream (transcripts) and writes its
--   own isolated table in the same Neon database. The Kura "second brain" consumes
--   `distillations` later to build its own RAG (chunk + embed + vector index) —
--   total isolation: rara-distill never calls Kura.
--
-- "Compile once": the (expensive) LLM pass happens with the whole transcript in
-- front of it, so we capture structure now (structured + doc_context) instead of
-- flattening everything to markdown and forcing Kura to re-derive (and re-pay the
-- pass). `content` is the human version; `structured`/`doc_context` are the
-- pre-structured material for Kura's RAG/graph.

CREATE TABLE IF NOT EXISTS distillations (
    id                SERIAL PRIMARY KEY,
    youtube_video_id  VARCHAR(50),                          -- logical video key (NULL for non-youtube sources)
    source_type       VARCHAR(16)  NOT NULL,                -- 'youtube' | 'url' | 'local' (mirrors transcripts)
    source_ref        TEXT         NOT NULL,                -- watch url / page url / file path
    source_key        TEXT         NOT NULL,                -- stable, never NULL: youtube -> id; url/local -> normalized source_ref
    pattern           VARCHAR(48)  NOT NULL,                -- Fabric pattern applied (final stage), e.g. 'extract_wisdom'
    context           VARCHAR(48),                          -- injected context name, e.g. 'software-ai' (NULL = none)
    strategy          VARCHAR(48),                          -- reasoning strategy name, e.g. 'cot' (NULL = none)
    session_patterns  TEXT,                                 -- session chain as CSV (NULL = single pass)
    engine            VARCHAR(48)  NOT NULL,                -- combined 'engine/model', e.g. 'gemini/gemini-2.5-flash' (same string hashed into recipe_sha256)
    title             TEXT,                                 -- video title (from channel_videos / playlist_videos)
    content           TEXT,                                 -- curated markdown (human version)
    structured        JSONB        NOT NULL DEFAULT '{}',   -- queryable extraction for RAG/graph (shape documented in README)
    structured_status VARCHAR(16)  NOT NULL DEFAULT 'ok',   -- 'ok' | 'empty' | 'parse_failed' (extraction observability)
    doc_context       TEXT,                                 -- 1-3 sentence situational summary (Contextual Retrieval)
    metadata          JSONB,                                -- free-form extras (domain, etc.)
    source_sha256     VARCHAR(64)  NOT NULL,                -- hash of the source transcript -> staleness
    recipe_sha256     VARCHAR(64)  NOT NULL,                -- hash of the recipe (patterns+context+strategy+engine) -> staleness
    status            VARCHAR(16)  NOT NULL DEFAULT 'done', -- 'done' | 'failed'
    error             TEXT,                                 -- failure reason when status = 'failed'
    attempt_count     INT          NOT NULL DEFAULT 0,      -- consecutive failures (retried up to a cap)
    created_at        TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMPTZ  DEFAULT CURRENT_TIMESTAMP
);

-- Uniqueness that separates a session chain from a single pass and never collides
-- on a NULL id (url/local): NULLs would otherwise be treated as distinct rows.
-- Standalone 'extract_wisdom' -> (src, 'extract_wisdom'); session
-- 'summary,extract_wisdom' -> (src, 'summary,extract_wisdom'). recipe_sha256 is a
-- staleness trigger only and deliberately NOT part of the key (Option A,
-- "current view": changing context/strategy/recipe updates the same row in place).
CREATE UNIQUE INDEX IF NOT EXISTS uq_distillations_source_recipe
    ON distillations (source_key, COALESCE(session_patterns, pattern));

CREATE INDEX IF NOT EXISTS idx_distillations_status ON distillations(status);

-- Keep distillations.updated_at current on every UPDATE. Dedicated function name to
-- avoid colliding with the other agents' set_updated_at() variants in the same DB.
CREATE OR REPLACE FUNCTION distill_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_distillations_updated_at ON distillations;
CREATE TRIGGER trg_distillations_updated_at
    BEFORE UPDATE ON distillations
    FOR EACH ROW
    EXECUTE FUNCTION distill_set_updated_at();

-- Documentation
COMMENT ON TABLE distillations IS 'Curated, RAG-ready knowledge documents distilled from rara-scribe transcripts; consumed by the Kura second brain';
COMMENT ON COLUMN distillations.source_key IS 'Stable dedup key, never NULL: youtube_video_id, or normalized source_ref for url/local';
COMMENT ON COLUMN distillations.structured IS 'Queryable extraction (concepts/insights/references/connections/entities/claims)';
COMMENT ON COLUMN distillations.structured_status IS 'ok | empty | parse_failed — observability of structured extraction';
COMMENT ON COLUMN distillations.doc_context IS '1-3 sentence situational summary for Contextual Retrieval (prefixed to chunks by Kura)';
COMMENT ON COLUMN distillations.recipe_sha256 IS 'Hash of patterns+context+strategy+engine; staleness trigger, not part of the unique key';
