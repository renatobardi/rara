package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// LLMModelInput is the write-side payload for PUT /v1/llm-models.
type LLMModelInput struct {
	ProviderID         int             `json:"provider_id"`
	Alias              string          `json:"alias"`
	Upstream           string          `json:"upstream"`
	InputCostPerToken  float64         `json:"input_cost_per_token"`
	OutputCostPerToken float64         `json:"output_cost_per_token"`
	Params             json.RawMessage `json:"params,omitempty"`
	Enabled            *bool           `json:"enabled"`
}

// LLMModelRow is the read-side DTO; ProviderName is a light join for display.
type LLMModelRow struct {
	ID                 int             `json:"id"`
	ProviderID         int             `json:"provider_id"`
	ProviderName       string          `json:"provider_name"`
	Alias              string          `json:"alias"`
	Upstream           string          `json:"upstream"`
	InputCostPerToken  float64         `json:"input_cost_per_token"`
	OutputCostPerToken float64         `json:"output_cost_per_token"`
	Params             json.RawMessage `json:"params"`
	Enabled            bool            `json:"enabled"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

// llmModelUpsert is the resolved, validated payload handed to the storage layer.
// Using a struct (not a positional arg list) keeps same-typed fields — alias/upstream
// and inputCost/outputCost — from being silently swapped at the call site.
type llmModelUpsert struct {
	ProviderID int
	Alias      string
	Upstream   string
	InputCost  float64
	OutputCost float64
	Params     json.RawMessage
	Enabled    bool
}

// UpsertLLMModel validates the input and upserts by (owner_id=NULL, alias).
func (c *Core) UpsertLLMModel(ctx context.Context, in LLMModelInput) error {
	// Normalize before validating so blank-but-not-empty input ("   ") is rejected
	// and the trimmed values are what gets persisted.
	in.Alias = strings.TrimSpace(in.Alias)
	in.Upstream = strings.TrimSpace(in.Upstream)
	if in.Alias == "" {
		return badInput("alias cannot be empty")
	}
	if in.Upstream == "" {
		return badInput("upstream cannot be empty")
	}
	if in.ProviderID <= 0 {
		return badInput("provider_id must be positive, got %d", in.ProviderID)
	}
	if in.InputCostPerToken < 0 {
		return badInput("input_cost_per_token must be >= 0, got %v", in.InputCostPerToken)
	}
	if in.OutputCostPerToken < 0 {
		return badInput("output_cost_per_token must be >= 0, got %v", in.OutputCostPerToken)
	}
	if len(in.Params) > 0 && !isJSONObject(in.Params) {
		return badInput("params must be a JSON object")
	}

	p, found, err := c.db.GetLLMProvider(ctx, in.ProviderID)
	if err != nil {
		return fmt.Errorf("get llm provider %d: %w", in.ProviderID, err)
	}
	if !found {
		return badInput("provider_id %d not found", in.ProviderID)
	}
	if !p.Enabled {
		return badInput("provider_id %d is disabled", in.ProviderID)
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	params := in.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}

	// The GetLLMProvider check above is advisory only; the provider could be deleted or
	// disabled in the window before persist. The real guard is the storage layer, which
	// re-checks the provider is active in the same INSERT (see pgxDatabase.UpsertLLMModel).
	if _, err := c.db.UpsertLLMModel(ctx, llmModelUpsert{
		ProviderID: in.ProviderID,
		Alias:      in.Alias,
		Upstream:   in.Upstream,
		InputCost:  in.InputCostPerToken,
		OutputCost: in.OutputCostPerToken,
		Params:     params,
		Enabled:    enabled,
	}); err != nil {
		return fmt.Errorf("upsert llm model %q: %w", in.Alias, err)
	}
	return nil
}

// ListLLMModels returns non-deleted models; providerID=0 returns all.
func (c *Core) ListLLMModels(ctx context.Context, providerID int) ([]LLMModelRow, error) {
	models, err := c.db.ListLLMModels(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("list llm models: %w", err)
	}
	return models, nil
}

// DeleteLLMModel soft-deletes a model by id.
func (c *Core) DeleteLLMModel(ctx context.Context, id int) error {
	if id <= 0 {
		return badInput("id must be positive, got %d", id)
	}
	if err := c.db.DeleteLLMModel(ctx, id); err != nil {
		return fmt.Errorf("delete llm model %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (h *httpSurface) listLLMModels(w http.ResponseWriter, r *http.Request) {
	var providerID int
	if raw := r.URL.Query().Get("provider_id"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeResult(w, nil, badInput("provider_id must be a non-negative integer"))
			return
		}
		providerID = n
	}
	models, err := h.core.ListLLMModels(r.Context(), providerID)
	writeResult(w, models, err)
}

func (h *httpSurface) upsertLLMModel(w http.ResponseWriter, r *http.Request) {
	var req LLMModelInput
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertLLMModel(r.Context(), req))
}

func (h *httpSurface) deleteLLMModel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id in path"})
		return
	}
	writeResult(w, okResult{OK: true}, h.core.DeleteLLMModel(r.Context(), id))
}

// ---------------------------------------------------------------------------
// pgxDatabase implementation
// ---------------------------------------------------------------------------

// GetLLMProvider returns one provider by id for FK validation by the model layer.
func (d *pgxDatabase) GetLLMProvider(ctx context.Context, id int) (LLMProviderRow, bool, error) {
	const q = `
		SELECT id, name, kind, COALESCE(base_url,''), COALESCE(key_last4,''), enabled, created_at, updated_at
		FROM llm_providers
		WHERE id = $1 AND deleted_at IS NULL`
	var p LLMProviderRow
	err := d.conn.QueryRow(ctx, q, id).Scan(
		&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.KeyLast4, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLMProviderRow{}, false, nil
	}
	if err != nil {
		return LLMProviderRow{}, false, err
	}
	return p, true, nil
}

func (d *pgxDatabase) UpsertLLMModel(ctx context.Context, m llmModelUpsert) (int, error) {
	// INSERT ... SELECT conditions the write on the provider being active at persist
	// time, closing the TOCTOU gap left by a prior GetLLMProvider check: if the provider
	// was soft-deleted or disabled in the meantime, the SELECT yields no row, nothing is
	// written, and QueryRow returns pgx.ErrNoRows (mapped to errNotFound below).
	const q = `
		INSERT INTO llm_models (provider_id, alias, upstream, input_cost_per_token, output_cost_per_token, params, enabled)
		SELECT p.id, $2, $3, $4, $5, $6::jsonb, $7
		FROM llm_providers p
		WHERE p.id = $1 AND p.deleted_at IS NULL AND p.enabled
		ON CONFLICT (owner_id, alias) WHERE deleted_at IS NULL DO UPDATE SET
			provider_id           = EXCLUDED.provider_id,
			upstream              = EXCLUDED.upstream,
			input_cost_per_token  = EXCLUDED.input_cost_per_token,
			output_cost_per_token = EXCLUDED.output_cost_per_token,
			params                = EXCLUDED.params,
			enabled               = EXCLUDED.enabled
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, m.ProviderID, m.Alias, m.Upstream,
		m.InputCost, m.OutputCost, string(m.Params), m.Enabled).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	return id, err
}

func (d *pgxDatabase) ListLLMModels(ctx context.Context, providerID int) ([]LLMModelRow, error) {
	const baseQ = `
		SELECT m.id, m.provider_id, p.name, m.alias, m.upstream,
		       m.input_cost_per_token::float8, m.output_cost_per_token::float8,
		       m.params, m.enabled, m.created_at, m.updated_at
		FROM llm_models m
		JOIN llm_providers p ON p.id = m.provider_id AND p.deleted_at IS NULL
		WHERE m.deleted_at IS NULL`

	var (
		rows pgx.Rows
		err  error
	)
	if providerID > 0 {
		rows, err = d.conn.Query(ctx, baseQ+" AND m.provider_id = $1 ORDER BY m.id", providerID)
	} else {
		rows, err = d.conn.Query(ctx, baseQ+" ORDER BY m.id")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LLMModelRow
	for rows.Next() {
		var m LLMModelRow
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.ProviderName, &m.Alias, &m.Upstream,
			&m.InputCostPerToken, &m.OutputCostPerToken, &m.Params, &m.Enabled,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) DeleteLLMModel(ctx context.Context, id int) error {
	const q = `UPDATE llm_models SET deleted_at = CURRENT_TIMESTAMP WHERE id = $1 AND deleted_at IS NULL`
	_, err := d.conn.Exec(ctx, q, id)
	return err
}

// ListEnabledLLMModelsForSync feeds the LLM reconciler: enabled models whose provider is also
// enabled and not soft-deleted, with the provider's encrypted key material for decryption.
func (d *pgxDatabase) ListEnabledLLMModelsForSync(ctx context.Context) ([]llmModelSync, error) {
	const q = `
		SELECT m.alias, m.upstream, p.kind, COALESCE(p.base_url,''),
		       p.key_ciphertext, p.key_nonce,
		       m.input_cost_per_token::float8, m.output_cost_per_token::float8, m.params
		FROM llm_models m
		JOIN llm_providers p ON p.id = m.provider_id AND p.deleted_at IS NULL AND p.enabled
		WHERE m.deleted_at IS NULL AND m.enabled
		ORDER BY m.alias`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []llmModelSync
	for rows.Next() {
		var s llmModelSync
		if err := rows.Scan(&s.Alias, &s.Upstream, &s.ProviderKind, &s.BaseURL,
			&s.KeyCiphertext, &s.KeyNonce, &s.InputCost, &s.OutputCost, &s.Params); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
