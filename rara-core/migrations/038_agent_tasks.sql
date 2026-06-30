-- 038_agent_tasks: the single execution primitive of the agents module (CONSOLE-#10c1). Every
-- surface that runs an agent — free-form task, distillation quick-run, board — only ever creates
-- and reads rows here. It is the Multica `agent_task_queue` adapted to "a task over the corpus"
-- (not a repo issue), pulled by the 10c CLI daemon via the same claim pattern as item_steps
-- (FOR UPDATE SKIP LOCKED), NOT the rara-addon SDK (the executor is a CLI daemon, not a Go
-- claim-worker, so the SDK's semantics don't fit).

CREATE TABLE IF NOT EXISTS agent_tasks (
    id            SERIAL PRIMARY KEY,
    agent_id      INT  NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    instruction   TEXT NOT NULL CHECK (btrim(instruction) <> ''),       -- free-form prompt
    context_refs  JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(context_refs) = 'array'),
    status        TEXT NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'dispatched', 'running', 'done', 'failed', 'cancelled')),
    priority      INT  NOT NULL DEFAULT 0,
    result        JSONB,                                                -- written by the daemon
    error         TEXT,                                                 -- written by the daemon
    session_id    TEXT,                                                 -- daemon run handle
    work_dir      TEXT,                                                 -- daemon scratch path
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    dispatched_at TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ
);

-- Per-agent history read (GET /v1/agents/{id}/tasks): newest first over (agent_id).
CREATE INDEX IF NOT EXISTS agent_tasks_agent_idx ON agent_tasks (agent_id, id DESC);

-- The board feed (ListAllAgentTasks(status)): rows of one status, newest first (status, id DESC).
CREATE INDEX IF NOT EXISTS agent_tasks_status_idx ON agent_tasks (status, id DESC);

-- The claim frontier (ClaimAgentTask): the next queued row by (priority DESC, created_at). Partial
-- on status='queued' so the index only holds the live frontier, not the done/failed history.
CREATE INDEX IF NOT EXISTS agent_tasks_claim_idx
    ON agent_tasks (priority DESC, created_at, id)
    WHERE status = 'queued';
