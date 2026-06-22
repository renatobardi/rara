-- Flip gate providers from legacy apps (gate-barato*, gate-rico*) to the consolidated rara-gate
-- job/image. Workers 'sift' and 'assay' cover the 4 placements:
--   sift-cloud, sift-vpc, assay-cloud, assay-vpc.
-- Idempotent: re-running is a no-op (UPDATE on already-'gate' rows changes nothing).
UPDATE providers SET app = 'gate' WHERE worker IN ('sift', 'assay') AND app != 'gate';
