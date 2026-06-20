-- 015_provider_worker_not_null.sql
-- Promotes providers.worker to NOT NULL (Option B, slice E5a).
--
-- Migration 014 added the column and backfilled it from name (stripping "-local").
-- UpsertProvider now defaults worker = name when the caller omits it (Go guard added
-- in the same slice), so no new rows will be NULL going forward.
--
-- Backfill covers any rows that slipped through before the guard existed, and the
-- empty-string case that the NOT NULL constraint alone would not catch.

BEGIN;

-- Safety backfill: idempotent, covers NULL and the empty-string edge case.
-- NULLIF(worker, '') returns NULL when worker is NULL or '', so IS NULL catches both.
UPDATE providers
   SET worker = name
 WHERE NULLIF(worker, '') IS NULL;

-- No-op if the column is already NOT NULL (e.g. re-running on a fresh schema).
ALTER TABLE providers ALTER COLUMN worker SET NOT NULL;

COMMIT;
