-- 013_provider_last_attempt.sql
-- Splits the "was it dispatched?" signal into two independent columns:
--   last_attempt_at     : stamped by the dispatcher on every wake attempt, regardless of whether the
--                         collector job succeeds. Drives the retry throttle (retry_interval_seconds).
--   retry_interval_seconds: minimum gap between dispatch attempts. NULL = no throttle.
--
-- last_collect_at (from 012) is now stamped by the collector itself on success, not the dispatcher.
-- This makes failure silent-no-longer: a collector that keeps failing advances last_attempt_at but
-- NOT last_collect_at, so the gap between the two grows and operators can detect stale collections.
--
-- The ListDueCollectors predicate becomes:
--   due = (last_collect_at IS NULL OR age(last_collect_at) >= cadence)
--         AND (last_attempt_at IS NULL OR retry_interval_seconds IS NULL
--              OR age(last_attempt_at) >= retry_interval_seconds)

BEGIN;

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS retry_interval_seconds INT;

COMMIT;
