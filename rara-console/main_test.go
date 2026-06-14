package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeCore stands in for the rara-core surface: it demands the bearer token and serves
// the two read endpoints the overview aggregates. A request without the token gets 401,
// so a successful aggregate proves the console injected the token server-side.
func fakeCore(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/flows", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"youtube","source_type":"youtube","enabled":true,"version":1}]`))
	})
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"distill-local","capability":"destilar","runtime":"vpc","activation":"resident","enabled":true}]`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOverviewAggregatesFlowsAndProviders(t *testing.T) {
	core := fakeCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Flows     []map[string]any `json:"flows"`
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Flows) != 1 || got.Flows[0]["name"] != "youtube" {
		t.Errorf("flows = %+v, want one youtube flow", got.Flows)
	}
	if len(got.Providers) != 1 || got.Providers[0]["name"] != "distill-local" {
		t.Errorf("providers = %+v, want one distill-local provider", got.Providers)
	}
}

// The bearer token is a server-side secret; it must never appear in a response served to the SPA.
func TestOverviewNeverLeaksToken(t *testing.T) {
	core := fakeCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestOverviewReturns502WhenCoreUnreachable(t *testing.T) {
	// A closed server URL: the dial fails, so the aggregate is a bad gateway, not a 200.
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHealthzReportsCoreReachability(t *testing.T) {
	core := fakeCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest("GET", "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["core"] != true {
		t.Errorf("core reachability = %v, want true", got["core"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
