-- migrations/006_interest_profile_status.sql
-- Phase 6 (learning loop): proposed-vs-active interest_profile versions + the LLM narrative.
--
-- Description: the Phase 6 reviser appends a revised interest_profile as a NEW version with
--   status 'proposed'; it does not take effect until a human APPROVES it (the prior active
--   version is then 'superseded'). The gate cascade reads the ACTIVE version, never "the latest".
--   This migration adds:
--     - status     proposed | active | superseded (the lifecycle).
--     - narrative  the LLM-written natural-language summary (gate LLM-judge context). The
--                  deterministic engine owns the structured columns; the LLM owns only this prose.
--   and enforces AT MOST ONE active version via a partial unique index.
--
--   Backfill: the column DEFAULT 'active' marks the pre-existing v1 (seeded active) as the live
--   document. Defensive: if more than one row somehow exists, all but the highest version are
--   demoted to 'superseded' BEFORE the unique index is built, so the migration never fails on
--   legacy data. Additive and idempotent (IF NOT EXISTS + guarded constraint); no cross-agent
--   tables (the 1.0 isolation convention holds); this is rara-core's own table.

ALTER TABLE interest_profile
    ADD COLUMN IF NOT EXISTS status VARCHAR(12) NOT NULL DEFAULT 'active';

ALTER TABLE interest_profile
    ADD COLUMN IF NOT EXISTS narrative TEXT;

-- Constrain status to the known lifecycle values (guarded — no ADD CONSTRAINT IF NOT EXISTS for
-- CHECKs in Postgres), mirroring 003_item_sensitivity / 005_feedback_source_check.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'interest_profile_status_check'
    ) THEN
        ALTER TABLE interest_profile
            ADD CONSTRAINT interest_profile_status_check
            CHECK (status IN ('proposed', 'active', 'superseded'));
    END IF;
END$$;

-- Defensive: keep only the highest version active before enforcing the one-active invariant.
UPDATE interest_profile
SET status = 'superseded'
WHERE status = 'active'
  AND version <> (SELECT MAX(version) FROM interest_profile);

-- At most one active version at a time (the document in force the gate reads).
CREATE UNIQUE INDEX IF NOT EXISTS idx_interest_profile_active
    ON interest_profile (status)
    WHERE status = 'active';

COMMENT ON COLUMN interest_profile.status IS
    'proposed (a revision awaiting approval) | active (in force; at most one) | superseded';
COMMENT ON COLUMN interest_profile.narrative IS
    'LLM-written natural-language summary (gate LLM-judge context); structured columns are owned by the deterministic reviser';
