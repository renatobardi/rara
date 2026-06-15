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
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
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

// An oversized core response must fail as a bad gateway, never pass through truncated (which would
// be invalid JSON served to the SPA as a 200).
func TestOverviewRejectsOversizedCoreResponse(t *testing.T) {
	core := fakeCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}
	old := maxCoreBytes
	maxCoreBytes = 8 // tiny: the fake flows body is larger than this
	defer func() { maxCoreBytes = old }()

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass as success)", rec.Code)
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

// fakePipelineCore serves the items endpoints the pipeline and item-steps handlers consume.
func fakePipelineCore(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		st := r.URL.Query().Get("status")
		switch st {
		case "discovered":
			_, _ = w.Write([]byte(`[{"id":1,"title":"Video A","status":"discovered"}]`))
		case "to_text":
			_, _ = w.Write([]byte(`[{"id":2,"title":"Video B","status":"to_text"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})
	mux.HandleFunc("GET /v1/items/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"capability":"destilar","provider":"distill-local","status":"done","attempts":1}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPipelineAggregatesAllStatuses(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Counts map[string]int              `json:"counts"`
		Items  map[string][]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.Counts["discovered"] != 1 {
		t.Errorf("counts[discovered] = %d, want 1", got.Counts["discovered"])
	}
	if got.Counts["to_text"] != 1 {
		t.Errorf("counts[to_text] = %d, want 1", got.Counts["to_text"])
	}
	if got.Counts["done"] != 0 {
		t.Errorf("counts[done] = %d, want 0", got.Counts["done"])
	}
	if len(got.Items["discovered"]) != 1 {
		t.Errorf("items[discovered] len = %d, want 1", len(got.Items["discovered"]))
	}
}

func TestPipelineNeverLeaksToken(t *testing.T) {
	core := fakePipelineCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestPipelineRejectsOversizedCoreResponse(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}
	old := maxCoreBytes
	maxCoreBytes = 1 // even "[]" (2 bytes) exceeds this
	defer func() { maxCoreBytes = old }()

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass as success)", rec.Code)
	}
}

func TestPipelineReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestItemStepsProxiesCorrectly(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0]["capability"] != "destilar" {
		t.Errorf("steps = %+v, want one destilar step", got)
	}
}

func TestItemStepsNeverLeaksToken(t *testing.T) {
	core := fakePipelineCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestItemStepsRejectsOversizedCoreResponse(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}
	old := maxCoreBytes
	maxCoreBytes = 1
	defer func() { maxCoreBytes = old }()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized response must not pass as success)", rec.Code)
	}
}

func TestItemStepsReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestItemStepsRejectsBadID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "", "12x", "../etc", "9999999999999999999999"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/items/"+bad+"/steps", nil)
		req.SetPathValue("id", bad)
		s.handleItemSteps(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q: status = %d, want 400", bad, rec.Code)
		}
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
