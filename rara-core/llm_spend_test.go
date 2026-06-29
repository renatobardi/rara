package main

// llm_spend_test.go — tests for GET /v1/llm-spend (CONSOLE-INFER-#9).
// Real cost/tokens per model alias, read straight from the litellm spend log
// (litellm."LiteLLM_SpendLogs", aggregated by model_group = the worker alias).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// seedSpendLog appends one spend-log row to the mock, mirroring a litellm
// LiteLLM_SpendLogs row. prompt+completion sum into total_tokens.
func seedSpendLog(db *MockDatabase, modelGroup string, spend float64, prompt, completion int, at time.Time) {
	db.spendLogs = append(db.spendLogs, mockSpendLog{
		ModelGroup:       modelGroup,
		Spend:            spend,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		StartTime:        at,
	})
}

// spendByModel indexes a []LLMSpend by alias for assertions.
func spendByModel(rows []LLMSpend) map[string]LLMSpend {
	m := make(map[string]LLMSpend, len(rows))
	for _, r := range rows {
		m[r.Model] = r
	}
	return m
}

// TestLLMSpendAggregatesByModelGroup: rows fold into one entry per alias, summing
// spend, tokens, and request count.
func TestLLMSpendAggregatesByModelGroup(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	now := time.Now()
	seedSpendLog(db, "groq-llama", 0.001, 100, 50, now)
	seedSpendLog(db, "groq-llama", 0.003, 200, 70, now)
	seedSpendLog(db, "gemini-flash", 0.010, 1000, 500, now)

	rows, err := db.LLMSpend(ctx, "", nil)
	if err != nil {
		t.Fatalf("LLMSpend: %v", err)
	}
	got := spendByModel(rows)
	if len(got) != 2 {
		t.Fatalf("want 2 aliases, got %d (%+v)", len(got), rows)
	}

	g := got["groq-llama"]
	if g.Requests != 2 {
		t.Errorf("groq-llama requests=%d, want 2", g.Requests)
	}
	if g.Spend < 0.0039 || g.Spend > 0.0041 {
		t.Errorf("groq-llama spend=%v, want ~0.004", g.Spend)
	}
	if g.PromptTokens != 300 || g.CompletionTokens != 120 || g.TotalTokens != 420 {
		t.Errorf("groq-llama tokens prompt=%d completion=%d total=%d, want 300/120/420",
			g.PromptTokens, g.CompletionTokens, g.TotalTokens)
	}
}

// TestLLMSpendExcludesEmptyModelGroup: health-check rows (empty model_group, zero
// spend) must not appear as a phantom alias.
func TestLLMSpendExcludesEmptyModelGroup(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	now := time.Now()
	seedSpendLog(db, "", 0, 0, 0, now) // liveness probe
	seedSpendLog(db, "groq-llama", 0.002, 10, 5, now)

	rows, err := db.LLMSpend(ctx, "", nil)
	if err != nil {
		t.Fatalf("LLMSpend: %v", err)
	}
	if _, ok := spendByModel(rows)[""]; ok {
		t.Errorf("empty model_group must be excluded, got %+v", rows)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 alias, got %d", len(rows))
	}
}

// TestLLMSpendSinceFilter: a since cutoff drops older rows.
func TestLLMSpendSinceFilter(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now()
	seedSpendLog(db, "groq-llama", 1.0, 10, 10, old)
	seedSpendLog(db, "groq-llama", 2.0, 20, 20, recent)

	since := time.Now().Add(-7 * 24 * time.Hour)
	rows, err := db.LLMSpend(ctx, "", &since)
	if err != nil {
		t.Fatalf("LLMSpend: %v", err)
	}
	got := spendByModel(rows)["groq-llama"]
	if got.Requests != 1 {
		t.Errorf("requests=%d, want 1 (old row excluded)", got.Requests)
	}
	if got.Spend < 1.99 || got.Spend > 2.01 {
		t.Errorf("spend=%v, want ~2.0", got.Spend)
	}
}

// TestLLMSpendModelFilter: a non-empty model restricts to that alias only.
func TestLLMSpendModelFilter(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	now := time.Now()
	seedSpendLog(db, "groq-llama", 0.001, 10, 5, now)
	seedSpendLog(db, "gemini-flash", 0.010, 100, 50, now)

	rows, err := db.LLMSpend(ctx, "gemini-flash", nil)
	if err != nil {
		t.Fatalf("LLMSpend: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "gemini-flash" {
		t.Fatalf("want only gemini-flash, got %+v", rows)
	}
}

// TestHTTPLLMSpend200: the endpoint returns []LLMSpend.
func TestHTTPLLMSpend200(t *testing.T) {
	core, db, _ := newTestCore(t)
	seedSpendLog(db, "groq-llama", 0.005, 100, 50, time.Now())
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/llm-spend", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rows []LLMSpend
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode []LLMSpend: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "groq-llama" {
		t.Fatalf("want groq-llama, got %+v", rows)
	}
}

// TestHTTPLLMSpendInvalidDays: non-integer or out-of-range days returns 400.
func TestHTTPLLMSpendInvalidDays(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	for _, raw := range []string{"abc", "0", "-1", "366", "999999"} {
		rec := do(t, h, http.MethodGet, "/v1/llm-spend?days="+raw, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("days=%q should be 400, got %d: %s", raw, rec.Code, rec.Body.String())
		}
	}
}

// TestHTTPLLMSpendNoData: an empty spend log yields an empty result, not an error.
func TestHTTPLLMSpendNoData(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/llm-spend?days=7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 on empty, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCoreLLMSpendWrapsDBError: Core.LLMSpend propagates db errors (cancelled ctx).
func TestCoreLLMSpendWrapsDBError(t *testing.T) {
	core, _, _ := newTestCore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := core.LLMSpend(ctx, "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
