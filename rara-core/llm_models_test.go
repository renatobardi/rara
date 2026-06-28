package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// MockDatabase — LLMModel methods (implements Database interface extension)
// ---------------------------------------------------------------------------

func (m *MockDatabase) GetLLMProvider(_ context.Context, id int) (LLMProviderRow, bool, error) {
	for _, p := range m.llmProviders {
		if p.ID == id && p.DeletedAt == nil {
			return LLMProviderRow{
				ID:      p.ID,
				Name:    p.Name,
				Kind:    p.Kind,
				BaseURL: p.BaseURL,
				Enabled: p.Enabled,
			}, true, nil
		}
	}
	return LLMProviderRow{}, false, nil
}

func (m *MockDatabase) UpsertLLMModel(_ context.Context, in llmModelUpsert) (int, error) {
	// Mirror the SQL INSERT ... SELECT guard: provider must be active (not soft-deleted
	// AND enabled) at persist time, else the write is rejected as errNotFound.
	providerName := ""
	found := false
	for _, p := range m.llmProviders {
		if p.ID == in.ProviderID && p.DeletedAt == nil && p.Enabled {
			providerName = p.Name
			found = true
			break
		}
	}
	if !found {
		return 0, errNotFound
	}
	// Upsert by (owner_id=NULL, alias): update existing active row if found.
	for i, mdl := range m.llmModels {
		if mdl.Alias == in.Alias && mdl.DeletedAt == nil {
			m.llmModels[i].ProviderID = in.ProviderID
			m.llmModels[i].ProviderName = providerName
			m.llmModels[i].Upstream = in.Upstream
			m.llmModels[i].InputCost = in.InputCost
			m.llmModels[i].OutputCost = in.OutputCost
			m.llmModels[i].Params = in.Params
			m.llmModels[i].Enabled = in.Enabled
			return mdl.ID, nil
		}
	}
	id := m.nextLLMModelID
	m.nextLLMModelID++
	m.llmModels = append(m.llmModels, mockLLMModel{
		ID: id, ProviderID: in.ProviderID, ProviderName: providerName,
		Alias: in.Alias, Upstream: in.Upstream,
		InputCost: in.InputCost, OutputCost: in.OutputCost,
		Params: in.Params, Enabled: in.Enabled,
	})
	return id, nil
}

func (m *MockDatabase) ListLLMModels(_ context.Context, providerID int) ([]LLMModelRow, error) {
	var out []LLMModelRow
	for _, mdl := range m.llmModels {
		if mdl.DeletedAt != nil {
			continue
		}
		if providerID > 0 && mdl.ProviderID != providerID {
			continue
		}
		// Mirror the SQL JOIN ... AND p.deleted_at IS NULL: a model whose provider was
		// soft-deleted is orphaned config and must not appear.
		if m.providerSoftDeleted(mdl.ProviderID) {
			continue
		}
		out = append(out, LLMModelRow{
			ID:                 mdl.ID,
			ProviderID:         mdl.ProviderID,
			ProviderName:       mdl.ProviderName,
			Alias:              mdl.Alias,
			Upstream:           mdl.Upstream,
			InputCostPerToken:  mdl.InputCost,
			OutputCostPerToken: mdl.OutputCost,
			Params:             mdl.Params,
			Enabled:            mdl.Enabled,
		})
	}
	return out, nil
}

func (m *MockDatabase) ListEnabledLLMModelsForSync(_ context.Context) ([]llmModelSync, error) {
	var out []llmModelSync
	for _, mdl := range m.llmModels {
		if mdl.DeletedAt != nil || !mdl.Enabled {
			continue
		}
		for _, p := range m.llmProviders {
			if p.ID != mdl.ProviderID || p.DeletedAt != nil || !p.Enabled {
				continue
			}
			out = append(out, llmModelSync{
				Alias:         mdl.Alias,
				Upstream:      mdl.Upstream,
				ProviderKind:  p.Kind,
				BaseURL:       p.BaseURL,
				KeyCiphertext: p.KeyCiphertext,
				KeyNonce:      p.KeyNonce,
				InputCost:     mdl.InputCost,
				OutputCost:    mdl.OutputCost,
				Params:        mdl.Params,
			})
			break
		}
	}
	return out, nil
}

// providerSoftDeleted reports whether the provider with id is soft-deleted (or absent).
func (m *MockDatabase) providerSoftDeleted(id int) bool {
	for _, p := range m.llmProviders {
		if p.ID == id {
			return p.DeletedAt != nil
		}
	}
	return true // unknown provider == orphaned
}

func (m *MockDatabase) DeleteLLMModel(_ context.Context, id int) error {
	t := true
	for i, mdl := range m.llmModels {
		if mdl.ID == id {
			m.llmModels[i].DeletedAt = &t
			return nil
		}
	}
	return nil // no-op for unknown id (mirrors SQL UPDATE affecting 0 rows)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// listModels fetches models through the mock and fails the test on error — keeps the
// call sites from swallowing a real regression behind a discarded error.
func listModels(t *testing.T, db *MockDatabase, providerID int) []LLMModelRow {
	t.Helper()
	out, err := db.ListLLMModels(context.Background(), providerID)
	if err != nil {
		t.Fatalf("ListLLMModels(%d): %v", providerID, err)
	}
	return out
}

// mustUpsertModel upserts a model and fails the test on error.
func mustUpsertModel(t *testing.T, core *Core, in LLMModelInput) {
	t.Helper()
	if err := core.UpsertLLMModel(context.Background(), in); err != nil {
		t.Fatalf("UpsertLLMModel(%q): %v", in.Alias, err)
	}
}

// seedProvider creates a groq provider in the mock and returns its id (always 1 on fresh db).
func seedProvider(t *testing.T, core *Core, db *MockDatabase) int {
	t.Helper()
	ctx := context.Background()
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "groq-test",
		Kind:   "groq",
		APIKey: "testkey-groq-seed-1234", // gitleaks:allow
	}); err != nil {
		t.Fatalf("seedProvider: %v", err)
	}
	providers, err := db.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("seedProvider list: %v", err)
	}
	if len(providers) == 0 {
		t.Fatal("seedProvider: provider not persisted")
	}
	return providers[0].ID
}

// ---------------------------------------------------------------------------
// Core.UpsertLLMModel — validation + upsert behaviour
// ---------------------------------------------------------------------------

func TestUpsertLLMModel(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid,
		Alias:      "llama-fast",
		Upstream:   "groq/llama-3.3-70b-versatile",
	}); err != nil {
		t.Fatalf("UpsertLLMModel: %v", err)
	}

	all, err := db.ListLLMModels(ctx, 0)
	if err != nil {
		t.Fatalf("ListLLMModels: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 model, got %d", len(all))
	}
	m := all[0]
	if m.Alias != "llama-fast" || m.ProviderName != "groq-test" {
		t.Errorf("model = %+v, want alias=llama-fast provider_name=groq-test", m)
	}
}

func TestUpsertLLMModelSameAliasUpdates(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	upsert := func(upstream string) {
		t.Helper()
		if err := core.UpsertLLMModel(ctx, LLMModelInput{
			ProviderID: pid, Alias: "same-alias", Upstream: upstream,
		}); err != nil {
			t.Fatalf("UpsertLLMModel(%q): %v", upstream, err)
		}
	}

	upsert("groq/llama-3.3-70b-versatile")
	upsert("groq/llama-3.1-8b-instant") // re-upsert with same alias

	all := listModels(t, db, 0)
	if len(all) != 1 {
		t.Fatalf("want 1 model after re-upsert, got %d", len(all))
	}
	if all[0].Upstream != "groq/llama-3.1-8b-instant" {
		t.Errorf("upstream not updated: got %q", all[0].Upstream)
	}
}

func TestUpsertLLMModelAliasFreeAfterSoftDelete(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "reusable", Upstream: "groq/v1",
	}); err != nil {
		t.Fatalf("setup upsert: %v", err)
	}
	if err := db.DeleteLLMModel(ctx, 1); err != nil {
		t.Fatalf("DeleteLLMModel: %v", err)
	}

	// Same alias must be usable after soft-delete (partial index).
	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "reusable", Upstream: "groq/v2",
	}); err != nil {
		t.Fatalf("re-create after soft-delete: %v", err)
	}

	all := listModels(t, db, 0)
	if len(all) != 1 || all[0].Upstream != "groq/v2" {
		t.Errorf("want one active model groq/v2, got %+v", all)
	}
}

func TestUpsertLLMModelSoftDelete(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "to-delete", Upstream: "groq/v1",
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := db.DeleteLLMModel(ctx, 1); err != nil {
		t.Fatalf("DeleteLLMModel: %v", err)
	}

	all := listModels(t, db, 0)
	if len(all) != 0 {
		t.Error("soft-deleted model should not appear in list")
	}
}

func TestUpsertLLMModelNegativeInputCost(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "m", Upstream: "u", InputCostPerToken: -0.001,
	})
	if err == nil {
		t.Fatal("expected error for negative input_cost_per_token")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMModelNegativeOutputCost(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "m", Upstream: "u", OutputCostPerToken: -1,
	})
	if err == nil {
		t.Fatal("expected error for negative output_cost_per_token")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMModelProviderNotFound(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCoreWithBox(t)

	err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: 999, Alias: "m", Upstream: "u",
	})
	if err == nil {
		t.Fatal("expected error for non-existent provider_id")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMModelProviderDisabled(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	// Disable the provider directly in the mock.
	for i := range db.llmProviders {
		if db.llmProviders[i].ID == pid {
			db.llmProviders[i].Enabled = false
		}
	}

	err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "m", Upstream: "u",
	})
	if err == nil {
		t.Fatal("expected error for disabled provider")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMModelInvalidParams(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid,
		Alias:      "m",
		Upstream:   "u",
		Params:     json.RawMessage(`["not","an","object"]`),
	})
	if err == nil {
		t.Fatal("expected error for non-object params")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMModelEmptyAlias(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	err := core.UpsertLLMModel(ctx, LLMModelInput{ProviderID: pid, Alias: "", Upstream: "u"})
	if err == nil {
		t.Fatal("expected error for empty alias")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMModelEmptyUpstream(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	err := core.UpsertLLMModel(ctx, LLMModelInput{ProviderID: pid, Alias: "a", Upstream: ""})
	if err == nil {
		t.Fatal("expected error for empty upstream")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestListLLMModelsFilterByProvider(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)

	// Create two providers.
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "groq", Kind: "groq", APIKey: "k1"}); err != nil { // gitleaks:allow
		t.Fatalf("seed groq: %v", err)
	}
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "gemini", Kind: "gemini", APIKey: "k2"}); err != nil { // gitleaks:allow
		t.Fatalf("seed gemini: %v", err)
	}

	providers, err := db.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("ListLLMProviders: %v", err)
	}
	pid1, pid2 := providers[0].ID, providers[1].ID

	mustUpsertModel(t, core, LLMModelInput{ProviderID: pid1, Alias: "fast", Upstream: "groq/fast"})
	mustUpsertModel(t, core, LLMModelInput{ProviderID: pid2, Alias: "flash", Upstream: "gemini/flash"})

	all := listModels(t, db, 0)
	if len(all) != 2 {
		t.Fatalf("want 2 total models, got %d", len(all))
	}

	filtered := listModels(t, db, pid1)
	if len(filtered) != 1 || filtered[0].Alias != "fast" {
		t.Errorf("filtered by provider 1: got %+v", filtered)
	}
}

func TestUpsertLLMModelTrimsWhitespace(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "  fast  ", Upstream: "  groq/fast  ",
	}); err != nil {
		t.Fatalf("UpsertLLMModel: %v", err)
	}
	all := listModels(t, db, 0)
	if len(all) != 1 || all[0].Alias != "fast" || all[0].Upstream != "groq/fast" {
		t.Errorf("want trimmed alias/upstream persisted, got %+v", all)
	}
}

func TestUpsertLLMModelBlankAliasRejected(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	err := core.UpsertLLMModel(ctx, LLMModelInput{ProviderID: pid, Alias: "   ", Upstream: "groq/x"})
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Fatalf("want badInputError for blank alias, got %T: %v", err, err)
	}
	if all := listModels(t, db, 0); len(all) != 0 {
		t.Errorf("blank-alias model must not persist, got %+v", all)
	}
}

// TestUpsertLLMModelRejectsInactiveProviderAtPersist locks the TOCTOU guard: even if a
// caller reaches the storage layer with a provider that is no longer active (deleted or
// disabled after validation), the write is rejected as errNotFound.
func TestUpsertLLMModelRejectsInactiveProviderAtPersist(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	disabled := false
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name: "groq-test", Kind: "groq", Enabled: &disabled,
	}); err != nil {
		t.Fatalf("disable provider: %v", err)
	}

	_, err := db.UpsertLLMModel(ctx, llmModelUpsert{
		ProviderID: pid, Alias: "fast", Upstream: "groq/fast", Params: json.RawMessage("{}"),
	})
	if !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound persisting against disabled provider, got %v", err)
	}
}

func TestListLLMModelsExcludesSoftDeletedProvider(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)
	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "fast", Upstream: "groq/fast",
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	if err := core.DeleteLLMProvider(ctx, pid); err != nil {
		t.Fatalf("soft-delete provider: %v", err)
	}
	if all := listModels(t, db, 0); len(all) != 0 {
		t.Errorf("models of a soft-deleted provider must not list, got %+v", all)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func TestHTTPListLLMModels(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)
	ctx := context.Background()
	pid := seedProvider(t, core, db)
	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "llama", Upstream: "groq/llama-3.3-70b-versatile",
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	mux := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest(http.MethodGet, "/v1/llm-models", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "llama") {
		t.Errorf("response should contain alias, got: %s", w.Body)
	}
}

func TestHTTPListLLMModelsFilterByProvider(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)
	ctx := context.Background()

	// Two providers, two models — filter must return only the model for pid1.
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "groq", Kind: "groq", APIKey: "k1"}); err != nil { // gitleaks:allow
		t.Fatalf("seed groq: %v", err)
	}
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "gemini", Kind: "gemini", APIKey: "k2"}); err != nil { // gitleaks:allow
		t.Fatalf("seed gemini: %v", err)
	}
	providers, err := db.ListLLMProviders(ctx)
	if err != nil || len(providers) < 2 {
		t.Fatalf("setup providers: %v / %d", err, len(providers))
	}
	pid1, pid2 := providers[0].ID, providers[1].ID

	mustUpsertModel(t, core, LLMModelInput{ProviderID: pid1, Alias: "llama", Upstream: "groq/llama"})
	mustUpsertModel(t, core, LLMModelInput{ProviderID: pid2, Alias: "flash", Upstream: "gemini/flash"})

	mux := NewSurfaceMux(core, testToken)
	url := fmt.Sprintf("/v1/llm-models?provider_id=%d", pid1)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, "llama") {
		t.Errorf("response should contain llama (provider 1 model), got: %s", body)
	}
	if strings.Contains(body, "flash") {
		t.Errorf("response must not contain flash (provider 2 model), got: %s", body)
	}
}

func TestHTTPUpsertLLMModel(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	mux := NewSurfaceMux(core, testToken)
	body := fmt.Sprintf(`{"provider_id":%d,"alias":"llama","upstream":"groq/llama-3.3-70b-versatile"}`, pid)
	req := httptest.NewRequest(http.MethodPut, "/v1/llm-models", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	all := listModels(t, db, 0)
	if len(all) != 1 || all[0].Alias != "llama" {
		t.Errorf("model not persisted: %+v", all)
	}
}

func TestHTTPDeleteLLMModel(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)
	ctx := context.Background()
	pid := seedProvider(t, core, db)
	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: pid, Alias: "bye", Upstream: "groq/bye",
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := listModels(t, db, 0)
	if len(before) == 0 {
		t.Fatal("setup: model not persisted")
	}
	modelID := before[0].ID

	mux := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/v1/llm-models/%d", modelID), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	after := listModels(t, db, 0)
	if len(after) != 0 {
		t.Errorf("want empty list after delete, got %d", len(after))
	}
}

func TestHTTPUpsertLLMModelInvalidProvider(t *testing.T) {
	core, _, _ := newTestCoreWithBox(t)

	mux := NewSurfaceMux(core, testToken)
	body := `{"provider_id":999,"alias":"x","upstream":"u"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/llm-models", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid provider_id, got %d", w.Code)
	}
}

func TestHTTPUpsertLLMModelNegativeCost(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)
	pid := seedProvider(t, core, db)

	mux := NewSurfaceMux(core, testToken)
	body := fmt.Sprintf(`{"provider_id":%d,"alias":"x","upstream":"u","input_cost_per_token":-0.001}`, pid)
	req := httptest.NewRequest(http.MethodPut, "/v1/llm-models", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for negative cost, got %d", w.Code)
	}
}
