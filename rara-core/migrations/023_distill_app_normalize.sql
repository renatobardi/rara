-- Normalize distill-vpc provider: set app='distill' to match distill-cloud.
-- Before P1a the app column was backfilled from name, so distill-vpc ended up with app='distill-local'.
-- Both distill placements should target the same job/image (rara-distill) via app='distill'.
-- Idempotent: re-running is a no-op.
UPDATE providers
SET    app = 'distill'
WHERE  worker = 'distill'
  AND  app != 'distill';
