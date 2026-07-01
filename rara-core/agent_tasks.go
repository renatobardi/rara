package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// agent_tasks (CONSOLE-#10c1)
//
// The single execution primitive of the agents module: every surface that runs an agent (free-form
// task, distillation quick-run, board) only creates and reads rows here. The 10c CLI daemon pulls
// work via ClaimAgentTask, which mirrors the item_steps claim (FOR UPDATE SKIP LOCKED) — NOT the
// rara-addon SDK, whose claim-worker semantics don't fit a CLI executor.
// ---------------------------------------------------------------------------

// Task status values, mirroring the agent_tasks.status CHECK.
const (
	taskQueued     = "queued"
	taskDispatched = "dispatched"
	taskRunning    = "running"
	taskDone       = "done"
	taskFailed     = "failed"
	taskCancelled  = "cancelled"
)

var validTaskStatuses = map[string]bool{
	taskQueued: true, taskDispatched: true, taskRunning: true,
	taskDone: true, taskFailed: true, taskCancelled: true,
}

// AgentTaskInput is the write-side payload for enqueuing a task.
type AgentTaskInput struct {
	Instruction string          `json:"instruction"`
	ContextRefs json.RawMessage `json:"context_refs,omitempty"` // JSON array of distillation/item refs
	Priority    int             `json:"priority,omitempty"`
}

// AgentTaskRow is the read-side DTO. Daemon-written fields (result/error/session_id/work_dir/
// completed_at) were absent in 10c1; 10c2a adds them now that the executor populates them.
type AgentTaskRow struct {
	ID           int             `json:"id"`
	AgentID      int             `json:"agent_id"`
	Instruction  string          `json:"instruction"`
	ContextRefs  json.RawMessage `json:"context_refs"`
	Status       string          `json:"status"`
	Priority     int             `json:"priority"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	WorkDir      string          `json:"work_dir,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	DispatchedAt *time.Time      `json:"dispatched_at,omitempty"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`
}

// AgentTaskRecord is the validated, normalized write payload handed to the Database layer.
type AgentTaskRecord struct {
	AgentID     int
	Instruction string
	ContextRefs json.RawMessage
	Priority    int
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

// EnqueueAgentTask validates and enqueues a task for an agent (status=queued). A missing or
// soft-deleted agent maps to errNotFound (→ 404) via the DB layer's active-agent guard.
func (c *Core) EnqueueAgentTask(ctx context.Context, agentID int, in AgentTaskInput) (int, error) {
	if agentID <= 0 {
		return 0, badInput("agent id must be positive, got %d", agentID)
	}
	instruction := strings.TrimSpace(in.Instruction)
	if instruction == "" {
		return 0, badInput("instruction cannot be empty")
	}
	// context_refs defaults to an empty array; when present it must be a JSON array (mirrors the
	// CHECK (jsonb_typeof = 'array')) so the mock and pgx paths reject the same shapes.
	refs := in.ContextRefs
	if len(refs) == 0 {
		refs = json.RawMessage("[]")
	} else {
		var arr []json.RawMessage
		if err := json.Unmarshal(refs, &arr); err != nil {
			return 0, badInput("context_refs must be a JSON array")
		}
	}
	return c.db.EnqueueAgentTask(ctx, AgentTaskRecord{
		AgentID: agentID, Instruction: instruction, ContextRefs: refs, Priority: in.Priority,
	})
}

// ListAgentTasks returns one agent's task history (newest first).
func (c *Core) ListAgentTasks(ctx context.Context, agentID int) ([]AgentTaskRow, error) {
	if agentID <= 0 {
		return nil, badInput("agent id must be positive, got %d", agentID)
	}
	return c.db.ListAgentTasks(ctx, agentID)
}

// ListAllAgentTasks returns the global task feed for the board, optionally filtered by status.
// An empty status returns all; an unknown status is rejected (400) rather than yielding an empty list.
func (c *Core) ListAllAgentTasks(ctx context.Context, status string) ([]AgentTaskRow, error) {
	if status != "" && !validTaskStatuses[status] {
		return nil, badInput("unknown status %q", status)
	}
	return c.db.ListAllAgentTasks(ctx, status)
}

// ClaimAgentTask pulls the next queued task (highest priority, then oldest), moving it to
// dispatched. Returns nil when the queue is empty (idempotent no-op).
func (c *Core) ClaimAgentTask(ctx context.Context) (*AgentTaskRow, error) {
	return c.db.ClaimAgentTask(ctx)
}

// UpdateAgentTask writes daemon progress for a claimed task. Validates the status value; the
// database layer executes an unconditional UPDATE (the daemon is a trusted internal caller).
func (c *Core) UpdateAgentTask(ctx context.Context, id int, status, sessionID, workDir string, result json.RawMessage, errMsg string) error {
	if id <= 0 {
		return badInput("task id must be positive, got %d", id)
	}
	if !validTaskStatuses[status] {
		return badInput("unknown status %q", status)
	}
	return c.db.UpdateAgentTask(ctx, id, status, sessionID, workDir, result, errMsg)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (h *httpSurface) enqueueAgentTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req AgentTaskInput
	if !decodeJSON(w, r, &req) {
		return
	}
	taskID, err := h.core.EnqueueAgentTask(r.Context(), id, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": taskID})
}

func (h *httpSurface) listAgentTasks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tasks, err := h.core.ListAgentTasks(r.Context(), id)
	writeResult(w, tasks, err)
}

func (h *httpSurface) listAllAgentTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.core.ListAllAgentTasks(r.Context(), r.URL.Query().Get("status"))
	writeResult(w, tasks, err)
}

// ---------------------------------------------------------------------------
// pgxDatabase implementation
// ---------------------------------------------------------------------------

func (d *pgxDatabase) EnqueueAgentTask(ctx context.Context, t AgentTaskRecord) (int, error) {
	// INSERT … SELECT … WHERE EXISTS(active agent): enqueuing onto a missing or soft-deleted agent
	// inserts zero rows → errNotFound, rather than relying on the FK (which a soft-deleted agent
	// still satisfies, since the row survives a soft delete).
	const q = `
		INSERT INTO agent_tasks (agent_id, instruction, context_refs, priority)
		SELECT $1, $2, $3, $4
		WHERE EXISTS (SELECT 1 FROM agents WHERE id = $1 AND deleted_at IS NULL)
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, t.AgentID, t.Instruction, t.ContextRefs, t.Priority).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("enqueue agent task: %w", err)
	}
	return id, nil
}

// agentTaskCols is the shared read projection (column order matches scanAgentTask).
const agentTaskCols = `id, agent_id, instruction, context_refs, status, priority,
	result, error, session_id, work_dir, created_at, dispatched_at, completed_at`

// scanAgentTask scans one row in agentTaskCols order.
func scanAgentTask(rows pgx.Row) (AgentTaskRow, error) {
	var a AgentTaskRow
	var errStr, sessionID, workDir *string
	err := rows.Scan(&a.ID, &a.AgentID, &a.Instruction, &a.ContextRefs, &a.Status, &a.Priority,
		&a.Result, &errStr, &sessionID, &workDir, &a.CreatedAt, &a.DispatchedAt, &a.CompletedAt)
	if errStr != nil {
		a.Error = *errStr
	}
	if sessionID != nil {
		a.SessionID = *sessionID
	}
	if workDir != nil {
		a.WorkDir = *workDir
	}
	return a, err
}

func (d *pgxDatabase) ListAgentTasks(ctx context.Context, agentID int) ([]AgentTaskRow, error) {
	q := `SELECT ` + agentTaskCols + ` FROM agent_tasks WHERE agent_id = $1 ORDER BY id DESC`
	rows, err := d.conn.Query(ctx, q, agentID)
	if err != nil {
		return nil, fmt.Errorf("list agent tasks: %w", err)
	}
	defer rows.Close()
	return collectAgentTasks(rows)
}

func (d *pgxDatabase) ListAllAgentTasks(ctx context.Context, status string) ([]AgentTaskRow, error) {
	// One query, optional filter: an empty status ($1 = '') matches every row.
	q := `SELECT ` + agentTaskCols + ` FROM agent_tasks
		WHERE ($1 = '' OR status = $1) ORDER BY id DESC`
	rows, err := d.conn.Query(ctx, q, status)
	if err != nil {
		return nil, fmt.Errorf("list all agent tasks: %w", err)
	}
	defer rows.Close()
	return collectAgentTasks(rows)
}

func collectAgentTasks(rows pgx.Rows) ([]AgentTaskRow, error) {
	var out []AgentTaskRow
	for rows.Next() {
		a, err := scanAgentTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent task: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent tasks: %w", err)
	}
	return out, nil
}

// ClaimAgentTask is the Postgres work-queue pull, mirroring ClaimPendingStep: SELECT … FOR UPDATE
// SKIP LOCKED inside a transaction so concurrent claimers each grab a distinct queued row, then
// move it queued→dispatched (stamp dispatched_at) before COMMIT — leaving the queued frontier
// atomically. Returns nil when no task is queued.
func (d *pgxDatabase) ClaimAgentTask(ctx context.Context) (*AgentTaskRow, error) {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim agent task: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	// JOIN agents + deleted_at IS NULL: a task whose agent was soft-deleted after enqueue must NOT
	// be claimed — dispatching it would run an agent the operator already disabled (and burn its
	// corpus/credentials). FOR UPDATE OF t locks only the task row, never the agents row.
	const sel = `
		SELECT t.id
		FROM agent_tasks t
		JOIN agents a ON a.id = t.agent_id
		WHERE t.status = 'queued' AND a.deleted_at IS NULL
		ORDER BY t.priority DESC, t.created_at, t.id
		FOR UPDATE OF t SKIP LOCKED
		LIMIT 1`
	var id int
	if err := tx.QueryRow(ctx, sel).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // queue empty
	} else if err != nil {
		return nil, fmt.Errorf("claim agent task: select: %w", err)
	}

	upd := `UPDATE agent_tasks
		SET status = 'dispatched', dispatched_at = CURRENT_TIMESTAMP
		WHERE id = $1
		RETURNING ` + agentTaskCols
	a, err := scanAgentTask(tx.QueryRow(ctx, upd, id))
	if err != nil {
		return nil, fmt.Errorf("claim agent task: transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("claim agent task: commit: %w", err)
	}
	return &a, nil
}

func (d *pgxDatabase) UpdateAgentTask(ctx context.Context, id int, status, sessionID, workDir string, result json.RawMessage, errMsg string) error {
	var resultStr *string
	if len(result) > 0 {
		s := string(result)
		resultStr = &s
	}
	const q = `
		UPDATE agent_tasks SET
			status      = $2,
			session_id  = NULLIF($3, ''),
			work_dir    = NULLIF($4, ''),
			result      = CASE WHEN $5::text IS NOT NULL THEN $5::jsonb ELSE result END,
			error       = NULLIF($6, ''),
			completed_at = CASE WHEN $2 IN ('done','failed','cancelled')
			                    THEN CURRENT_TIMESTAMP ELSE completed_at END
		WHERE id = $1`
	if _, err := d.conn.Exec(ctx, q, id, status, sessionID, workDir, resultStr, errMsg); err != nil {
		return fmt.Errorf("update agent task %d: %w", id, err)
	}
	return nil
}
