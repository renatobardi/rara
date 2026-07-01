-- 039_agent_executor: CLI vs gateway executor tag (CONSOLE-#10c2a). v1 = 'cli' for all agents;
-- 'gateway' is reserved for the 10c2b harness. DEFAULT 'cli' back-fills existing rows.
ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS executor TEXT NOT NULL DEFAULT 'cli'
        CHECK (executor IN ('cli', 'gateway'));
