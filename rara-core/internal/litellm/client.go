// Package litellm is a thin admin-API client for the LiteLLM proxy gateway.
//
// It speaks only the handful of admin endpoints the registry reconciler needs:
// list the current models, add one, delete one, and a liveness probe. The reconciler
// (package main) owns the policy — this package only does HTTP + (de)serialization and
// is deliberately ignorant of llm_providers/llm_models.
//
// Secret hygiene: a model's api_key is write-only here. It is sent in the /model/new
// body and never logged, never returned by ListModels (the gateway masks it), and never
// placed in an error string.
package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fingerprintKey is the model_info field where the reconciler stashes a content hash of
// the desired model. The gateway echoes model_info verbatim, so reading it back on the
// next pass lets the reconciler detect drift without comparing masked/normalized fields.
const fingerprintKey = "rara_fp"

// Model is the gateway's view of one registered model — both the write payload for
// AddModel and the parsed read shape from ListModels. Only fields the reconciler uses
// are modeled; the gateway tolerates and preserves the rest.
type Model struct {
	ModelName   string         // alias workers send as LITELLM_MODEL → litellm "model_name"
	Upstream    string         // litellm_params.model, e.g. groq/llama-3.3-70b-versatile
	APIKey      string         // write-only, decrypted; never logged or read back
	APIBase     string         // litellm_params.api_base (BYO/openai_compatible)
	InputCost   float64        // litellm_params.input_cost_per_token
	OutputCost  float64        // litellm_params.output_cost_per_token
	Params      map[string]any // extra litellm_params (temperature, max_tokens, …)
	Fingerprint string         // model_info.rara_fp — content hash, set on write/read

	// Read-only, populated by ListModels (ignored on write):
	ID      string // model_info.id — the gateway-assigned id needed to delete
	DBModel bool   // model_info.db_model — true = managed via DB/API; false = config.yaml
}

// Client talks to one LiteLLM gateway with a master key.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New builds a client. baseURL and masterKey are required (fail-fast: the reconciler is
// the only writer of the gateway registry and must never run half-configured).
func New(baseURL, masterKey string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("litellm: base URL is required")
	}
	if masterKey == "" {
		return nil, fmt.Errorf("litellm: master key is required")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     masterKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Health probes /health/liveliness (no auth needed, but we send the bearer anyway).
func (c *Client) Health(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/health/liveliness", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("litellm: health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("litellm: health: status %d", resp.StatusCode)
	}
	return nil
}

// modelInfoResponse mirrors GET /model/info.
type modelInfoResponse struct {
	Data []struct {
		ModelName     string `json:"model_name"`
		LitellmParams struct {
			Model   string `json:"model"`
			APIBase string `json:"api_base"`
		} `json:"litellm_params"`
		ModelInfo map[string]any `json:"model_info"`
	} `json:"data"`
}

// ListModels returns every model the gateway currently serves, including config.yaml
// models (DBModel=false). The reconciler filters to DBModel=true before acting.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/model/info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("litellm: list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("litellm: list models: status %d: %s", resp.StatusCode, snippet(resp.Body))
	}
	var parsed modelInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("litellm: list models: decode: %w", err)
	}
	out := make([]Model, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		m := Model{
			ModelName: d.ModelName,
			Upstream:  d.LitellmParams.Model,
			APIBase:   d.LitellmParams.APIBase,
		}
		if d.ModelInfo != nil {
			if id, ok := d.ModelInfo["id"].(string); ok {
				m.ID = id
			}
			if db, ok := d.ModelInfo["db_model"].(bool); ok {
				m.DBModel = db
			}
			if fp, ok := d.ModelInfo[fingerprintKey].(string); ok {
				m.Fingerprint = fp
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// AddModel registers a model via POST /model/new. The api_key travels in the body and is
// never logged. Costs and the fingerprint are written so a later ListModels can detect drift.
func (c *Client) AddModel(ctx context.Context, m Model) error {
	params := map[string]any{"model": m.Upstream}
	if m.APIKey != "" {
		params["api_key"] = m.APIKey
	}
	if m.APIBase != "" {
		params["api_base"] = m.APIBase
	}
	if m.InputCost != 0 {
		params["input_cost_per_token"] = m.InputCost
	}
	if m.OutputCost != 0 {
		params["output_cost_per_token"] = m.OutputCost
	}
	for k, v := range m.Params {
		params[k] = v
	}
	body := map[string]any{
		"model_name":     m.ModelName,
		"litellm_params": params,
		"model_info":     map[string]any{fingerprintKey: m.Fingerprint},
	}
	// Status-only error (never echo the body — it carries the api_key).
	if err := c.post(ctx, "/model/new", body, m.ModelName); err != nil {
		return err
	}
	return nil
}

// DeleteModel removes a model via POST /model/delete keyed by its gateway id.
func (c *Client) DeleteModel(ctx context.Context, id string) error {
	return c.post(ctx, "/model/delete", map[string]any{"id": id}, id)
}

// post sends a JSON body and treats any non-2xx as an error. label identifies the target
// in the error without leaking the payload.
func (c *Client) post(ctx context.Context, path string, body any, label string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("litellm: %s: marshal: %w", path, err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("litellm: %s (%s): %w", path, label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("litellm: %s (%s): status %d", path, label, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("litellm: build request %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	return req, nil
}

// snippet reads a short, safe prefix of a response body for error context (GET/health only).
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(string(b))
}
