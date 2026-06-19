-- 012_provider_collect_cadence.sql
-- Adds collector scheduling to providers:
--   collect_cadence_seconds: NULL = not a collector; >0 = wake interval in seconds.
--   last_collect_at: set by the dispatcher after each successful wake; NULL = never dispatched.
--
-- The dispatcher (rara-runner) reads both columns to decide whether a collector is due, and
-- stamps last_collect_at = now() after a successful Cloud Run or host wake. The reconciler
-- (rara-core) only writes collect_cadence_seconds via UpsertProvider seeds — it never touches
-- last_collect_at (that belongs to the runner).
--
-- Also corrects two provider name mismatches introduced when coletar was auto-satisfied (and
-- never actually dispatched, so the names never mattered until now):
--   "rara-dial"          -> "dial"              (Cloud Run job = "rara-" + "dial" = rara-dial)
--   "brightdata-linkedin" -> "clip"              (Cloud Run job = "rara-" + "clip" = rara-clip)
-- IF NOT EXISTS makes both ALTERs re-runnable (already applied manually on primary).

BEGIN;

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS collect_cadence_seconds INT,
    ADD COLUMN IF NOT EXISTS last_collect_at TIMESTAMPTZ;

-- Rename provider "rara-dial" -> "dial" so the dispatcher constructs job "rara-dial"
-- (CLOUD_RUN_JOB_PREFIX="rara-" + app="dial"). No-op if already renamed.
UPDATE providers SET name = 'dial' WHERE name = 'rara-dial';

-- Rename provider "brightdata-linkedin" -> "clip" so the dispatcher constructs job "rara-clip".
-- No-op if already renamed.
UPDATE providers SET name = 'clip' WHERE name = 'brightdata-linkedin';

COMMIT;
