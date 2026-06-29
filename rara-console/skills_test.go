package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSkillsCore stands in for rara-core's /v1/skills surface: enforces the bearer and records
// the forwarded method+path+body so the BFF tests can prove path params and bodies pass through.
func fakeSkillsCore(t *testing.T, token string, method, path, body *string) *httptest.Server {
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
				t.Errorf("fakeSkillsCore: read request body: %v", err)
				http.Error(w, "read body", http.StatusInternalServerError)
				return
			}
			*body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(resp)); err != nil {
			t.Errorf("fakeSkillsCore: write response: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/skills", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `[{"id":1,"name":"docslide","trusted":false}]`)
	})
	mux.HandleFunc("PUT /v1/skills", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"id":1}`) })
	mux.HandleFunc("POST /v1/skills/import", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"id":2,"name":"linkedin-docslide","trusted":false}`)
	})
	mux.HandleFunc("DELETE /v1/skills/{id}", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"ok":true}`) })
	mux.HandleFunc("GET /v1/skills/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `[{"id":1,"path":"utils.py"}]`)
	})
	mux.HandleFunc("PUT /v1/skills/{id}/files", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"id":1}`) })
	mux.HandleFunc("DELETE /v1/skills/{id}/files", func(w http.ResponseWriter, r *http.Request) { record(w, r, `{"ok":true}`) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSkillsProxiesGET(t *testing.T) {
	var method, path string
	core := fakeSkillsCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleSkills(rec, httptest.NewRequest("GET", "/api/skills", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if method != "GET" || path != "/v1/skills" {
		t.Errorf("forwarded %s %s, want GET /v1/skills", method, path)
	}
}

func TestSkillsNeverLeaksToken(t *testing.T) {
	core := fakeSkillsCore(t, "supersecret", nil, nil, nil)
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}
	rec := httptest.NewRecorder()
	s.handleSkills(rec, httptest.NewRequest("GET", "/api/skills", nil))
	// Assert the happy path ran, else a handler that bailed before writing would pass vacuously.
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body)
	}
}

// The file endpoints must reject a non-numeric id before touching the core (path-traversal guard).
func TestSkillFileHandlersRejectBadID(t *testing.T) {
	var path string
	core := fakeSkillsCore(t, "secret", nil, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	cases := []struct {
		name    string
		method  string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"files", "GET", s.handleSkillFiles},
		{"upsert", "PUT", s.handleUpsertSkillFile},
		{"delete", "DELETE", s.handleDeleteSkillFile},
	}
	for _, c := range cases {
		path = ""
		rec := httptest.NewRecorder()
		req := newReqWithPathValue(c.method, "/api/skills/abc/files", `{"path":"x"}`, map[string]string{"id": "abc"})
		c.handler(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 for non-numeric id", c.name, rec.Code)
		}
		if path != "" {
			t.Errorf("%s: bad id reached upstream at %q", c.name, path)
		}
	}
}

func TestUpsertSkillProxiesPUT(t *testing.T) {
	var method, path, body string
	core := fakeSkillsCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"name":"docslide","content":"# hi","trusted":true}`
	rec := httptest.NewRecorder()
	s.handleUpsertSkill(rec, httptest.NewRequest("PUT", "/api/skills", strings.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if method != "PUT" || path != "/v1/skills" || body != reqBody {
		t.Errorf("forwarded %s %s body=%s", method, path, body)
	}
}

func TestImportSkillProxiesPOST(t *testing.T) {
	var method, path, body string
	core := fakeSkillsCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"url":"https://clawhub.example/x"}`
	rec := httptest.NewRecorder()
	s.handleImportSkill(rec, httptest.NewRequest("POST", "/api/skills/import", strings.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if method != "POST" || path != "/v1/skills/import" || body != reqBody {
		t.Errorf("forwarded %s %s body=%s, want POST /v1/skills/import with body verbatim", method, path, body)
	}
}

func TestDeleteSkillProxiesIDInPath(t *testing.T) {
	var method, path string
	core := fakeSkillsCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/skills/7", "", map[string]string{"id": "7"})
	s.handleDeleteSkill(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if method != "DELETE" || path != "/v1/skills/7" {
		t.Errorf("forwarded %s %s, want DELETE /v1/skills/7", method, path)
	}
}

func TestDeleteSkillRejectsBadID(t *testing.T) {
	var path string
	core := fakeSkillsCore(t, "secret", nil, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/skills/abc", "", map[string]string{"id": "abc"})
	s.handleDeleteSkill(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for non-numeric id", rec.Code)
	}
	if path != "" {
		t.Errorf("bad id reached upstream at %q", path)
	}
}

func TestSkillFilesProxyIDInPath(t *testing.T) {
	var method, path string
	core := fakeSkillsCore(t, "secret", &method, &path, nil)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("GET", "/api/skills/3/files", "", map[string]string{"id": "3"})
	s.handleSkillFiles(rec, req)
	if rec.Code != http.StatusOK || method != "GET" || path != "/v1/skills/3/files" {
		t.Errorf("forwarded %s %s (code %d), want GET /v1/skills/3/files", method, path, rec.Code)
	}
}

func TestUpsertSkillFileProxiesPUT(t *testing.T) {
	var method, path, body string
	core := fakeSkillsCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"path":"utils.py","content":"print(1)"}`
	rec := httptest.NewRecorder()
	req := newReqWithPathValue("PUT", "/api/skills/3/files", reqBody, map[string]string{"id": "3"})
	s.handleUpsertSkillFile(rec, req)
	if rec.Code != http.StatusOK || method != "PUT" || path != "/v1/skills/3/files" || body != reqBody {
		t.Errorf("forwarded %s %s body=%s (code %d)", method, path, body, rec.Code)
	}
}

func TestDeleteSkillFileProxiesDELETE(t *testing.T) {
	var method, path, body string
	core := fakeSkillsCore(t, "secret", &method, &path, &body)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	const reqBody = `{"path":"utils.py"}`
	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/skills/3/files", reqBody, map[string]string{"id": "3"})
	s.handleDeleteSkillFile(rec, req)
	if rec.Code != http.StatusOK || method != "DELETE" || path != "/v1/skills/3/files" {
		t.Errorf("forwarded %s %s (code %d), want DELETE /v1/skills/3/files", method, path, rec.Code)
	}
	// The path travels in the body — assert it's forwarded, else a regression that drops it is silent.
	if body != reqBody {
		t.Errorf("forwarded body=%s, want %s", body, reqBody)
	}
}
