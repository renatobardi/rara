-- migrations/011_provider_env_object.sql
-- Follow-up to 010: constrain providers.env to a JSON OBJECT.
--
-- env is injected into the worker's environment key=value when the dispatcher wakes it, so it must
-- be an object. JSONB also accepts arrays/scalars/null; one of those would break key iteration and
-- silently prevent a wake. This CHECK fails the bad write at the boundary instead. The seed and the
-- surface only ever write objects (default '{}'), so every existing row already satisfies it.
--
-- Idempotent: DROP IF EXISTS then ADD (Postgres has no ADD CONSTRAINT IF NOT EXISTS), so re-running
-- converges. NOT VALID is unnecessary — '{}' default means no existing row violates it.

ALTER TABLE providers DROP CONSTRAINT IF EXISTS providers_env_is_object;
ALTER TABLE providers
    ADD CONSTRAINT providers_env_is_object CHECK (jsonb_typeof(env) = 'object');
