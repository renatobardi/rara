-- 031_llm_models: LLM model registry (alias → upstream mapping with cost and params).
-- Linked to llm_providers via provider_id FK. Workers send the alias; LiteLLM sees upstream.

CREATE TABLE IF NOT EXISTS llm_models (
    id         SERIAL PRIMARY KEY,
    owner_id   INT,
    provider_id INT NOT NULL REFERENCES llm_providers(id),
    alias       VARCHAR(64)  NOT NULL,              -- what the worker sends in LITELLM_MODEL
    upstream    VARCHAR(128) NOT NULL,              -- litellm_params.model, e.g. groq/llama-3.3-70b-versatile
    input_cost_per_token  NUMERIC(12,9) NOT NULL DEFAULT 0,
    output_cost_per_token NUMERIC(12,9) NOT NULL DEFAULT 0,
    params      JSONB        NOT NULL DEFAULT '{}', -- temperature, max_tokens, thinking…
    enabled     BOOLEAN      NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMPTZ,
    CHECK (input_cost_per_token >= 0 AND output_cost_per_token >= 0)
);

-- Partial unique index: only active (non-deleted) rows must be unique per (owner_id, alias).
-- NULLS NOT DISTINCT makes two NULL owner_ids equal (system-owned). Allows reusing a
-- soft-deleted alias without conflict.
CREATE UNIQUE INDEX IF NOT EXISTS llm_models_active_alias
    ON llm_models (owner_id, alias) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trg_llm_models_updated_at ON llm_models;
CREATE TRIGGER trg_llm_models_updated_at
    BEFORE UPDATE ON llm_models
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();
