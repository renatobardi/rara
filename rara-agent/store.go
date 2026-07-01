package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the agent daemon's persistence contract.
type Store interface {
	ClaimTask(ctx context.Context) (*Task, error)
	UpdateTask(ctx context.Context, id int, status, sessionID, workDir string, result json.RawMessage, errMsg string) error
	FetchAgent(ctx context.Context, agentID int) (AgentInfo, error)
	FetchSkills(ctx context.Context, skillIDs []int) ([]SkillBundle, error)
}

type pgxStore struct {
	pool      *pgxpool.Pool
	coreURL   string
	coreToken string
	client    *http.Client
}

func newPgxStore(ctx context.Context, dbURL, coreURL, coreToken string) (*pgxStore, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol // PgBouncer compat
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	return &pgxStore{
		pool:      pool,
		coreURL:   coreURL,
		coreToken: coreToken,
		client:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// ClaimTask atomically claims the next queued task for executor='cli' (queued→dispatched).
// Returns nil when the queue is empty.
func (s *pgxStore) ClaimTask(ctx context.Context) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const sel = `
		SELECT t.id, t.agent_id, t.instruction, t.context_refs
		FROM agent_tasks t
		JOIN agents a ON a.id = t.agent_id
		WHERE t.status = 'queued'
		  AND a.deleted_at IS NULL
		  AND a.executor = 'cli'
		ORDER BY t.priority DESC, t.created_at, t.id
		FOR UPDATE OF t SKIP LOCKED
		LIMIT 1`
	var t Task
	err = tx.QueryRow(ctx, sel).Scan(&t.ID, &t.AgentID, &t.Instruction, &t.ContextRefs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim: select: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_tasks SET status='dispatched', dispatched_at=NOW() WHERE id=$1`, t.ID); err != nil {
		return nil, fmt.Errorf("claim: update: %w", err)
	}
	return &t, tx.Commit(ctx)
}

// UpdateTask writes progress for a claimed task.
func (s *pgxStore) UpdateTask(ctx context.Context, id int, status, sessionID, workDir string, result json.RawMessage, errMsg string) error {
	var resultStr *string
	if len(result) > 0 {
		rs := string(result)
		resultStr = &rs
	}
	const q = `
		UPDATE agent_tasks SET
			status       = $2,
			session_id   = NULLIF($3, ''),
			work_dir     = NULLIF($4, ''),
			result       = CASE WHEN $5::text IS NOT NULL THEN $5::jsonb ELSE result END,
			error        = NULLIF($6, ''),
			completed_at = CASE WHEN $2 IN ('done','failed','cancelled')
			                    THEN NOW() ELSE completed_at END
		WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id, status, sessionID, workDir, resultStr, errMsg); err != nil {
		return fmt.Errorf("update task %d: %w", id, err)
	}
	return nil
}

// FetchAgent calls rara-core GET /v1/agents/{id}.
func (s *pgxStore) FetchAgent(ctx context.Context, agentID int) (AgentInfo, error) {
	url := fmt.Sprintf("%s/v1/agents/%d", s.coreURL, agentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AgentInfo{}, fmt.Errorf("build agent request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.coreToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return AgentInfo{}, fmt.Errorf("fetch agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AgentInfo{}, fmt.Errorf("fetch agent %d: status %d", agentID, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return AgentInfo{}, fmt.Errorf("read agent response: %w", err)
	}

	var row struct {
		ID           int             `json:"id"`
		Name         string          `json:"name"`
		Model        string          `json:"model"`
		MCPConfig    json.RawMessage `json:"mcp_config"`
		CustomEnv    json.RawMessage `json:"custom_env"`
		CustomArgs   json.RawMessage `json:"custom_args"`
		Instructions string          `json:"instructions"`
		Executor     string          `json:"executor"`
		SkillIDs     []int           `json:"skill_ids"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		return AgentInfo{}, fmt.Errorf("decode agent: %w", err)
	}

	var customEnv map[string]string
	if len(row.CustomEnv) > 0 {
		if err := json.Unmarshal(row.CustomEnv, &customEnv); err != nil {
			return AgentInfo{}, fmt.Errorf("decode agent custom_env: %w", err)
		}
	}
	var customArgs []string
	if len(row.CustomArgs) > 0 {
		if err := json.Unmarshal(row.CustomArgs, &customArgs); err != nil {
			return AgentInfo{}, fmt.Errorf("decode agent custom_args: %w", err)
		}
	}

	return AgentInfo{
		ID: row.ID, Name: row.Name, Model: row.Model,
		MCPConfig: row.MCPConfig, CustomEnv: customEnv, CustomArgs: customArgs,
		Instructions: row.Instructions, Executor: row.Executor, SkillIDs: row.SkillIDs,
	}, nil
}

// FetchSkills calls GET /v1/skills then builds bundles in the requested order.
// ponytail: client-side filter — skills are few (~10s), not worth a new endpoint.
func (s *pgxStore) FetchSkills(ctx context.Context, skillIDs []int) ([]SkillBundle, error) {
	if len(skillIDs) == 0 {
		return nil, nil
	}

	url := s.coreURL + "/v1/skills"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build skills request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.coreToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch skills: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch skills: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read skills response: %w", err)
	}

	var rows []struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		Content string `json:"content"`
		Trusted bool   `json:"trusted"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode skills: %w", err)
	}

	// Index by ID for O(1) lookup, then iterate skillIDs to preserve configured order.
	byID := make(map[int]struct {
		Name    string
		Content string
		Trusted bool
	}, len(rows))
	for _, r := range rows {
		byID[r.ID] = struct {
			Name    string
			Content string
			Trusted bool
		}{r.Name, r.Content, r.Trusted}
	}

	out := make([]SkillBundle, 0, len(skillIDs))
	for _, id := range skillIDs {
		r, ok := byID[id]
		if !ok {
			continue
		}
		bundle := SkillBundle{ID: id, Name: r.Name, Content: r.Content, Trusted: r.Trusted}
		if r.Trusted {
			files, err := s.fetchSkillFiles(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("skill %d files: %w", id, err)
			}
			bundle.Files = files
		}
		out = append(out, bundle)
	}
	return out, nil
}

func (s *pgxStore) fetchSkillFiles(ctx context.Context, skillID int) ([]SkillFile, error) {
	url := fmt.Sprintf("%s/v1/skills/%d/files", s.coreURL, skillID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build skill-files request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.coreToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skill files %d: status %d", skillID, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read skill files: %w", err)
	}
	var rows []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode skill files: %w", err)
	}
	files := make([]SkillFile, 0, len(rows))
	for _, r := range rows {
		files = append(files, SkillFile{Path: r.Path, Content: r.Content})
	}
	return files, nil
}
