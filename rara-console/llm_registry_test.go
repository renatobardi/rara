package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeLLMCore stands in for the rara-core surface's llm_providers endpoints. It enforces the
// bearer, records the forwarded method+path+body, and serves masked reads (only key_last4,
// never api_key) so the BFF tests can prove the secret never reaches the SPA.
func fakeLLMCore(t *testing.T, token string, method, path, body *string) *httptest.Server {
	t.Helper()
	record := func(w http.ResponseWriter, r *http.Request, resp string) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if method != nil {
			*method = r.Method
		}
		if path != nil {
			*path = r.URL.Path
		}
		if body != nil {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("fakeLLMCore: read request body: %v", err)
				http.Error(w, "read body", http.StatusInternalServerError)
				return
			}
			*body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(resp)); err != nil {
			t.Errorf("fakeLLMCore: write response: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/llm-providers", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `[{"id":1,"name":"openai","kind":"openai","key_last4":"7xyz","enabled":true}]`)
	})
	mux.HandleFunc("PUT /v1/llm-providers", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"id":1,"name":"openai","key_last4":"7xyz","enabled":true}`)
	})
	mux.HandleFunc("DELETE /v1/llm-providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"deleted":true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLLMProvidersProxiesGET(t *testing.T) {
	var method, path string
	core := fakeLLMCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleLLMProviders(rec, httptest.NewRequest("GET", "/api/llm-providers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	if method != "GET" || path != "/v1/llm-providers" {
		t.Errorf("forwarded %s %s, want GET /v1/llm-providers", method, path)
	}
}

// The core returns only key_last4; the BFF must pass the masked body through untouched and never
// reintroduce an api_key field.
func TestLLMProvidersGETNeverLeaksKey(t *testing.T) {
	core := fakeLLMCore(t, "secret", nil, nil, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleLLMProviders(rec, httptest.NewRequest("GET", "/api/llm-providers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !contains(body, "key_last4") {
		t.Errorf("response dropped key_last4: %s", body)
	}
	if contains(body, "api_key") {
		t.Errorf("response leaked an api_key field: %s", body)
	}
	if h := headerDump(rec); contains(h, "api_key") {
		t.Errorf("response headers leaked an api_key: %s", h)
	}
}

// headerDump flattens the recorded response headers so leak assertions can scan them as one string.
func headerDump(rec *httptest.ResponseRecorder) string {
	var b strings.Builder
	for k, vs := range rec.Result().Header {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(strings.Join(vs, ","))
		b.WriteString("\n")
	}
	return b.String()
}

func TestLLMProvidersNeverLeaksToken(t *testing.T) {
	core := fakeLLMCore(t, "supersecret", nil, nil, nil)
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleLLMProviders(rec, httptest.NewRequest("GET", "/api/llm-providers", nil))

	// Assert the happy path ran, else a handler that bailed before writing would pass vacuously.
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
	if h := headerDump(rec); contains(h, "supersecret") {
		t.Errorf("response headers leaked the surface token: %s", h)
	}
}

func TestLLMProvidersReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleLLMProviders(rec, httptest.NewRequest("GET", "/api/llm-providers", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rec.Code)
	}
}

func TestUpsertLLMProviderProxiesPUT(t *testing.T) {
	var method, path, body string
	core := fakeLLMCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"name":"openai","kind":"openai","api_key":"sk-live-abc"}`
	rec := httptest.NewRecorder()
	s.handleUpsertLLMProvider(rec, httptest.NewRequest("PUT", "/api/llm-providers", strings.NewReader(reqBody)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	if method != "PUT" || path != "/v1/llm-providers" {
		t.Errorf("forwarded %s %s, want PUT /v1/llm-providers", method, path)
	}
	if body != reqBody {
		t.Errorf("forwarded body=%s, want %s", body, reqBody)
	}
	// The write echo must not contain the plaintext key the SPA sent up.
	if contains(rec.Body.String(), "sk-live-abc") {
		t.Errorf("response echoed the plaintext api_key: %s", rec.Body.String())
	}
}

func TestUpsertLLMProviderPropagates4xx(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
	}))
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleUpsertLLMProvider(rec, httptest.NewRequest("PUT", "/api/llm-providers", strings.NewReader(`{"name":""}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (core validation must propagate)", rec.Code)
	}
}

func TestDeleteLLMProviderProxiesIDInPath(t *testing.T) {
	var method, path string
	core := fakeLLMCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/llm-providers/7", "", map[string]string{"id": "7"})
	s.handleDeleteLLMProvider(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	if method != "DELETE" || path != "/v1/llm-providers/7" {
		t.Errorf("forwarded %s %s, want DELETE /v1/llm-providers/7", method, path)
	}
}

func TestDeleteLLMProviderRejectsBadID(t *testing.T) {
	var path string
	core := fakeLLMCore(t, "secret", nil, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/llm-providers/abc", "", map[string]string{"id": "abc"})
	s.handleDeleteLLMProvider(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for non-numeric id", rec.Code)
	}
	if path != "" {
		t.Errorf("bad id reached upstream at %q, want no upstream call", path)
	}
}

// The collection paths are method-scoped (GET list, PUT upsert); an unsupported method must get a
// 405 from the router, not fall through to a handler. This mux mirrors the registrations in main().
func TestLLMRegistryUnsupportedMethodReturns405(t *testing.T) {
	core := fakeLLMCore(t, "secret", nil, nil, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/llm-providers", s.handleLLMProviders)
	mux.HandleFunc("PUT /api/llm-providers", s.handleUpsertLLMProvider)
	mux.HandleFunc("DELETE /api/llm-providers/{id}", s.handleDeleteLLMProvider)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/llm-providers", strings.NewReader(`{}`)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/llm-providers: status=%d, want 405", rec.Code)
	}
}
