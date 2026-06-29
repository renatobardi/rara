package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// maxSkillNameLen bounds name to the skills.name column width (VARCHAR(64)).
const maxSkillNameLen = 64

// SkillInput is the write-side payload for a skill (the SKILL.md bundle metadata).
type SkillInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Content     string          `json:"content"` // the SKILL.md body
	Config      json.RawMessage `json:"config"`  // jsonb object; empty defaults to {}
	Trusted     bool            `json:"trusted"` // operator gate — daemon (10c2) only runs trusted skills
}

// SkillRow is the read-side DTO.
type SkillRow struct {
	ID          int             `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Content     string          `json:"content"`
	Config      json.RawMessage `json:"config"`
	Trusted     bool            `json:"trusted"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// SkillFile is one supporting file in a skill bundle (e.g. utils.py, config.json).
type SkillFile struct {
	ID        int       `json:"id"`
	SkillID   int       `json:"skill_id"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SkillFileInput is the write-side payload for a skill file.
type SkillFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// skillImportInput is the body for POST /v1/skills/import.
type skillImportInput struct {
	URL string `json:"url"`
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// isJSONObjectRaw reports whether raw is empty (defaults to {}) or a JSON object. config is a
// free-form jsonb object — unlike provider env it may hold non-string values, so this only
// rejects arrays/scalars/null, not nested types.
func isJSONObjectRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return obj != nil // JSON null unmarshals to a nil map
}

// validateSkillPath rejects empty, absolute, and traversal paths. The 10c2 daemon writes these
// files into a skill workdir, so a "../" or "/abs" path would escape it — this is the trust
// boundary for that future write, validated here even though the daemon isn't built yet.
func validateSkillPath(path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return badInput("file path cannot be empty")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return badInput("file path %q must be relative", path)
	}
	// Reject ':' — catches Windows drive letters (C:\) and NTFS alternate data streams (a.txt:s),
	// which the segment check below would otherwise miss.
	if strings.Contains(p, ":") {
		return badInput("file path %q must not contain ':'", path)
	}
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return badInput("file path %q must not contain ..", path)
		}
	}
	// SKILL.md is reserved — it's the skill's content column, not a bundle file.
	if strings.EqualFold(p, "SKILL.md") {
		return badInput("path %q is reserved (that's the SKILL.md content)", path)
	}
	return nil
}

// parseSkillMD extracts name/description from a SKILL.md YAML frontmatter block (--- … ---) and
// returns the FULL document as content (the column stores the verbatim SKILL.md). A minimal
// line parser — skill frontmatter is flat key: value, so no YAML dependency is warranted.
func parseSkillMD(body []byte) (name, description, content string) {
	content = string(body)
	s := strings.TrimLeft(content, "\ufeff \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return "", "", content
	}
	// Frontmatter is between the first and second --- line.
	rest := strings.TrimPrefix(s, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", content
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v := strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	return name, description, content
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

// UpsertSkill validates and persists a skill, keyed by (owner_id=NULL, name).
func (c *Core) UpsertSkill(ctx context.Context, in SkillInput) (int, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return 0, badInput("name cannot be empty")
	}
	if len(name) > maxSkillNameLen {
		return 0, badInput("name too long (max %d chars)", maxSkillNameLen)
	}
	if !isJSONObjectRaw(in.Config) {
		return 0, badInput("config must be a JSON object")
	}
	cfg := strings.TrimSpace(string(in.Config))
	if cfg == "" {
		cfg = "{}"
	}
	return c.db.UpsertSkill(ctx, name, in.Description, in.Content, cfg, in.Trusted)
}

// ListSkills returns non-deleted skills (with content + config).
func (c *Core) ListSkills(ctx context.Context) ([]SkillRow, error) {
	return c.db.ListSkills(ctx)
}

// DeleteSkill soft-deletes a skill by id.
func (c *Core) DeleteSkill(ctx context.Context, id int) error {
	if id <= 0 {
		return badInput("id must be positive, got %d", id)
	}
	return c.db.DeleteSkill(ctx, id)
}

// ListSkillFiles returns the files of a skill bundle.
func (c *Core) ListSkillFiles(ctx context.Context, skillID int) ([]SkillFile, error) {
	if skillID <= 0 {
		return nil, badInput("skill id must be positive")
	}
	return c.db.ListSkillFiles(ctx, skillID)
}

// UpsertSkillFile validates the path and persists a file in a skill bundle.
func (c *Core) UpsertSkillFile(ctx context.Context, skillID int, in SkillFileInput) (int, error) {
	if skillID <= 0 {
		return 0, badInput("skill id must be positive")
	}
	if err := validateSkillPath(in.Path); err != nil {
		return 0, err
	}
	return c.db.UpsertSkillFile(ctx, skillID, strings.TrimSpace(in.Path), in.Content)
}

// DeleteSkillFile removes a file from a skill bundle.
func (c *Core) DeleteSkillFile(ctx context.Context, skillID int, path string) error {
	if skillID <= 0 {
		return badInput("skill id must be positive")
	}
	if strings.TrimSpace(path) == "" {
		return badInput("file path cannot be empty")
	}
	return c.db.DeleteSkillFile(ctx, skillID, strings.TrimSpace(path))
}

// ImportSkill fetches a SKILL.md from a public URL (ClawHub/skills.sh), parses its frontmatter,
// and stores it trusted=false — the operator must deliberately trust it before the 10c2 daemon
// will run any script it carries. Provenance (source_url) is recorded in config; we keep it in
// the durable DB row rather than a sidecar lockfile that an ephemeral Cloud Run job would lose.
func (c *Core) ImportSkill(ctx context.Context, rawURL string) (SkillRow, error) {
	if c.fetchURL == nil {
		return SkillRow{}, badInput("skill import not configured")
	}
	if err := validateEndpointURL(rawURL); err != nil {
		return SkillRow{}, err
	}
	body, err := c.fetchURL(ctx, rawURL)
	if err != nil {
		return SkillRow{}, fmt.Errorf("fetch skill %q: %w", rawURL, err)
	}
	name, desc, content := parseSkillMD(body)
	if strings.TrimSpace(name) == "" {
		return SkillRow{}, badInput("SKILL.md has no name in its frontmatter")
	}
	cfg, err := json.Marshal(map[string]string{"source_url": rawURL})
	if err != nil {
		return SkillRow{}, fmt.Errorf("marshal skill import config: %w", err)
	}
	id, err := c.UpsertSkill(ctx, SkillInput{
		Name: name, Description: desc, Content: content, Config: cfg, Trusted: false,
	})
	if err != nil {
		return SkillRow{}, err
	}
	return SkillRow{ID: id, Name: name, Description: desc, Content: content, Config: cfg, Trusted: false}, nil
}

// httpGetURL fetches rawURL with a bounded body, used as the default Core.fetchURL in production.
// maxSkillImportBytes caps the import to keep a hostile registry from exhausting memory.
const maxSkillImportBytes = 1 << 20 // 1 MiB

func httpGetURL(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSkillImportBytes))
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (h *httpSurface) listSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := h.core.ListSkills(r.Context())
	writeResult(w, skills, err)
}

func (h *httpSurface) upsertSkill(w http.ResponseWriter, r *http.Request) {
	var req SkillInput
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := h.core.UpsertSkill(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": id})
}

func (h *httpSurface) deleteSkill(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.DeleteSkill(r.Context(), id))
}

func (h *httpSurface) listSkillFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	files, err := h.core.ListSkillFiles(r.Context(), id)
	writeResult(w, files, err)
}

func (h *httpSurface) upsertSkillFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req SkillFileInput
	if !decodeJSON(w, r, &req) {
		return
	}
	fid, err := h.core.UpsertSkillFile(r.Context(), id, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": fid})
}

func (h *httpSurface) deleteSkillFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req SkillFileInput // only Path is read for delete
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.DeleteSkillFile(r.Context(), id, req.Path))
}

func (h *httpSurface) importSkill(w http.ResponseWriter, r *http.Request) {
	var req skillImportInput
	if !decodeJSON(w, r, &req) {
		return
	}
	row, err := h.core.ImportSkill(r.Context(), strings.TrimSpace(req.URL))
	writeResult(w, row, err)
}

// ---------------------------------------------------------------------------
// pgxDatabase implementation
// ---------------------------------------------------------------------------

func (d *pgxDatabase) UpsertSkill(ctx context.Context, name, description, content, config string, trusted bool) (int, error) {
	const q = `
		INSERT INTO skills (name, description, content, config, trusted)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (owner_id, name) WHERE deleted_at IS NULL DO UPDATE SET
			description = EXCLUDED.description,
			content     = EXCLUDED.content,
			config      = EXCLUDED.config,
			trusted     = EXCLUDED.trusted
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, name, description, content, config, trusted).Scan(&id)
	return id, err
}

func (d *pgxDatabase) ListSkills(ctx context.Context) ([]SkillRow, error) {
	const q = `
		SELECT id, name, description, content, config, trusted, created_at, updated_at
		FROM skills
		WHERE deleted_at IS NULL
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillRow
	for rows.Next() {
		var s SkillRow
		if err := rows.Scan(&s.ID, &s.Name, &s.Description, &s.Content, &s.Config,
			&s.Trusted, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) DeleteSkill(ctx context.Context, id int) error {
	const q = `UPDATE skills SET deleted_at = CURRENT_TIMESTAMP WHERE id = $1 AND deleted_at IS NULL`
	_, err := d.conn.Exec(ctx, q, id)
	return err
}

func (d *pgxDatabase) ListSkillFiles(ctx context.Context, skillID int) ([]SkillFile, error) {
	// EXISTS guard: a soft-deleted skill's files must not leak (DeleteSkill is soft, so the
	// rows survive — only the parent's deleted_at gates visibility).
	const q = `
		SELECT id, skill_id, path, content, created_at, updated_at
		FROM skill_files
		WHERE skill_id = $1
		  AND EXISTS (SELECT 1 FROM skills WHERE id = $1 AND deleted_at IS NULL)
		ORDER BY path`
	rows, err := d.conn.Query(ctx, q, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillFile
	for rows.Next() {
		var f SkillFile
		if err := rows.Scan(&f.ID, &f.SkillID, &f.Path, &f.Content, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) UpsertSkillFile(ctx context.Context, skillID int, path, content string) (int, error) {
	// INSERT … SELECT … WHERE EXISTS guards against writing to a soft-deleted skill: no active
	// parent → no row → ErrNoRows, which the caller maps to errNotFound (404).
	const q = `
		INSERT INTO skill_files (skill_id, path, content)
		SELECT $1, $2, $3
		WHERE EXISTS (SELECT 1 FROM skills WHERE id = $1 AND deleted_at IS NULL)
		ON CONFLICT (skill_id, path) DO UPDATE SET content = EXCLUDED.content
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, skillID, path, content).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	return id, err
}

func (d *pgxDatabase) DeleteSkillFile(ctx context.Context, skillID int, path string) error {
	const q = `DELETE FROM skill_files WHERE skill_id = $1 AND path = $2`
	_, err := d.conn.Exec(ctx, q, skillID, path)
	return err
}
