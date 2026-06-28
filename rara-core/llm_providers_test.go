package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rara-core/internal/secretbox"
)

// ---------------------------------------------------------------------------
// MockDatabase — LLMProvider methods (implements Database interface extension)
// ---------------------------------------------------------------------------

func (m *MockDatabase) UpsertLLMProvider(_ context.Context, name, kind, baseURL string,
	keyCiphertext, keyNonce []byte, keyLast4 string, enabled bool) (int, error) {
	// Find existing active row (upsert by name, owner_id=NULL).
	for i, p := range m.llmProviders {
		if p.Name == name && p.DeletedAt == nil {
			m.llmProviders[i].KeyCiphertext = keyCiphertext
			m.llmProviders[i].KeyNonce = keyNonce
			m.llmProviders[i].KeyLast4 = keyLast4
			m.llmProviders[i].Kind = kind
			m.llmProviders[i].BaseURL = baseURL
			m.llmProviders[i].Enabled = enabled
			return p.ID, nil
		}
	}
	id := m.nextLLMProviderID
	m.nextLLMProviderID++
	m.llmProviders = append(m.llmProviders, mockLLMProvider{
		ID: id, Name: name, Kind: kind, BaseURL: baseURL,
		KeyCiphertext: keyCiphertext, KeyNonce: keyNonce, KeyLast4: keyLast4,
		Enabled: enabled,
	})
	return id, nil
}

func (m *MockDatabase) UpdateLLMProviderFields(_ context.Context, name, kind, baseURL string, enabled bool) error {
	for i, p := range m.llmProviders {
		if p.Name == name && p.DeletedAt == nil {
			m.llmProviders[i].Kind = kind
			m.llmProviders[i].BaseURL = baseURL
			m.llmProviders[i].Enabled = enabled
			return nil
		}
	}
	return errNotFound
}

func (m *MockDatabase) ListLLMProviders(_ context.Context) ([]LLMProviderRow, error) {
	var out []LLMProviderRow
	for _, p := range m.llmProviders {
		if p.DeletedAt != nil {
			continue
		}
		out = append(out, LLMProviderRow{
			ID:       p.ID,
			Name:     p.Name,
			Kind:     p.Kind,
			BaseURL:  p.BaseURL,
			KeyLast4: p.KeyLast4,
			Enabled:  p.Enabled,
			// KeyCiphertext/KeyNonce intentionally omitted (nil).
		})
	}
	return out, nil
}

func (m *MockDatabase) DeleteLLMProvider(_ context.Context, id int) error {
	t := true
	for i, p := range m.llmProviders {
		if p.ID == id {
			m.llmProviders[i].DeletedAt = &t
			return nil
		}
	}
	return nil // no-op for unknown id (mirrors SQL UPDATE affecting 0 rows)
}

// getLLMProviderRaw returns the raw secret fields for decryption tests.
// Not part of the Database interface — only used by test code.
func (m *MockDatabase) getLLMProviderRaw(_ context.Context, name string) (llmProviderRaw, error) {
	for _, p := range m.llmProviders {
		if p.Name == name && p.DeletedAt == nil {
			return llmProviderRaw{KeyCiphertext: p.KeyCiphertext, KeyNonce: p.KeyNonce}, nil
		}
	}
	return llmProviderRaw{}, errNotFound
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testBox returns a deterministic secretbox.Box for tests (32 zero bytes).
func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(bytes.Repeat([]byte{0}, 32))
	if err != nil {
		t.Fatalf("testBox: %v", err)
	}
	return b
}

// newTestCoreWithBox wires a Core + secretbox over a fresh MockDatabase.
func newTestCoreWithBox(t *testing.T) (*Core, *MockDatabase, *secretbox.Box) {
	t.Helper()
	b := testBox(t)
	db := newMockDatabase()
	store := newFakeLinkedInStore()
	core := NewCore(db, store)
	core.box = b
	return core, db, b
}

// ---------------------------------------------------------------------------
// Core.UpsertLLMProvider — encrypt on write, mask on read
// ---------------------------------------------------------------------------

func TestUpsertLLMProviderEncryptsKey(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)

	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "groq-main",
		Kind:   "groq",
		APIKey: "testkey-groq-1234567890", // gitleaks:allow
	}); err != nil {
		t.Fatalf("UpsertLLMProvider: %v", err)
	}

	all, err := db.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("ListLLMProviders: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 provider, got %d", len(all))
	}
	p := all[0]
	if p.KeyLast4 != "7890" {
		t.Errorf("KeyLast4 = %q, want %q", p.KeyLast4, "7890")
	}
	// key_last4 is safe to surface; raw ciphertext must never appear in the DTO
	if p.KeyCiphertext != nil {
		t.Error("GET must not return key_ciphertext")
	}
	if p.KeyNonce != nil {
		t.Error("GET must not return key_nonce")
	}
}

func TestListLLMProvidersNeverReturnsKey(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)

	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "gemini-main",
		Kind:   "gemini",
		APIKey: "testkey-gemini-XXXX1234",
	}); err != nil {
		t.Fatalf("setup: UpsertLLMProvider: %v", err)
	}

	providers, err := db.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("ListLLMProviders: %v", err)
	}
	for _, p := range providers {
		if p.KeyCiphertext != nil || p.KeyNonce != nil {
			t.Errorf("provider %q leaks raw key bytes in GET response", p.Name)
		}
	}
}

func TestUpsertLLMProviderPreservesKeyOnEdit(t *testing.T) {
	ctx := context.Background()
	core, db, box := newTestCoreWithBox(t)

	// Create with a key.
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "openai-main",
		Kind:   "openai",
		APIKey: "testkey-original-9999",
	}); err != nil {
		t.Fatalf("setup: UpsertLLMProvider: %v", err)
	}

	// Edit without sending api_key — must preserve the stored ciphertext.
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "openai-main",
		Kind:   "openai",
		APIKey: "", // empty = no-op on key
	}); err != nil {
		t.Fatalf("UpsertLLMProvider edit without key: %v", err)
	}

	// Decrypt and verify original key is still there.
	raw, err := db.getLLMProviderRaw(ctx, "openai-main")
	if err != nil {
		t.Fatalf("getLLMProviderRaw: %v", err)
	}
	decrypted, err := box.Decrypt(raw.KeyCiphertext, raw.KeyNonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(decrypted) != "testkey-original-9999" {
		t.Errorf("decrypted key = %q, want original", string(decrypted))
	}
}

func TestUpsertLLMProviderRequiresKeyOnCreate(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCoreWithBox(t)

	// No existing provider — empty api_key must be rejected.
	err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "new-provider",
		Kind:   "groq",
		APIKey: "",
	})
	if err == nil {
		t.Fatal("expected error when api_key is empty and provider does not exist")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMProviderRejectsSSRFBaseURL(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCoreWithBox(t)

	err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:    "local-llm",
		Kind:    "openai_compatible",
		BaseURL: "http://localhost:11434",
		APIKey:  "testkey-localllm",
	})
	if err == nil {
		t.Fatal("expected error for loopback base_url (SSRF risk)")
	}
}

func TestUpsertLLMProviderSoftDelete(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCoreWithBox(t)

	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "to-delete", Kind: "anthropic", APIKey: "key"}); err != nil {
		t.Fatalf("setup: UpsertLLMProvider: %v", err)
	}

	if err := db.DeleteLLMProvider(ctx, 1); err != nil {
		t.Fatalf("DeleteLLMProvider: %v", err)
	}

	all, err := db.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("ListLLMProviders: %v", err)
	}
	for _, p := range all {
		if p.Name == "to-delete" {
			t.Error("soft-deleted provider should not appear in list")
		}
	}
}

func TestUpsertLLMProviderInvalidKind(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCoreWithBox(t)

	err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:   "bad-kind",
		Kind:   "unknown_vendor",
		APIKey: "key",
	})
	if err == nil {
		t.Fatal("invalid kind should return error")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestUpsertLLMProviderOpenAICompatibleRequiresBaseURL(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCoreWithBox(t)

	err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name:    "local-llm",
		Kind:    "openai_compatible",
		BaseURL: "", // missing
		APIKey:  "k",
	})
	if err == nil {
		t.Fatal("openai_compatible without base_url should return error")
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func TestHTTPListLLMProviders(t *testing.T) {
	core, _, _ := newTestCoreWithBox(t)
	ctx := context.Background()
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "g", Kind: "groq", APIKey: "testkey-zero-0000"}); err != nil {
		t.Fatalf("setup: UpsertLLMProvider: %v", err)
	}

	mux := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest(http.MethodGet, "/v1/llm-providers", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	// key_last4 should be present
	if !strings.Contains(body, "0000") {
		t.Errorf("response should contain key_last4=0000, got: %s", body)
	}
	// raw key must not leak
	if strings.Contains(body, "testkey-zero-0000") {
		t.Error("plaintext API key must not appear in list response")
	}
}

func TestHTTPUpsertLLMProvider(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)

	mux := NewSurfaceMux(core, testToken)
	body := `{"name":"groq-main","kind":"groq","api_key":"testkey-abc-1234"}` // gitleaks:allow
	req := httptest.NewRequest(http.MethodPut, "/v1/llm-providers", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	all, err := db.ListLLMProviders(context.Background())
	if err != nil {
		t.Fatalf("ListLLMProviders: %v", err)
	}
	if len(all) != 1 || all[0].Name != "groq-main" {
		t.Errorf("provider not persisted: %+v", all)
	}
}

func TestHTTPDeleteLLMProvider(t *testing.T) {
	core, db, _ := newTestCoreWithBox(t)
	ctx := context.Background()
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{Name: "bye", Kind: "gemini", APIKey: "k"}); err != nil {
		t.Fatalf("setup: UpsertLLMProvider: %v", err)
	}

	mux := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest(http.MethodDelete, "/v1/llm-providers/1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	all, err := db.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("ListLLMProviders: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("want empty list after delete, got %d", len(all))
	}
}

func TestHTTPUpsertLLMProviderInvalidKind(t *testing.T) {
	core, _, _ := newTestCoreWithBox(t)

	mux := NewSurfaceMux(core, testToken)
	body := `{"name":"bad","kind":"alien","api_key":"k"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/llm-providers", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid kind, got %d", w.Code)
	}
}
