package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAgentsCore stands in for rara-core's /v1/agents surface: enforces the bearer and records the
// forwarded method+path+body so the BFF tests can prove path params and bodies pass through.
func fakeAgentsCore(t *testing.T, token string, method, path, body *string) *httptest.Server {
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
				t.Errorf("fakeAgentsCore: read request body: %v", err)
				http.Error(w, "read body", http.StatusInternalServerError)
				return
			}
			*body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(resp)); err != nil {
			t.Errorf("fakeAgentsCore: write response: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `[{"id":1,"name":"Docslide Writer"}]`)
	})
	mux.HandleFunc("PUT /v1/agents", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"id":1}`) })
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"id":1,"name":"Docslide Writer","skill_ids":[7]}`)
	})
	mux.HandleFunc("DELETE /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"ok":true}`) })
	mux.HandleFunc("PUT /v1/agents/{id}/skills", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"ok":true}`) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentsProxiesGET(t *testing.T) {
	var method, path string
	core := fakeAgentsCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleAgents(rec, httptest.NewRequest("GET", "/api/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if method != "GET" || path != "/v1/agents" {
		t.Errorf("forwarded %s %s, want GET /v1/agents", method, path)
	}
}

func TestAgentsNeverLeaksToken(t *testing.T) {
	core := fakeAgentsCore(t, "supersecret", nil, nil, nil)
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}
	rec := httptest.NewRecorder()
	s.handleAgents(rec, httptest.NewRequest("GET", "/api/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body)
	}
}

func TestUpsertAgentProxiesPUT(t *testing.T) {
	var method, path, body string
	core := fakeAgentsCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"name":"Docslide Writer","instructions":"slides","model":"groq/llama"}`
	rec := httptest.NewRecorder()
	s.handleUpsertAgent(rec, httptest.NewRequest("PUT", "/api/agents", strings.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if method != "PUT" || path != "/v1/agents" || body != reqBody {
		t.Errorf("forwarded %s %s body=%s", method, path, body)
	}
}

func TestGetAgentProxiesIDInPath(t *testing.T) {
	var method, path string
	core := fakeAgentsCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("GET", "/api/agents/3", "", map[string]string{"id": "3"})
	s.handleGetAgent(rec, req)
	if rec.Code != http.StatusOK || method != "GET" || path != "/v1/agents/3" {
		t.Errorf("forwarded %s %s (code %d), want GET /v1/agents/3", method, path, rec.Code)
	}
}

func TestDeleteAgentProxiesIDInPath(t *testing.T) {
	var method, path string
	core := fakeAgentsCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/agents/7", "", map[string]string{"id": "7"})
	s.handleDeleteAgent(rec, req)
	if rec.Code != http.StatusOK || method != "DELETE" || path != "/v1/agents/7" {
		t.Errorf("forwarded %s %s, want DELETE /v1/agents/7", method, path)
	}
}

func TestSetAgentSkillsProxiesPUT(t *testing.T) {
	var method, path, body string
	core := fakeAgentsCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"skill_ids":[7,9]}`
	rec := httptest.NewRecorder()
	req := newReqWithPathValue("PUT", "/api/agents/3/skills", reqBody, map[string]string{"id": "3"})
	s.handleSetAgentSkills(rec, req)
	if rec.Code != http.StatusOK || method != "PUT" || path != "/v1/agents/3/skills" || body != reqBody {
		t.Errorf("forwarded %s %s body=%s (code %d)", method, path, body, rec.Code)
	}
}

// The {id} handlers must reject a non-numeric id before touching the core (path-traversal guard).
func TestAgentHandlersRejectBadID(t *testing.T) {
	var path string
	core := fakeAgentsCore(t, "secret", nil, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	cases := []struct {
		name    string
		method  string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"get", "GET", s.handleGetAgent},
		{"delete", "DELETE", s.handleDeleteAgent},
		{"skills", "PUT", s.handleSetAgentSkills},
	}
	for _, c := range cases {
		path = ""
		reqPath := "/api/agents/abc"
		if c.name == "skills" {
			reqPath = "/api/agents/abc/skills" // exercise the real skills route, not the generic one
		}
		rec := httptest.NewRecorder()
		req := newReqWithPathValue(c.method, reqPath, `{"skill_ids":[]}`, map[string]string{"id": "abc"})
		c.handler(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 for non-numeric id", c.name, rec.Code)
		}
		if path != "" {
			t.Errorf("%s: bad id reached upstream at %q", c.name, path)
		}
	}
}
