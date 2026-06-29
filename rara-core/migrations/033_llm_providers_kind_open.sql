-- 033_llm_providers_kind_open: kind is no longer a fixed enum of 6.
-- A provider's kind may be any litellm provider (the SPA constrains the picker
-- to the litellm catalog; the core validates non-empty/length only). Drop the
-- enum CHECK; keep the base_url-required-for-openai_compatible CHECK intact.
-- Idempotent: IF EXISTS guards a re-run, and the name is the Postgres-assigned
-- one for the single-column anonymous CHECK on kind (verified on the live DB).

ALTER TABLE llm_providers DROP CONSTRAINT IF EXISTS llm_providers_kind_check;
