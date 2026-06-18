-- migrations/009_provider_runner_url.sql
-- F3 (rara-runner dispatch): give host providers a runner agent URL.
--
-- runner_url is the tailnet endpoint of the rara-runner agent on the host (VPC or Mac):
-- http://<tailnet-ip>:<port>. The dispatcher POSTs <runner_url>/run (Bearer RUNNER_AUTH_TOKEN)
-- to wake the worker via docker run. This is DISTINCT from poke_url (migr 008):
--   - poke_url   = the worker process's own poke listener (rara-addon/poke.go), POSTed by the
--                  reconciler to drain a resident that is already running.
--   - runner_url = the rara-runner agent on the host, POSTed by the dispatcher to START a
--                  container from scratch (the "portable Cloud Run").
--
-- NULL for cloudrun providers (woken via Cloud Run Jobs `run` instead) and for residents
-- that have no runner agent (rely on poll only). Additive and idempotent.

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS runner_url VARCHAR(255);

COMMENT ON COLUMN providers.runner_url IS
    'Tailnet endpoint of the rara-runner agent on this host (VPC/Mac). The dispatcher POSTs <runner_url>/run (Bearer) to wake the worker. NULL for cloudrun providers and poll-only residents.';
