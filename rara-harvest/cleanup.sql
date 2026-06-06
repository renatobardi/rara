-- cleanup.sql
-- Script para apagar toda a estrutura de dados do Neon
-- ⚠️ CUIDADO: Este script deleta TODOS os dados

-- Drop tables em ordem reversa de dependência (foreign keys)
DROP TABLE IF EXISTS channel_videos CASCADE;
DROP TABLE IF EXISTS target_channels CASCADE;

-- Drop indexes
DROP INDEX IF EXISTS idx_videos_published_at;
DROP INDEX IF EXISTS idx_channels_youtube_id;

-- Verify cleanup
SELECT 'All tables dropped successfully' AS status;
