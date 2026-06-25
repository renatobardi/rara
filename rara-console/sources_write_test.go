package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSourcesWriteCore stands in for the rara-core surface's source-write endpoints (fatia #2/#2b).
// It demands the bearer token and records the method+path it received so a test can assert the BFF
// forwarded to the right upstream route with the token injected.
func fakeSourcesWriteCore(t *testing.T, token string, gotMethod, gotPath *string) *httptest.Server {
	t.Helper()
	record := func(w http.ResponseWriter, r *http.Request, reply string) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if gotMethod != nil {
			*gotMethod = r.Method
		}
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		if _, err := w.Write([]byte(reply)); err != nil {
			t.Errorf("fake core write: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sources/{kind}", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"id":7}`)
	})
	mux.HandleFunc("PATCH /v1/sources/{source_id}", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"ok":true}`)
	})
	mux.HandleFunc("DELETE /v1/sources/{source_id}", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"ok":true}`)
	})
	mux.HandleFunc("POST /v1/sources/{source_id}/pause", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"ok":true}`)
	})
	mux.HandleFunc("POST /v1/sources/{source_id}/resume", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newReqWithPathValue builds a request and sets the Go-1.22 path value(s) by hand, since calling a
// handler directly (not via the mux) doesn't populate r.PathValue.
func newReqWithPathValue(method, target, body string, pv map[string]string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestAddSourceProxiesPostWithKindInPath(t *testing.T) {
	var method, path string
	core := fakeSourcesWriteCore(t, "secret", &method, &path)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("POST", "/api/sources/youtube_channel",
		`{"channel_id":"@lex","display_name":"Lex"}`, map[string]string{"kind": "youtube_channel"})
	s.handleAddSource(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if method != "POST" || path != "/v1/sources/youtube_channel" {
		t.Errorf("forwarded %s %s, want POST /v1/sources/youtube_channel", method, path)
	}
	if !strings.Contains(rec.Body.String(), `"id":7`) {
		t.Errorf("body = %s, want the new source id", rec.Body.String())
	}
}

func TestAddSourceRejectsUnsafeKind(t *testing.T) {
	var path string
	core := fakeSourcesWriteCore(t, "secret", nil, &path)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	// A traversal payload must be rejected at the BFF, never reaching the upstream.
	req := newReqWithPathValue("POST", "/api/sources/x", `{}`, map[string]string{"kind": "../admin"})
	s.handleAddSource(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unsafe kind", rec.Code)
	}
	if path != "" {
		t.Errorf("unsafe kind reached the upstream at %q", path)
	}
}

func TestAddSourcePropagatesCoreError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sources/{kind}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"channel_id cannot be empty"}`, http.StatusBadRequest)
	})
	core := httptest.NewServer(mux)
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("POST", "/api/sources/rss", `{"name":""}`, map[string]string{"kind": "rss"})
	s.handleAddSource(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core validation must propagate, not become 502)", rec.Code)
	}
}

func TestPatchSourceProxiesWithIDInPath(t *testing.T) {
	var method, path string
	core := fakeSourcesWriteCore(t, "secret", &method, &path)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("PATCH", "/api/sources/youtube_channel:42",
		`{"display_name":"New","tags":["ml"]}`, map[string]string{"source_id": "youtube_channel:42"})
	s.handlePatchSource(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if method != "PATCH" || path != "/v1/sources/youtube_channel:42" {
		t.Errorf("forwarded %s %s, want PATCH /v1/sources/youtube_channel:42", method, path)
	}
}

func TestPatchSourceRejectsMalformedID(t *testing.T) {
	var path string
	core := fakeSourcesWriteCore(t, "secret", nil, &path)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("PATCH", "/api/sources/x", `{}`, map[string]string{"source_id": "../../v1/flows"})
	s.handlePatchSource(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed source id", rec.Code)
	}
	if path != "" {
		t.Errorf("malformed id reached the upstream at %q", path)
	}
}

func TestDeleteAndToggleRejectMalformedID(t *testing.T) {
	var path string
	core := fakeSourcesWriteCore(t, "secret", nil, &path)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"delete", s.handleDeleteSource},
		{"pause", s.handlePauseSource},
		{"resume", s.handleResumeSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path = ""
			rec := httptest.NewRecorder()
			req := newReqWithPathValue("POST", "/api/sources/x", "", map[string]string{"source_id": "../../v1/flows"})
			tc.handler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for malformed source id", rec.Code)
			}
			if path != "" {
				t.Errorf("malformed id reached the upstream at %q", path)
			}
		})
	}
}

func TestDeleteSourceProxiesWithIDInPath(t *testing.T) {
	var method, path string
	core := fakeSourcesWriteCore(t, "secret", &method, &path)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("DELETE", "/api/sources/podcast:1", "", map[string]string{"source_id": "podcast:1"})
	s.handleDeleteSource(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if method != "DELETE" || path != "/v1/sources/podcast:1" {
		t.Errorf("forwarded %s %s, want DELETE /v1/sources/podcast:1", method, path)
	}
}

func TestPauseResumeSourceProxiesToSubpath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(*server) http.HandlerFunc
		want    string
	}{
		{"pause", func(s *server) http.HandlerFunc { return s.handlePauseSource }, "/v1/sources/rss:3/pause"},
		{"resume", func(s *server) http.HandlerFunc { return s.handleResumeSource }, "/v1/sources/rss:3/resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var method, path string
			core := fakeSourcesWriteCore(t, "secret", &method, &path)
			s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

			rec := httptest.NewRecorder()
			req := newReqWithPathValue("POST", "/api/sources/rss:3/"+tc.name, "", map[string]string{"source_id": "rss:3"})
			tc.handler(s)(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if method != "POST" || path != tc.want {
				t.Errorf("forwarded %s %s, want POST %s", method, path, tc.want)
			}
		})
	}
}

func TestSourceWritesNeverLeakToken(t *testing.T) {
	core := fakeSourcesWriteCore(t, "supersecret", nil, nil)
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := newReqWithPathValue("PATCH", "/api/sources/podcast:1",
		`{"tags":["x"]}`, map[string]string{"source_id": "podcast:1"})
	s.handlePatchSource(rec, req)

	// Assert the happy path ran (else a handler that bailed before writing would pass vacuously).
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), "supersecret") {
		t.Errorf("write response leaked the surface token: %s", rec.Body.String())
	}
}
