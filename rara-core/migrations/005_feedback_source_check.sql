-- migrations/005_feedback_source_check.sql
-- Phase 6 (RARA<->KURA contract): constrain feedback.source to the known learning signals,
-- admitting the new `kura_implicit` value.
--
-- Description: KURA closes the learning loop by calling the EXISTING control surface
--   (rara_feedback_distillation / POST /v1/feedback/distillation) when you engage with or
--   dismiss a distilled doc; those land in `feedback` with source = 'kura_implicit', distinct
--   from 'user_explicit' (manual thumbs) and 'quarantine_review' (human review of a deferred
--   item). See KURA-CONTRACT.md §2.
--
--   This is the ONLY rara change the contract requires: admit the new enum value. rara stays
--   KURA-agnostic — KURA reads `distillations` on its own and pushes nothing else here.
--
--   feedback.source was previously free-text; this adds a CHECK pinning it to the three known
--   sources. Existing rows only ever carried 'user_explicit'/'quarantine_review' (the Go
--   writers), so the constraint is satisfied by all current data. Guard the ADD CONSTRAINT so
--   re-applying the migration is a no-op (Postgres has no ADD CONSTRAINT IF NOT EXISTS for
--   CHECKs), mirroring 003_item_sensitivity. Additive and idempotent; no cross-agent tables.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'feedback_source_check'
    ) THEN
        ALTER TABLE feedback
            ADD CONSTRAINT feedback_source_check
            CHECK (source IN ('user_explicit', 'quarantine_review', 'kura_implicit'));
    END IF;
END$$;

COMMENT ON COLUMN feedback.source IS
    'user_explicit (manual thumbs) | quarantine_review (human review of a defer) | kura_implicit (KURA engagement, per KURA-CONTRACT.md)';
