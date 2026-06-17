package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSourcesFlowsCore stands in for the rara-core surface, serving the podcast-sources and flows
// endpoints the Fontes & Flows BFF proxies. Every route demands the bearer, so a 2xx proves the
// console injected the token server-side.
func fakeSourcesFlowsCore(t *testing.T, token string) *httptest.Server {
	t.Helper()
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+token }
	guard := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authed(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(body))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sources/podcast", guard(`[{"id":1,"feed_url":"https://a.example/rss","title":"A","active":true}]`))
	mux.HandleFunc("POST /v1/sources/podcast", guard(`{"id":2}`))
	mux.HandleFunc("PUT /v1/sources/podcast", guard(`{"ok":true}`))
	mux.HandleFunc("GET /v1/flows", guard(`[{"id":1,"name":"podcast","source_type":"podcast","enabled":true,"version":1}]`))
	mux.HandleFunc("GET /v1/flows/{id}/steps", guard(`[{"flow_id":1,"seq":1,"capability":"transcrever","enabled":true}]`))
	mux.HandleFunc("PUT /v1/flows", guard(`{"id":1}`))
	mux.HandleFunc("PUT /v1/flow-steps", guard(`{"ok":true}`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// --- Fontes (podcast feeds) ---

func TestPodcastSourcesProxiesWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePodcastSources(rec, httptest.NewRequest("GET", "/api/sources/podcast", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a.example") {
		t.Errorf("body missing the feed: %s", rec.Body.String())
	}
}

func TestPodcastSourcesNeverLeaksToken(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePodcastSources(rec, httptest.NewRequest("GET", "/api/sources/podcast", nil))

	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
}

func TestPodcastSourcesReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handlePodcastSources(rec, httptest.NewRequest("GET", "/api/sources/podcast", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestAddPodcastSourceProxiesPostWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sources/podcast", strings.NewReader(`{"feed_url":"https://b.example/rss"}`))
	s.handleAddPodcastSource(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":2`) {
		t.Errorf("body = %s, want the new feed id", rec.Body.String())
	}
}

// A core 4xx (e.g. empty feed_url) must propagate to the SPA, not be masked as a 502.
func TestAddPodcastSourcePropagatesCoreError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sources/podcast", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"feed_url cannot be empty"}`, http.StatusBadRequest)
	})
	core := httptest.NewServer(mux)
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sources/podcast", strings.NewReader(`{"feed_url":""}`))
	s.handleAddPodcastSource(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core error must propagate, not become 502)", rec.Code)
	}
}

func TestAddPodcastSourceRejectsOversizedBody(t *testing.T) {
	shrinkMaxBytes(t, 4)
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sources/podcast", strings.NewReader(`{"feed_url":"https://way-too-long.example/rss"}`))
	s.handleAddPodcastSource(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized body must not reach core)", rec.Code)
	}
}

func TestAddPodcastSourceNeverLeaksToken(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sources/podcast", strings.NewReader(`{"feed_url":"https://b.example/rss"}`))
	s.handleAddPodcastSource(rec, req)

	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("write response leaked the surface token: %s", rec.Body.String())
	}
}

func TestTogglePodcastSourceNeverLeaksToken(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/sources/podcast", strings.NewReader(`{"id":1,"active":false}`))
	s.handleTogglePodcastSource(rec, req)

	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("write response leaked the surface token: %s", rec.Body.String())
	}
}

func TestTogglePodcastSourceProxiesPutWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/sources/podcast", strings.NewReader(`{"id":1,"active":false}`))
	s.handleTogglePodcastSource(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// --- Flows ---

func TestFlowsProxiesWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleFlows(rec, httptest.NewRequest("GET", "/api/flows", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "podcast") {
		t.Errorf("body missing the flow: %s", rec.Body.String())
	}
}

func TestFlowsNeverLeaksToken(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleFlows(rec, httptest.NewRequest("GET", "/api/flows", nil))

	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
}

func TestFlowStepsRejectsNonNumericID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/flows/abc/steps", nil)
	req.SetPathValue("id", "abc")
	s.handleFlowSteps(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (non-numeric id must not reach core)", rec.Code)
	}
}

func TestFlowStepsProxiesWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/flows/1/steps", nil)
	req.SetPathValue("id", "1")
	s.handleFlowSteps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "transcrever") {
		t.Errorf("body missing the step: %s", rec.Body.String())
	}
}

func TestUpsertFlowProxiesPutWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flows", strings.NewReader(`{"name":"podcast","source_type":"podcast","enabled":false,"version":1}`))
	s.handleUpsertFlow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpsertFlowNeverLeaksToken(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flows", strings.NewReader(`{"name":"podcast","source_type":"podcast","enabled":false,"version":1}`))
	s.handleUpsertFlow(rec, req)

	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("write response leaked the surface token: %s", rec.Body.String())
	}
}

func TestUpsertFlowStepProxiesPutWithBearer(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flow-steps", strings.NewReader(`{"flow_id":1,"seq":1,"capability":"transcrever","enabled":false}`))
	s.handleUpsertFlowStep(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpsertFlowStepNeverLeaksToken(t *testing.T) {
	core := fakeSourcesFlowsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flow-steps", strings.NewReader(`{"flow_id":1,"seq":1,"capability":"transcrever","enabled":false}`))
	s.handleUpsertFlowStep(rec, req)

	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("write response leaked the surface token: %s", rec.Body.String())
	}
}
