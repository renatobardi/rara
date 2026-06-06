-- cleanup.sql
-- Script para apagar toda a estrutura de dados do Neon
-- ⚠️ CUIDADO: Este script deleta TODOS os dados

-- Drop tables em ordem reversa de dependência (foreign keys).
-- DROP TABLE ... CASCADE already removes the tables' indexes and triggers,
-- so no separate DROP INDEX is needed.
DROP TABLE IF EXISTS channel_videos CASCADE;
DROP TABLE IF EXISTS target_channels CASCADE;

-- Drop the shared trigger function (no longer referenced once tables are gone).
DROP FUNCTION IF EXISTS set_updated_at();

-- Verify cleanup
SELECT 'All tables dropped successfully' AS status;
