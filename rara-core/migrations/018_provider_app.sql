-- 018_provider_app.sql
-- P1a: targeting column that decouples the placement name from the deploy artifact
-- (Cloud Run job + runner allowlist). Dispatcher will target job = jobPrefix + app
-- instead of prov.Name in P1a-2 (runner); until then app == name (no behaviour change).
ALTER TABLE providers ADD COLUMN IF NOT EXISTS app VARCHAR(48);

-- Idempotent backfill: seed app = name for every existing row that hasn't been set.
UPDATE providers
    SET app = name
    WHERE app IS NULL;
