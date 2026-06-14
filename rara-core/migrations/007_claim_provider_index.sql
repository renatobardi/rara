-- migrations/007_claim_provider_index.sql
-- P1a (bridge-total / rara-addon SDK): make the claim frontier index match the claim query.
--
-- Description: the worker pull (now in the rara-addon SDK, mirrored by rara-core's Store) claims with
--     SELECT ... FROM item_steps
--     WHERE capability = $1 AND assigned_provider = $2 AND status = 'pending'
--     ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
--   The claim has filtered by (capability, assigned_provider) since Phase 4 — that provider filter is
--   the isolation guarantee (a PRIVATE item routed to distill-local must never be pulled by a
--   third-party worker). But the frontier index (001's idx_item_steps_claim) only keyed on
--   (capability, id), so the assigned_provider predicate fell through to a filter. This migration
--   adds idx_item_steps_claim_provider on (capability, assigned_provider, id) WHERE status = 'pending'
--   so the index covers the full claim predicate and still gives FIFO ordering by the monotonic id.
--
--   The old idx_item_steps_claim (capability, id) is now redundant — the composite's leading column
--   still serves any capability-only scan — so it is dropped to avoid double write-amplification on
--   the hot item_steps table. Additive and idempotent (IF NOT EXISTS / IF EXISTS); no cross-agent
--   tables (the 1.0 isolation convention holds); this is rara-core's own contract table.

-- New frontier index: covers (capability, assigned_provider, status) with FIFO by id.
CREATE INDEX IF NOT EXISTS idx_item_steps_claim_provider
    ON item_steps (capability, assigned_provider, id)
    WHERE status = 'pending';

-- Retire the now-redundant capability-only frontier index.
DROP INDEX IF EXISTS idx_item_steps_claim;

COMMENT ON INDEX idx_item_steps_claim_provider IS
    'Claim frontier: backs SELECT ... WHERE capability=$1 AND assigned_provider=$2 AND status=pending ORDER BY id FOR UPDATE SKIP LOCKED (provider-isolated pull)';
