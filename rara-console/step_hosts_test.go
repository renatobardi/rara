package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStepHostsCore serves the step-hosts endpoints the console BFF proxies.
func fakeStepHostsCore(t *testing.T, token string) *httptest.Server {
	t.Helper()
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+token }
	guard := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authed(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	mux := http.NewServeMux()
	hostsBody := `{"providers":["caption-mac"],"available":[{"name":"caption-mac","capability":"transcrever","runtime":"local","activation":"resident","enabled":true}]}`
	mux.HandleFunc("GET /v1/flows/{flow_id}/steps/{seq}/hosts", guard(hostsBody))
	mux.HandleFunc("PUT /v1/flows/{flow_id}/steps/{seq}/hosts", guard(`{"ok":true}`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeStepHostsCoreError serves the step-hosts endpoints returning the given status code.
func fakeStepHostsCoreError(t *testing.T, token string, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"core error"}`))
	}
	mux.HandleFunc("GET /v1/flows/{flow_id}/steps/{seq}/hosts", handler)
	mux.HandleFunc("PUT /v1/flows/{flow_id}/steps/{seq}/hosts", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// doGETHosts fires GET /api/flows/{flowID}/steps/{seq}/hosts directly at the handler.
func doGETHosts(s *server, flowID, seq string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/flows/"+flowID+"/steps/"+seq+"/hosts", nil)
	req.SetPathValue("flow_id", flowID)
	req.SetPathValue("seq", seq)
	s.handleStepHosts(rec, req)
	return rec
}

// doPUTHosts fires PUT /api/flows/{flowID}/steps/{seq}/hosts directly at the handler.
func doPUTHosts(s *server, flowID, seq, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flows/"+flowID+"/steps/"+seq+"/hosts",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("flow_id", flowID)
	req.SetPathValue("seq", seq)
	s.handleSetStepHosts(rec, req)
	return rec
}

func TestStepHostsGETProxiesWithBearer(t *testing.T) {
	core := fakeStepHostsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := doGETHosts(s, "1", "3")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "caption-mac") {
		t.Errorf("body missing caption-mac: %s", rec.Body.String())
	}
}

func TestStepHostsGETNeverLeaksToken(t *testing.T) {
	core := fakeStepHostsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := doGETHosts(s, "1", "3")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
}

func TestStepHostsGETRejects502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := doGETHosts(s, "1", "3")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestStepHostsGETPropagatesCoreError(t *testing.T) {
	core := fakeStepHostsCoreError(t, "secret", http.StatusNotFound)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := doGETHosts(s, "1", "99")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 propagated from core", rec.Code)
	}
}

func TestStepHostsPUTProxiesWithBearer(t *testing.T) {
	core := fakeStepHostsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := doPUTHosts(s, "1", "3", `{"providers":["caption-mac"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestStepHostsPUTReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := doPUTHosts(s, "1", "3", `{"providers":["caption-mac"]}`)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestStepHostsPUTNeverLeaksToken(t *testing.T) {
	core := fakeStepHostsCore(t, "topsecret")
	s := &server{coreURL: core.URL, token: "topsecret", client: core.Client()}

	rec := doPUTHosts(s, "1", "3", `{"providers":["caption-mac"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "topsecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
}

func TestStepHostsPUTPropagatesCoreError(t *testing.T) {
	core := fakeStepHostsCoreError(t, "secret", http.StatusBadRequest)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := doPUTHosts(s, "1", "3", `{"providers":["no-such"]}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 propagated from core", rec.Code)
	}
}

// TestStepHostsRejectsBadParams covers all bad flow_id / seq inputs for both GET and PUT.
// Since parseFlowStepIDs is shared by both handlers, testing via both here confirms
// the validation wires through end-to-end for each route.
func TestStepHostsRejectsBadParams(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}
	bads := []string{"abc", "-1", "", "12x"}

	for _, bad := range bads {
		if rec := doGETHosts(s, bad, "3"); rec.Code != http.StatusBadRequest {
			t.Errorf("GET flow_id=%q: status = %d, want 400", bad, rec.Code)
		}
		if rec := doGETHosts(s, "1", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("GET seq=%q: status = %d, want 400", bad, rec.Code)
		}
		if rec := doPUTHosts(s, bad, "3", `{"providers":[]}`); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT flow_id=%q: status = %d, want 400", bad, rec.Code)
		}
		if rec := doPUTHosts(s, "1", bad, `{"providers":[]}`); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT seq=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}
