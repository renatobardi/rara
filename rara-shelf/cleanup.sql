-- cleanup.sql — drops the entire rara-shelf data structure.
-- ⚠️ Destructive: deletes ALL playlist data.

-- DROP TABLE ... CASCADE removes the tables' indexes and triggers too.
DROP TABLE IF EXISTS playlist_videos CASCADE;
DROP TABLE IF EXISTS playlists CASCADE;

-- Drop the shelf trigger function once the tables are gone.
DROP FUNCTION IF EXISTS shelf_set_updated_at();

SELECT 'rara-shelf tables dropped successfully' AS status;
