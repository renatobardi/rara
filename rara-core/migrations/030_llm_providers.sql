-- 030_llm_providers: LLM vendor registry with encrypted API keys.
-- key_ciphertext/key_nonce store the AES-256-GCM result; key_last4 is the
-- last 4 chars of the plaintext for masked display ("•••• xxxx") only.
-- The plaintext key is NEVER stored; decryption requires RARA_SECRETS_KEY.

CREATE TABLE IF NOT EXISTS llm_providers (
    id         SERIAL PRIMARY KEY,
    owner_id   INT,                    -- NULL = system-owned (tenant-ready)
    name       VARCHAR(48)  NOT NULL,
    kind       VARCHAR(24)  NOT NULL,  -- groq|gemini|anthropic|openai|deepseek|openai_compatible
    base_url   TEXT,                   -- required only for kind='openai_compatible'
    key_ciphertext BYTEA,
    key_nonce      BYTEA,
    key_last4      VARCHAR(8),
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMPTZ,
    CHECK (kind IN ('groq','gemini','anthropic','openai','deepseek','openai_compatible')),
    CHECK (kind != 'openai_compatible' OR base_url IS NOT NULL)
);

-- Partial unique index: only active (non-deleted) rows must be unique per
-- (owner_id, name), so a soft-deleted name can be reused without conflict.
-- NULLS NOT DISTINCT makes two NULL owner_ids count as equal (system-owned).
CREATE UNIQUE INDEX IF NOT EXISTS llm_providers_active_name
    ON llm_providers (owner_id, name) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trg_llm_providers_updated_at ON llm_providers;
CREATE TRIGGER trg_llm_providers_updated_at
    BEFORE UPDATE ON llm_providers
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();
