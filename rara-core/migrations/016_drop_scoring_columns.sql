-- 016_drop_scoring_columns.sql
-- Routing is now purely order + health + constraints (no score).
-- Drop the columns that became inert after the score-free router landed.
ALTER TABLE providers        DROP COLUMN IF EXISTS cost,
                             DROP COLUMN IF EXISTS quality,
                             DROP COLUMN IF EXISTS latency_ms;
ALTER TABLE routing_policies DROP COLUMN IF EXISTS cost_weight,
                             DROP COLUMN IF EXISTS quality_weight;
