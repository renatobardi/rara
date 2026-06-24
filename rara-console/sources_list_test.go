package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeSourcesCore stands in for the rara-core surface's unified-source reads (fatia #1).
// It demands the bearer token and records the raw query string it received so a test can assert
// the BFF forwarded only the whitelisted filter/pagination params (no injection).
func fakeSourcesCore(t *testing.T, token string, gotQuery *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sources", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if gotQuery != nil {
			*gotQuery = r.URL.RawQuery
		}
		_, _ = w.Write([]byte(`{"items":[{"api_id":"podcast:1","kind":"podcast","lane":"podcast","display_name":"Lex","tags":[],"status":"active","config_summary":"https://feed","created_at":"2026-06-24T00:00:00Z","updated_at":"2026-06-24T00:00:00Z"}],"page":1,"page_size":20,"total":1,"counts":{"by_status":{"active":1},"by_kind":{"podcast":1}}}`))
	})
	mux.HandleFunc("GET /v1/source-kinds", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"kind":"podcast","label":"Podcast Feed","lane":"podcast","icon":"podcast","target_app":"rara-dial","supports_pause":true,"supports_tags":true,"fields":[]}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSourcesProxiesListWithTokenInjected(t *testing.T) {
	core := fakeSourcesCore(t, "secret", nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleSources(rec, httptest.NewRequest("GET", "/api/sources", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !contains(body, `"api_id":"podcast:1"`) {
		t.Errorf("response did not pass through the source item: %s", body)
	}
}

func TestSourcesForwardsOnlyWhitelistedQueryParams(t *testing.T) {
	var got string
	core := fakeSourcesCore(t, "secret", &got)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	// status= is whitelisted and forwarded; evil= is dropped so it can't reach the upstream.
	r := httptest.NewRequest("GET", "/api/sources?kind=podcast&status=active&tag=x&q=lex&page=2&page_size=50&evil=1", nil)
	s.handleSources(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	q, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("parse forwarded query %q: %v", got, err)
	}
	for k, want := range map[string]string{
		"kind": "podcast", "status": "active", "tag": "x", "q": "lex", "page": "2", "page_size": "50",
	} {
		if q.Get(k) != want {
			t.Errorf("forwarded %s = %q, want %q (full query=%q)", k, q.Get(k), want, got)
		}
	}
	if q.Has("evil") {
		t.Errorf("forwarded a non-whitelisted param: %q", got)
	}
}

func TestSourcesDropsInvalidPagination(t *testing.T) {
	var got string
	core := fakeSourcesCore(t, "secret", &got)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	// Non-numeric and non-positive pagination must not be forwarded verbatim to the upstream.
	r := httptest.NewRequest("GET", "/api/sources?page=abc&page_size=-5&kind=podcast", nil)
	s.handleSources(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	q, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("parse forwarded query %q: %v", got, err)
	}
	if q.Has("page") || q.Has("page_size") {
		t.Errorf("forwarded invalid pagination: %q", got)
	}
	if q.Get("kind") != "podcast" {
		t.Errorf("dropped a valid filter alongside bad pagination: %q", got)
	}
}

func TestSourceKindsProxiesRegistry(t *testing.T) {
	core := fakeSourcesCore(t, "secret", nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleSourceKinds(rec, httptest.NewRequest("GET", "/api/source-kinds", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !contains(body, `"kind":"podcast"`) {
		t.Errorf("response did not pass through the registry: %s", body)
	}
}

func TestSourcesBadGatewayOnCoreError(t *testing.T) {
	core := fakeSourcesCore(t, "right", nil)
	s := &server{coreURL: core.URL, token: "wrong", client: core.Client()} // token mismatch → 401 upstream

	rec := httptest.NewRecorder()
	s.handleSources(rec, httptest.NewRequest("GET", "/api/sources", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (upstream non-2xx must become a bad gateway)", rec.Code)
	}
}
