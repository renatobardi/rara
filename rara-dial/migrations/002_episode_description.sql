-- migrations/002_episode_description.sql
-- Additive: add a description column to podcast_episodes so the dial collector can persist
-- the episode show-notes (<itunes:summary> preferred over <description>). Used by the core
-- surface display join (title/channel/summary) — the column is nullable so episodes collected
-- before this migration keep their existing rows intact.
--
-- Idempotent (IF NOT EXISTS): safe to re-apply or run while the collector is live.

ALTER TABLE podcast_episodes
    ADD COLUMN IF NOT EXISTS description TEXT;
