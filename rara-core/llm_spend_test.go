package main

// llm_spend_test.go — tests for GET /v1/llm-spend (CONSOLE-INFER-#9).
// Real cost/tokens per model alias, read straight from the litellm spend log
// (litellm."LiteLLM_SpendLogs", aggregated by model_group = the worker alias).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	// Order must mirror the pgx ORDER BY SUM(spend) DESC: gemini-flash (0.010) before groq-llama (0.004).
	if rows[0].Model != "gemini-flash" || rows[1].Model != "groq-llama" {
		t.Errorf("order = [%s, %s], want [gemini-flash, groq-llama]", rows[0].Model, rows[1].Model)
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
	// Body must be [] (non-nil), not null — the frontend filters/aggregates over an array.
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("want body []; got %q", got)
	}
	var rows []LLMSpend
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode []LLMSpend: %v", err)
	}
	if rows == nil {
		t.Error("want non-nil empty slice, got nil (would serialize as null)")
	}
	if len(rows) != 0 {
		t.Errorf("want empty slice with no spend, got %+v", rows)
	}
}

// --- CORR-INFER-#4: timeseries + by-provider aggregations -------------------

// TestLLMSpendTimeseriesByDay: rows fold into one entry per calendar day, oldest first.
func TestLLMSpendTimeseriesByDay(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	day1 := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	day1b := time.Date(2026, 6, 20, 22, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	seedSpendLog(db, "groq/llama", 0.001, 100, 50, day1)
	seedSpendLog(db, "gemini/flash", 0.003, 200, 70, day1b)
	seedSpendLog(db, "groq/llama", 0.010, 1000, 500, day2)
	seedSpendLog(db, "", 0, 0, 0, day2) // liveness probe — excluded

	rows, err := db.LLMSpendTimeseries(ctx, nil)
	if err != nil {
		t.Fatalf("LLMSpendTimeseries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 days, got %d (%+v)", len(rows), rows)
	}
	if rows[0].Day != "2026-06-20" || rows[1].Day != "2026-06-21" {
		t.Fatalf("days = [%s, %s], want chronological [2026-06-20, 2026-06-21]", rows[0].Day, rows[1].Day)
	}
	if rows[0].Requests != 2 {
		t.Errorf("day1 requests=%d, want 2", rows[0].Requests)
	}
	if rows[0].Spend < 0.0039 || rows[0].Spend > 0.0041 {
		t.Errorf("day1 spend=%v, want ~0.004", rows[0].Spend)
	}
	if rows[0].TotalTokens != 420 {
		t.Errorf("day1 total_tokens=%d, want 420", rows[0].TotalTokens)
	}
}

// TestLLMSpendTimeseriesSinceFilter: a since cutoff drops older days.
func TestLLMSpendTimeseriesSinceFilter(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	seedSpendLog(db, "groq/llama", 1.0, 10, 10, time.Now().Add(-30*24*time.Hour))
	seedSpendLog(db, "groq/llama", 2.0, 20, 20, time.Now())

	since := time.Now().Add(-7 * 24 * time.Hour)
	rows, err := db.LLMSpendTimeseries(ctx, &since)
	if err != nil {
		t.Fatalf("LLMSpendTimeseries: %v", err)
	}
	var total float64
	var reqs int
	for _, r := range rows {
		total += r.Spend
		reqs += r.Requests
	}
	if reqs != 1 {
		t.Errorf("requests=%d, want 1 (old day excluded)", reqs)
	}
	if total < 1.99 || total > 2.01 {
		t.Errorf("spend=%v, want ~2.0", total)
	}
}

// TestLLMSpendByProviderGroupsByPrefix: model_group folds to its prefix before "/", spend DESC.
func TestLLMSpendByProviderGroupsByPrefix(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	now := time.Now()
	seedSpendLog(db, "groq/llama-3.3-70b", 0.001, 100, 50, now)
	seedSpendLog(db, "groq/llama-3.1-8b", 0.003, 200, 70, now)
	seedSpendLog(db, "gemini/2.5-flash", 0.010, 1000, 500, now)
	seedSpendLog(db, "", 0, 0, 0, now) // excluded

	rows, err := db.LLMSpendByProvider(ctx, nil)
	if err != nil {
		t.Fatalf("LLMSpendByProvider: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 providers, got %d (%+v)", len(rows), rows)
	}
	// ORDER BY spend DESC: gemini (0.010) before groq (0.004).
	if rows[0].Provider != "gemini" || rows[1].Provider != "groq" {
		t.Fatalf("providers = [%s, %s], want [gemini, groq]", rows[0].Provider, rows[1].Provider)
	}
	groq := rows[1]
	if groq.Requests != 2 {
		t.Errorf("groq requests=%d, want 2", groq.Requests)
	}
	if groq.Spend < 0.0039 || groq.Spend > 0.0041 {
		t.Errorf("groq spend=%v, want ~0.004", groq.Spend)
	}
	if groq.TotalTokens != 420 {
		t.Errorf("groq total_tokens=%d, want 420", groq.TotalTokens)
	}
}

// TestLLMSpendByProviderAliasWithoutSlash: an old alias (no "/") stays whole,
// mirroring split_part(model_group,'/',1) which returns the string intact.
func TestLLMSpendByProviderAliasWithoutSlash(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	seedSpendLog(db, "groq-llama", 0.002, 10, 5, time.Now())
	rows, err := db.LLMSpendByProvider(ctx, nil)
	if err != nil {
		t.Fatalf("LLMSpendByProvider: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "groq-llama" {
		t.Fatalf("want provider groq-llama (whole), got %+v", rows)
	}
}

// TestHTTPLLMSpendTimeseries200: the endpoint returns []LLMSpendDay.
func TestHTTPLLMSpendTimeseries200(t *testing.T) {
	core, db, _ := newTestCore(t)
	seedSpendLog(db, "groq/llama", 0.005, 100, 50, time.Now())
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/llm-spend/timeseries", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rows []LLMSpendDay
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode []LLMSpendDay: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 day, got %+v", rows)
	}
}

// TestHTTPLLMSpendByProvider200: the endpoint returns []LLMSpendProvider.
func TestHTTPLLMSpendByProvider200(t *testing.T) {
	core, db, _ := newTestCore(t)
	seedSpendLog(db, "groq/llama", 0.005, 100, 50, time.Now())
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/llm-spend/by-provider", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rows []LLMSpendProvider
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode []LLMSpendProvider: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "groq" {
		t.Fatalf("want groq, got %+v", rows)
	}
}

// TestHTTPLLMSpendChartsInvalidDays: both chart endpoints reject bad days with 400.
func TestHTTPLLMSpendChartsInvalidDays(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	for _, path := range []string{"/v1/llm-spend/timeseries", "/v1/llm-spend/by-provider"} {
		for _, raw := range []string{"abc", "0", "-1", "366", "999999"} {
			rec := do(t, h, http.MethodGet, path+"?days="+raw, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s days=%q should be 400, got %d: %s", path, raw, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestHTTPLLMSpendChartsNoData: empty spend log yields [] (not null) for both endpoints.
func TestHTTPLLMSpendChartsNoData(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	for _, path := range []string{"/v1/llm-spend/timeseries", "/v1/llm-spend/by-provider"} {
		rec := do(t, h, http.MethodGet, path+"?days=7", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s want 200 on empty, got %d: %s", path, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
			t.Fatalf("%s want body []; got %q", path, got)
		}
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
