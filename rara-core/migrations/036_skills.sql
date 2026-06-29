-- 036_skills: Agent Skill registry (CONSOLE-#10a).
-- A skill is a bundle: SKILL.md (skills.content) + supporting files (skill_files).
-- This is the base of the hybrid skill+script the 10c2 daemon executes — the SKILL.md
-- instructs, the files land in the workdir. An imported skill is born trusted=false;
-- the daemon only runs scripts from a trusted skill (operator gates trust in the UI),
-- which blocks arbitrary code execution from a third-party skill.

CREATE TABLE IF NOT EXISTS skills (
    id          SERIAL PRIMARY KEY,
    owner_id    INT,                              -- NULL = system-owned (tenant-ready)
    name        VARCHAR(64)  NOT NULL CHECK (btrim(name) <> ''),
    description TEXT         NOT NULL DEFAULT '',
    content     TEXT         NOT NULL DEFAULT '', -- the SKILL.md body
    config      JSONB        NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
    trusted     BOOLEAN      NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMPTZ
);

-- Partial unique index (precedent: 030_llm_providers): only active rows are unique per
-- (owner_id, name), so a soft-deleted name can be reused. NULLS NOT DISTINCT makes two
-- NULL owner_ids count as equal (system-owned).
CREATE UNIQUE INDEX IF NOT EXISTS skills_active_name
    ON skills (owner_id, name) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS skills_set_updated_at ON skills;
CREATE TRIGGER skills_set_updated_at
    BEFORE UPDATE ON skills
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

CREATE TABLE IF NOT EXISTS skill_files (
    id         SERIAL PRIMARY KEY,
    skill_id   INT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (skill_id, path),
    -- Defense in depth (the core validates too): a bundle path must be a relative path with no
    -- traversal, absolute prefix, or drive/ADS colon, so it can't escape the daemon's workdir.
    -- 'SKILL.md' is reserved — that's skills.content, not a bundle file.
    CHECK (btrim(path) <> '' AND path !~ '(^[/\\])|(\.\.)|:' AND lower(path) <> 'skill.md')
);

DROP TRIGGER IF EXISTS skill_files_set_updated_at ON skill_files;
CREATE TRIGGER skill_files_set_updated_at
    BEFORE UPDATE ON skill_files
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();
