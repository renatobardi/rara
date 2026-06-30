package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Agent registry (CONSOLE-#10b)
//
// An agent is the Multica-style persona the 10c daemon will run: name + instructions
// (system prompt) + a model + a set of attached skills. The model is the "kind/model"
// upstream the operator picks via the CORR-#3 provider+model picker, stored verbatim.
// ---------------------------------------------------------------------------

// maxAgentNameLen bounds name to the agents.name column width (VARCHAR(64)).
const maxAgentNameLen = 64

// validVisibilities mirrors the agents.visibility CHECK. Empty defaults to "workspace".
var validVisibilities = map[string]bool{"workspace": true, "private": true}

// AgentInput is the write-side payload for an agent.
type AgentInput struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	AvatarURL    string `json:"avatar_url"`
	Visibility   string `json:"visibility"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"` // "kind/model" upstream, via the picker
}

// AgentRow is the read-side DTO. SkillIDs is populated only by GetAgent (the detail read);
// ListAgents leaves it nil to keep the roster query a single table scan.
type AgentRow struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	AvatarURL    string    `json:"avatar_url"`
	Visibility   string    `json:"visibility"`
	Instructions string    `json:"instructions"`
	Model        string    `json:"model"`
	SkillIDs     []int     `json:"skill_ids"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// agentSkillsInput is the body for PUT /v1/agents/{id}/skills.
type agentSkillsInput struct {
	SkillIDs []int `json:"skill_ids"`
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

// UpsertAgent validates and persists an agent, keyed by (owner_id=NULL, name).
func (c *Core) UpsertAgent(ctx context.Context, in AgentInput) (int, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return 0, badInput("name cannot be empty")
	}
	if len(name) > maxAgentNameLen {
		return 0, badInput("name too long (max %d chars)", maxAgentNameLen)
	}
	vis := strings.TrimSpace(in.Visibility)
	if vis == "" {
		vis = "workspace"
	}
	if !validVisibilities[vis] {
		return 0, badInput("visibility must be 'workspace' or 'private', got %q", in.Visibility)
	}
	return c.db.UpsertAgent(ctx, name, in.Description, in.AvatarURL, vis, in.Instructions, strings.TrimSpace(in.Model))
}

// ListAgents returns non-deleted agents (roster; SkillIDs left nil).
func (c *Core) ListAgents(ctx context.Context) ([]AgentRow, error) {
	return c.db.ListAgents(ctx)
}

// GetAgent returns one non-deleted agent with its attached (non-deleted) skill ids.
func (c *Core) GetAgent(ctx context.Context, id int) (AgentRow, error) {
	if id <= 0 {
		return AgentRow{}, badInput("id must be positive, got %d", id)
	}
	return c.db.GetAgent(ctx, id)
}

// DeleteAgent soft-deletes an agent by id.
func (c *Core) DeleteAgent(ctx context.Context, id int) error {
	if id <= 0 {
		return badInput("id must be positive, got %d", id)
	}
	return c.db.DeleteAgent(ctx, id)
}

// SetAgentSkills replaces the agent's attached skills with the given set (idempotent).
func (c *Core) SetAgentSkills(ctx context.Context, agentID int, skillIDs []int) error {
	if agentID <= 0 {
		return badInput("agent id must be positive, got %d", agentID)
	}
	// Dedup and reject non-positive ids before the write (a 0/negative id can't be a real FK).
	seen := make(map[int]bool, len(skillIDs))
	clean := make([]int, 0, len(skillIDs))
	for _, sid := range skillIDs {
		if sid <= 0 {
			return badInput("skill id must be positive, got %d", sid)
		}
		if !seen[sid] {
			seen[sid] = true
			clean = append(clean, sid)
		}
	}
	return c.db.SetAgentSkills(ctx, agentID, clean)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (h *httpSurface) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.core.ListAgents(r.Context())
	writeResult(w, agents, err)
}

func (h *httpSurface) upsertAgent(w http.ResponseWriter, r *http.Request) {
	var req AgentInput
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := h.core.UpsertAgent(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": id})
}

func (h *httpSurface) getAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	agent, err := h.core.GetAgent(r.Context(), id)
	writeResult(w, agent, err)
}

func (h *httpSurface) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.DeleteAgent(r.Context(), id))
}

func (h *httpSurface) setAgentSkills(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req agentSkillsInput
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.SetAgentSkills(r.Context(), id, req.SkillIDs))
}

// ---------------------------------------------------------------------------
// pgxDatabase implementation
// ---------------------------------------------------------------------------

func (d *pgxDatabase) UpsertAgent(ctx context.Context, name, description, avatarURL, visibility, instructions, model string) (int, error) {
	const q = `
		INSERT INTO agents (name, description, avatar_url, visibility, instructions, model)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (owner_id, name) WHERE deleted_at IS NULL DO UPDATE SET
			description  = EXCLUDED.description,
			avatar_url   = EXCLUDED.avatar_url,
			visibility   = EXCLUDED.visibility,
			instructions = EXCLUDED.instructions,
			model        = EXCLUDED.model
		RETURNING id`
	var id int
	if err := d.conn.QueryRow(ctx, q, name, description, avatarURL, visibility, instructions, model).Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert agent: %w", err)
	}
	return id, nil
}

func (d *pgxDatabase) ListAgents(ctx context.Context) ([]AgentRow, error) {
	const q = `
		SELECT id, name, description, avatar_url, visibility, instructions, model, created_at, updated_at
		FROM agents
		WHERE deleted_at IS NULL
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var a AgentRow
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.AvatarURL, &a.Visibility,
			&a.Instructions, &a.Model, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list agents: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	return out, nil
}

func (d *pgxDatabase) GetAgent(ctx context.Context, id int) (AgentRow, error) {
	const q = `
		SELECT id, name, description, avatar_url, visibility, instructions, model, created_at, updated_at
		FROM agents
		WHERE id = $1 AND deleted_at IS NULL`
	var a AgentRow
	err := d.conn.QueryRow(ctx, q, id).Scan(&a.ID, &a.Name, &a.Description, &a.AvatarURL,
		&a.Visibility, &a.Instructions, &a.Model, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRow{}, errNotFound
	}
	if err != nil {
		return AgentRow{}, fmt.Errorf("get agent: %w", err)
	}
	// Attached skills, excluding any soft-deleted skill (the link survives a soft delete).
	const sq = `
		SELECT s.id
		FROM agent_skills a
		JOIN skills s ON s.id = a.skill_id AND s.deleted_at IS NULL
		WHERE a.agent_id = $1
		ORDER BY s.id`
	rows, err := d.conn.Query(ctx, sq, id)
	if err != nil {
		return AgentRow{}, fmt.Errorf("get agent skills: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid int
		if err := rows.Scan(&sid); err != nil {
			return AgentRow{}, fmt.Errorf("get agent skills: scan: %w", err)
		}
		a.SkillIDs = append(a.SkillIDs, sid)
	}
	if err := rows.Err(); err != nil {
		return AgentRow{}, fmt.Errorf("get agent skills: %w", err)
	}
	return a, nil
}

func (d *pgxDatabase) DeleteAgent(ctx context.Context, id int) error {
	const q = `UPDATE agents SET deleted_at = CURRENT_TIMESTAMP WHERE id = $1 AND deleted_at IS NULL`
	if _, err := d.conn.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return nil
}

func (d *pgxDatabase) SetAgentSkills(ctx context.Context, agentID int, skillIDs []int) error {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set agent skills: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Guard + lock: writing skills onto a soft-deleted (or absent) agent must 404. FOR UPDATE
	// serializes concurrent set-skills for the same agent, so two racing replaces can't interleave
	// their DELETE/INSERT and leave a nondeterministic set.
	var lockedID int
	err = tx.QueryRow(ctx,
		`SELECT id FROM agents WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, agentID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return fmt.Errorf("set agent skills: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_skills WHERE agent_id = $1`, agentID); err != nil {
		return fmt.Errorf("set agent skills: clear: %w", err)
	}
	if len(skillIDs) > 0 {
		// INSERT … SELECT filters to active skills, so a missing or soft-deleted id is dropped, not
		// linked behind GetAgent's back. skillIDs is deduped by the caller, so RowsAffected < len
		// means at least one id was invalid → reject the whole set (400) rather than partially apply.
		tag, err := tx.Exec(ctx,
			`INSERT INTO agent_skills (agent_id, skill_id)
			 SELECT $1, s.id FROM skills s WHERE s.id = ANY($2::int[]) AND s.deleted_at IS NULL`,
			agentID, skillIDs)
		if err != nil {
			return fmt.Errorf("set agent skills: insert: %w", err)
		}
		if int(tag.RowsAffected()) != len(skillIDs) {
			return badInput("one or more skills do not exist or are deleted")
		}
	}
	return tx.Commit(ctx)
}
