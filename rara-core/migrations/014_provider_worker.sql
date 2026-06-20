-- 014_provider_worker.sql
-- Promotes the logical worker name to a first-class column (Option B, slice E1).
--
-- Background: providers.name encodes (capability × host) by convention, e.g.
--   "distill"       — Cloud Run placement
--   "distill-local" — VPC/Mac placement
-- The worker column groups placements of the same binary so the console and the
-- control plane can reason about workers, not just individual provider rows.
-- Backfill rule: worker = name with the "-local" suffix stripped.
-- Pairs collapse (distill + distill-local → worker "distill"); standalone providers
-- keep their own name (asr-youtube → worker "asr-youtube").
--
-- The column is left nullable for now. Every UpsertProvider call stamps it going
-- forward; a NOT NULL constraint can be added in a future cleanup migration once
-- any pre-existing rows without a value have been addressed by this backfill.

BEGIN;

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS worker VARCHAR(48);

-- Idempotent backfill: only touch rows that are still NULL. Safe to re-run because
-- a newly added column starts as NULL; on second run all rows already have a value.
UPDATE providers
   SET worker = regexp_replace(name, '-local$', '')
 WHERE worker IS NULL;

COMMIT;
