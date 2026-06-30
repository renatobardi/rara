-- 037_agents: Agent registry (CONSOLE-#10b). An agent is the Multica-style persona the 10c
-- daemon will run: name + instructions (system prompt) + a model + a set of attached skills.
--
-- Model location choice: stored as a TOP-LEVEL `model` column (the "kind/model" upstream string
-- the operator picks via the CORR-#3 provider+model picker), NOT inside a runtime_config jsonb.
-- It's the one runtime field the form edits and the field we may want to query/filter agents by,
-- so a first-class column beats burying it in jsonb. mcp_config/custom_env/custom_args stay jsonb:
-- they're free-form daemon runtime config consumed by 10c (not yet plumbed through the API, same
-- way skills.trusted preceded its daemon).

CREATE TABLE IF NOT EXISTS agents (
    id           SERIAL PRIMARY KEY,
    owner_id     INT,                              -- NULL = system-owned (tenant-ready)
    name         VARCHAR(64)  NOT NULL CHECK (btrim(name) <> ''),
    description  TEXT         NOT NULL DEFAULT '',
    avatar_url   TEXT         NOT NULL DEFAULT '',
    visibility   TEXT         NOT NULL DEFAULT 'workspace' CHECK (visibility IN ('workspace', 'private')),
    instructions TEXT         NOT NULL DEFAULT '', -- the system prompt
    model        TEXT         NOT NULL DEFAULT '', -- "kind/model" upstream (via the provider+model picker)
    mcp_config   JSONB        NOT NULL DEFAULT '{}'  CHECK (jsonb_typeof(mcp_config) = 'object'),
    custom_env   JSONB        NOT NULL DEFAULT '{}'  CHECK (jsonb_typeof(custom_env) = 'object'),
    custom_args  JSONB        NOT NULL DEFAULT '[]'  CHECK (jsonb_typeof(custom_args) = 'array'),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at   TIMESTAMPTZ
);

-- Partial unique index (precedent: 036_skills): only active rows are unique per (owner_id, name),
-- so a soft-deleted name can be reused. NULLS NOT DISTINCT makes two NULL owner_ids count as equal.
CREATE UNIQUE INDEX IF NOT EXISTS agents_active_name
    ON agents (owner_id, name) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS agents_set_updated_at ON agents;
CREATE TRIGGER agents_set_updated_at
    BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION core_set_updated_at();

-- Junction: which skills are attached to an agent. Both sides CASCADE — deleting an agent or a
-- skill drops the link, not the surviving entity. (Skill delete is soft, so the FK target is the
-- live row; a soft-deleted skill keeps its links but the API filters it out on read.)
CREATE TABLE IF NOT EXISTS agent_skills (
    agent_id   INT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_id   INT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (agent_id, skill_id)
);

-- The PK indexes (agent_id, skill_id) leading-edge, so it can't serve a lookup by skill_id alone.
-- A skill delete cascades through agent_skills.skill_id, so index it to avoid a seq scan per delete.
CREATE INDEX IF NOT EXISTS agent_skills_skill_id_idx ON agent_skills (skill_id);
