-- migrations/003_item_sensitivity.sql
-- Phase 4 (new lanes): item-level content sensitivity for sensitivity-constrained routing.
--
-- Description: the email lane carries PRIVATE content. The router must never send a private
--   item to a third-party model — only local/self-host providers may process it. Sensitivity
--   is an attribute of the ITEM (stamped at discovery and frozen), checked by the router
--   against a provider's {"sensitivity":"third_party"} tag (see rara-core/router.go
--   constraintsSatisfied). Public is the default, so every existing YouTube/podcast item is
--   unaffected and YouTube routing is unchanged.
--
--   Additive and idempotent (ADD COLUMN IF NOT EXISTS): re-applying never clobbers, and the
--   DEFAULT 'public' backfills existing rows. No cross-agent tables (the 1.0 isolation
--   convention holds); this is rara-core's own spine table.

ALTER TABLE items
    ADD COLUMN IF NOT EXISTS sensitivity VARCHAR(8) NOT NULL DEFAULT 'public';

-- Constrain to the known values. Guard the ADD CONSTRAINT so re-applying the migration is a
-- no-op (Postgres has no ADD CONSTRAINT IF NOT EXISTS for CHECKs).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'items_sensitivity_check'
    ) THEN
        ALTER TABLE items
            ADD CONSTRAINT items_sensitivity_check
            CHECK (sensitivity IN ('public', 'private'));
    END IF;
END$$;

COMMENT ON COLUMN items.sensitivity IS
    'public (default) | private; stamped at discovery (email -> private) and frozen. The router excludes third-party providers for private items.';
