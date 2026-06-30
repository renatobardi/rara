package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeAgentTasksCore stands in for rara-core's task surface, recording the forwarded
// method+path+rawquery+body so the BFF tests can prove path/query/body pass through.
func fakeAgentTasksCore(t *testing.T, token string, method, path, rawQuery, body *string) *httptest.Server {
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
		if rawQuery != nil {
			*rawQuery = r.URL.RawQuery
		}
		if body != nil {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("fakeAgentTasksCore: read request body: %v", err)
				http.Error(w, "read body", http.StatusInternalServerError)
				return
			}
			*body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(resp)); err != nil {
			t.Errorf("fakeAgentTasksCore: write response: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents/{id}/tasks", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"id":1}`) })
	mux.HandleFunc("GET /v1/agents/{id}/tasks", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `[{"id":1,"instruction":"summarize"}]`)
	})
	mux.HandleFunc("GET /v1/agent-tasks", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `[{"id":1,"status":"queued"}]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnqueueAgentTaskProxiesPOST(t *testing.T) {
	var method, path, body string
	core := fakeAgentTasksCore(t, "secret", &method, &path, nil, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"instruction":"summarize","context_refs":[1,2]}`
	rec := httptest.NewRecorder()
	req := newReqWithPathValue("POST", "/api/agents/3/tasks", reqBody, map[string]string{"id": "3"})
	s.handleEnqueueAgentTask(rec, req)
	if rec.Code != http.StatusOK || method != "POST" || path != "/v1/agents/3/tasks" || body != reqBody {
		t.Errorf("forwarded %s %s body=%s (code %d)", method, path, body, rec.Code)
	}
}

func TestAgentTasksHistoryProxiesGET(t *testing.T) {
	var method, path string
	core := fakeAgentTasksCore(t, "secret", &method, &path, nil, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("GET", "/api/agents/3/tasks", "", map[string]string{"id": "3"})
	s.handleAgentTasks(rec, req)
	if rec.Code != http.StatusOK || method != "GET" || path != "/v1/agents/3/tasks" {
		t.Errorf("forwarded %s %s (code %d), want GET /v1/agents/3/tasks", method, path, rec.Code)
	}
}

func TestAgentTaskFeedForwardsStatus(t *testing.T) {
	var path, rawQuery string
	core := fakeAgentTasksCore(t, "secret", nil, &path, &rawQuery, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleAgentTaskFeed(rec, httptest.NewRequest("GET", "/api/agent-tasks?status=queued", nil))
	if rec.Code != http.StatusOK || path != "/v1/agent-tasks" || rawQuery != "status=queued" {
		t.Errorf("forwarded path=%s query=%s (code %d), want /v1/agent-tasks?status=queued", path, rawQuery, rec.Code)
	}
}

// An unknown status (or an injection attempt like "queued&limit=1") is rejected at the BFF with a
// 400 and never reaches the core — the allowlist closes the query-injection vector by construction.
func TestAgentTaskFeedRejectsBadStatus(t *testing.T) {
	var path string
	core := fakeAgentTasksCore(t, "secret", nil, &path, nil, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	for _, bad := range []string{"bogus", "queued&limit=1"} {
		path = ""
		rec := httptest.NewRecorder()
		s.handleAgentTaskFeed(rec, httptest.NewRequest("GET", "/api/agent-tasks?status="+url.QueryEscape(bad), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%q: code=%d, want 400", bad, rec.Code)
		}
		if path != "" {
			t.Errorf("status=%q reached upstream at %q", bad, path)
		}
	}
}

func TestAgentTaskFeedNoStatusSendsNoQuery(t *testing.T) {
	var rawQuery string
	core := fakeAgentTasksCore(t, "secret", nil, nil, &rawQuery, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleAgentTaskFeed(rec, httptest.NewRequest("GET", "/api/agent-tasks", nil))
	if rec.Code != http.StatusOK || rawQuery != "" {
		t.Errorf("unfiltered feed forwarded query=%q (code %d), want empty", rawQuery, rec.Code)
	}
}

// The {id} handlers must reject a non-numeric id before touching the core.
func TestAgentTaskHandlersRejectBadID(t *testing.T) {
	var path string
	core := fakeAgentTasksCore(t, "secret", nil, &path, nil, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	cases := []struct {
		name    string
		method  string
		reqPath string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"enqueue", "POST", "/api/agents/abc/tasks", s.handleEnqueueAgentTask},
		{"history", "GET", "/api/agents/abc/tasks", s.handleAgentTasks},
	}
	for _, c := range cases {
		path = ""
		rec := httptest.NewRecorder()
		req := newReqWithPathValue(c.method, c.reqPath, `{"instruction":"x"}`, map[string]string{"id": "abc"})
		c.handler(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 for non-numeric id", c.name, rec.Code)
		}
		if path != "" {
			t.Errorf("%s: bad id reached upstream at %q", c.name, path)
		}
	}
}
