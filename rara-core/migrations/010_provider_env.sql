-- migrations/010_provider_env.sql
-- feat/provider-env-column: give every provider a per-run env config so the dispatcher
-- (rara-runner) can inject the same config on EVERY host, not just baked into the Cloud Run deploy.
--
-- env is the per-run NON-secret config the worker image reads from its environment — the identity
-- it claims as (e.g. SIFT_GATE + SIFT_PROVIDER for sift, DISTILL_PROVIDER for distill) and policy
-- knobs that are config, not secrets (e.g. LITELLM_MODEL). The dispatcher merges this into the
-- container env when it wakes the worker (jobs:run on GCP, docker run on VPC/Mac).
--
-- SECRETS DO NOT BELONG HERE: DATABASE_URL, API keys, bearer tokens are resolved by the host/agent
-- (Secret Manager / the runner's own config), never trusted from this row. env is operator-curated
-- plaintext config that ships in the clear.
--
-- Additive and idempotent (ADD COLUMN IF NOT EXISTS); NOT NULL DEFAULT '{}' so every existing
-- provider gets an empty config and nothing reading it has to handle NULL. No cross-agent tables
-- (the 1.0 isolation convention holds); this is rara-core's own control table.

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS env JSONB NOT NULL DEFAULT '{}';

COMMENT ON COLUMN providers.env IS
    'Per-run NON-secret config the worker reads from the environment (identity keys like SIFT_GATE/SIFT_PROVIDER/DISTILL_PROVIDER + policy knobs like LITELLM_MODEL). The dispatcher injects this on wake. Secrets (DATABASE_URL, API keys) are NOT here — they are resolved by the host/agent.';
