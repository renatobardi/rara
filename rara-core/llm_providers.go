package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"rara-core/internal/secretbox"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// validLLMKinds mirrors the CHECK constraint in 030_llm_providers.sql.
var validLLMKinds = map[string]bool{
	"groq": true, "gemini": true, "anthropic": true,
	"openai": true, "deepseek": true, "openai_compatible": true,
}

// LLMProviderInput is the write-side payload (api_key is write-only).
type LLMProviderInput struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"` // write-only; empty = preserve existing on edit
	Enabled *bool  `json:"enabled"` // nil = defaults to true
}

// LLMProviderRow is the read-side DTO. KeyCiphertext/KeyNonce are never serialized.
type LLMProviderRow struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	BaseURL   string    `json:"base_url,omitempty"`
	KeyLast4  string    `json:"key_last4,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Internal fields — never serialized (json:"-").
	KeyCiphertext []byte `json:"-"`
	KeyNonce      []byte `json:"-"`
}

// llmProviderRaw holds the encrypted secret for server-side operations (rotation, verify).
// Never sent to callers.
type llmProviderRaw struct {
	KeyCiphertext []byte
	KeyNonce      []byte
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

// UpsertLLMProvider validates the input, encrypts api_key if provided, and persists.
// When api_key is empty, it updates only the non-secret fields of an existing provider;
// if no matching provider exists, it returns badInput (api_key is required for new providers).
func (c *Core) UpsertLLMProvider(ctx context.Context, in LLMProviderInput) error {
	if !validLLMKinds[in.Kind] {
		return badInput("invalid kind %q (want groq|gemini|anthropic|openai|deepseek|openai_compatible)", in.Kind)
	}
	if in.Kind == "openai_compatible" && in.BaseURL == "" {
		return badInput("base_url is required for kind openai_compatible")
	}
	if in.Name == "" {
		return badInput("name cannot be empty")
	}
	if in.BaseURL != "" {
		if err := validateEndpointURL(in.BaseURL); err != nil {
			return err
		}
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	if in.APIKey == "" {
		// Key-absent path: update existing fields only; preserves stored ciphertext.
		err := c.db.UpdateLLMProviderFields(ctx, in.Name, in.Kind, in.BaseURL, enabled)
		if errors.Is(err, errNotFound) {
			return badInput("provider %q not found; api_key is required for new providers", in.Name)
		}
		if err != nil {
			return fmt.Errorf("update llm provider %q: %w", in.Name, err)
		}
		return nil
	}

	if c.box == nil {
		return badInput("encryption not configured (RARA_SECRETS_KEY missing)")
	}
	ct, nonce, err := c.box.Encrypt([]byte(in.APIKey))
	if err != nil {
		return fmt.Errorf("encrypt api_key for %q: %w", in.Name, err)
	}
	last4 := secretbox.Last4(in.APIKey)
	if _, err := c.db.UpsertLLMProvider(ctx, in.Name, in.Kind, in.BaseURL, ct, nonce, last4, enabled); err != nil {
		return fmt.Errorf("upsert llm provider %q: %w", in.Name, err)
	}
	return nil
}

// ListLLMProviders returns non-deleted providers (no key material in response).
func (c *Core) ListLLMProviders(ctx context.Context) ([]LLMProviderRow, error) {
	return c.db.ListLLMProviders(ctx)
}

// DeleteLLMProvider soft-deletes a provider by id.
func (c *Core) DeleteLLMProvider(ctx context.Context, id int) error {
	if id <= 0 {
		return badInput("id must be positive, got %d", id)
	}
	return c.db.DeleteLLMProvider(ctx, id)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (h *httpSurface) listLLMProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.core.ListLLMProviders(r.Context())
	writeResult(w, providers, err)
}

func (h *httpSurface) upsertLLMProvider(w http.ResponseWriter, r *http.Request) {
	var req LLMProviderInput
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertLLMProvider(r.Context(), req))
}

func (h *httpSurface) deleteLLMProvider(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id in path"})
		return
	}
	writeResult(w, okResult{OK: true}, h.core.DeleteLLMProvider(r.Context(), id))
}

// ---------------------------------------------------------------------------
// pgxDatabase implementation
// ---------------------------------------------------------------------------

func (d *pgxDatabase) UpsertLLMProvider(ctx context.Context, name, kind, baseURL string,
	keyCiphertext, keyNonce []byte, keyLast4 string, enabled bool) (int, error) {
	const q = `
		INSERT INTO llm_providers (name, kind, base_url, key_ciphertext, key_nonce, key_last4, enabled)
		VALUES ($1, $2, NULLIF($3,''), $4, $5, NULLIF($6,''), $7)
		ON CONFLICT (owner_id, name) WHERE deleted_at IS NULL DO UPDATE SET
			kind           = EXCLUDED.kind,
			base_url       = EXCLUDED.base_url,
			key_ciphertext = EXCLUDED.key_ciphertext,
			key_nonce      = EXCLUDED.key_nonce,
			key_last4      = EXCLUDED.key_last4,
			enabled        = EXCLUDED.enabled
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, name, kind, baseURL, keyCiphertext, keyNonce, keyLast4, enabled).Scan(&id)
	return id, err
}

func (d *pgxDatabase) UpdateLLMProviderFields(ctx context.Context, name, kind, baseURL string, enabled bool) error {
	const q = `
		UPDATE llm_providers
		SET kind = $2, base_url = NULLIF($3,''), enabled = $4
		WHERE name = $1 AND owner_id IS NULL AND deleted_at IS NULL
		RETURNING id`
	var id int
	err := d.conn.QueryRow(ctx, q, name, kind, baseURL, enabled).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	return err
}

func (d *pgxDatabase) ListLLMProviders(ctx context.Context) ([]LLMProviderRow, error) {
	const q = `
		SELECT id, name, kind, COALESCE(base_url,''), COALESCE(key_last4,''), enabled, created_at, updated_at
		FROM llm_providers
		WHERE deleted_at IS NULL
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMProviderRow
	for rows.Next() {
		var p LLMProviderRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.KeyLast4,
			&p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		// ciphertext/nonce are never scanned; KeyCiphertext/KeyNonce stay nil.
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) DeleteLLMProvider(ctx context.Context, id int) error {
	const q = `UPDATE llm_providers SET deleted_at = CURRENT_TIMESTAMP WHERE id = $1 AND deleted_at IS NULL`
	_, err := d.conn.Exec(ctx, q, id)
	return err
}
