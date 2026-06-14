// runners.go — the I/O edge of the reviser: the cross-agent distillation read and the LiteLLM
// narrator. Both are the two narrow seams ReviseProfile depends on; the pure logic each backs
// (parseDistillationStructured for the resolver, the engine for everything else) is what the unit
// tests cover, so these are deliberately minimal glue exercised by real runs, not unit tests.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

// ---------------------------------------------------------------------------
// pgx DistillationResolver — the read side of the reviser's attribution.
//
// Reads a distillation's `structured` JSONB by id (cross-agent SELECT, no FK — the 1.0 isolation
// convention: read a sibling's table through the shared database, never call it) and parses out
// its concepts/entities/author via the pure parseDistillationStructured.
// ---------------------------------------------------------------------------

type pgxDistillationResolver struct{ conn *pgx.Conn }

func newPgxDistillationResolver(conn *pgx.Conn) *pgxDistillationResolver {
	return &pgxDistillationResolver{conn: conn}
}

var _ DistillationResolver = (*pgxDistillationResolver)(nil)

func (r *pgxDistillationResolver) Resolve(ctx context.Context, distillationID string) (DistillationStructured, bool, error) {
	const q = `SELECT COALESCE(structured::text, '{}') FROM distillations WHERE id = $1`
	var raw string
	switch err := r.conn.QueryRow(ctx, q, distillationID).Scan(&raw); {
	case errors.Is(err, pgx.ErrNoRows):
		return DistillationStructured{}, false, nil
	case err != nil:
		return DistillationStructured{}, false, err
	}
	return parseDistillationStructured([]byte(raw)), true, nil
}

// ---------------------------------------------------------------------------
// LiteLLM narrator — the prose half of the hybrid reviser.
//
// Writes ONLY the natural-language narrative of a proposed profile (the gate LLM-judge's
// context); it never touches a structured field — the deterministic engine owns those. Same
// gateway/dialect as the gate judge (LITELLM_*), so the model is a swappable config value. A
// failure is non-fatal (ReviseProfile falls back to a template narrative).
// ---------------------------------------------------------------------------

type liteLLMNarrator struct {
	apiKey   string
	model    string
	endpoint string
	client   *http.Client
}

var _ ProfileNarrator = (*liteLLMNarrator)(nil)

func newLiteLLMNarrator() (*liteLLMNarrator, error) {
	base := os.Getenv("LITELLM_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("LITELLM_BASE_URL is required for the profile narrator")
	}
	return &liteLLMNarrator{
		apiKey:   os.Getenv("LITELLM_API_KEY"),
		model:    envOr("LITELLM_MODEL", "claude-sonnet-4-6"),
		endpoint: strings.TrimRight(base, "/") + "/chat/completions",
		client:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (n *liteLLMNarrator) Narrate(ctx context.Context, old, proposed revisedStructured) (string, error) {
	reqBody := map[string]any{
		"model": n.model,
		"messages": []any{
			map[string]any{"role": "system", "content": narratorSystemPrompt()},
			map[string]any{"role": "user", "content": narratorUserPrompt(old, proposed)},
		},
		"temperature": 0.3,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+n.apiKey)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("litellm narrator: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var lr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", err
	}
	if len(lr.Choices) == 0 {
		return "", fmt.Errorf("litellm narrator returned no choices")
	}
	return lr.Choices[0].Message.Content, nil
}

// narratorSystemPrompt frames the narrative task and FORBIDS inventing structure — the LLM
// describes only the terms it is given; the deterministic engine already decided them.
func narratorSystemPrompt() string {
	return "You write a short natural-language summary of a personal content-curation interest " +
		"profile, to be used as CONTEXT for a curation judge. Describe, in 2-4 sentences, what the " +
		"reader is interested in (topics, authors) and what they want to avoid (anti-topics). " +
		"Use ONLY the lists you are given — never add, infer, or invent a topic, author, or " +
		"interest that is not listed. Output prose only, no JSON, no lists."
}

// narratorUserPrompt presents the proposed structured profile (and the prior one for contrast).
// These lists are the deterministic engine's output — the narrator describes, never edits, them.
func narratorUserPrompt(old, proposed revisedStructured) string {
	var b strings.Builder
	b.WriteString("Proposed profile to summarize:\n")
	b.WriteString("- Topics: " + joinOrNone(proposed.Topics) + "\n")
	b.WriteString("- Authors: " + joinOrNone(proposed.Authors) + "\n")
	b.WriteString("- Anti-topics: " + joinOrNone(proposed.AntiTopics) + "\n")
	b.WriteString(fmt.Sprintf("- keep_threshold: %.2f\n\n", proposed.KeepThreshold))
	b.WriteString("Previous profile (for contrast only):\n")
	b.WriteString("- Topics: " + joinOrNone(old.Topics) + "\n")
	b.WriteString("- Authors: " + joinOrNone(old.Authors) + "\n")
	b.WriteString("- Anti-topics: " + joinOrNone(old.AntiTopics) + "\n")
	return b.String()
}
