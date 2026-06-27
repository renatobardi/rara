-- migrations/002_target_profiles.sql
-- Target LinkedIn profiles: the operator-managed list of profiles the Bright Data crawler
-- collects posts from. Replaces the BRIGHTDATA_LINKEDIN_URLS env-var approach so profiles
-- are managed through the console's Fontes UI (same pattern as target_channels in rara-harvest).
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + UNIQUE (profile_url).

CREATE TABLE IF NOT EXISTS target_linkedin_profiles (
    id          SERIAL      PRIMARY KEY,
    profile_url TEXT        NOT NULL,
    active      BOOLEAN     NOT NULL DEFAULT TRUE,
    deleted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (profile_url)
);

COMMENT ON TABLE  target_linkedin_profiles             IS 'LinkedIn profiles the Bright Data crawler collects posts from; managed via Fontes UI';
COMMENT ON COLUMN target_linkedin_profiles.profile_url IS 'Canonical LinkedIn profile URL (e.g. https://www.linkedin.com/in/handle)';
COMMENT ON COLUMN target_linkedin_profiles.active      IS 'When false, profile is paused but not deleted';
COMMENT ON COLUMN target_linkedin_profiles.deleted_at  IS 'Soft-delete timestamp; NULL = not deleted';
