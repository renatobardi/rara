-- migrations/008_provider_poke_url.sql
-- P1b (real Activators): give resident providers a poke address for symmetric activation.
--
-- Description: trabalho = pull always; ativação = symmetric. When the reconciler assigns a step to a
--   RESIDENT provider (the Mac scribe, a VPC worker) it pokes that worker over the tailnet to drain
--   NOW instead of waiting for the next poll tick (the slow poll stays as the safety net — a poke is
--   best-effort and never wakes a sleeping Mac). The poke target is per-provider, so it lives on the
--   provider row: poke_url is the worker's tailnet endpoint (e.g. http://mac.tailnet:7700), and the
--   reconciler POSTs <poke_url>/poke (Bearer POKE_AUTH_TOKEN) to it.
--
--   on_demand cloudrun providers do NOT use this: they are woken via the Cloud Run Jobs `run` API
--   (project/region/credentials from env, job named after the provider), so poke_url stays NULL for
--   them. A resident with a NULL/empty poke_url is simply not poked — the slow poll still drains it.
--
--   Additive and idempotent (ADD COLUMN IF NOT EXISTS); NULL by default so every existing provider is
--   unaffected. No cross-agent tables (the 1.0 isolation convention holds); this is rara-core's own
--   control table.

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS poke_url VARCHAR(255);

COMMENT ON COLUMN providers.poke_url IS
    'Resident worker tailnet endpoint for symmetric activation; the reconciler POSTs <poke_url>/poke (Bearer). NULL for on_demand cloudrun providers (woken via Cloud Run Jobs run instead).';
