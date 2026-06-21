-- 017_provider_last_error.sql
-- Observability: surface the most recent dispatch failure so operators can see why a
-- provider last failed without digging through runner logs (CONSOLE-WORKERS P0c).
-- Written by the runner on a failed wake attempt (P0d); never touched by seed/upsert.
ALTER TABLE providers ADD COLUMN IF NOT EXISTS last_error TEXT;
