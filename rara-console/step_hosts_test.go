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
	hostsBody := `{"providers":["asr-youtube"],"available":[{"name":"asr-youtube","capability":"transcrever","runtime":"local","activation":"resident","enabled":true}]}`
	mux.HandleFunc("GET /v1/flows/{flow_id}/steps/{seq}/hosts", guard(hostsBody))
	mux.HandleFunc("PUT /v1/flows/{flow_id}/steps/{seq}/hosts", guard(`{"ok":true}`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestStepHostsGETProxiesWithBearer(t *testing.T) {
	core := fakeStepHostsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/flows/1/steps/3/hosts", nil)
	req.SetPathValue("flow_id", "1")
	req.SetPathValue("seq", "3")
	s.handleStepHosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "asr-youtube") {
		t.Errorf("body missing asr-youtube: %s", rec.Body.String())
	}
}

func TestStepHostsGETNeverLeaksToken(t *testing.T) {
	core := fakeStepHostsCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/flows/1/steps/3/hosts", nil)
	req.SetPathValue("flow_id", "1")
	req.SetPathValue("seq", "3")
	s.handleStepHosts(rec, req)

	if strings.Contains(rec.Body.String(), "supersecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
}

func TestStepHostsGETRejects502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/flows/1/steps/3/hosts", nil)
	req.SetPathValue("flow_id", "1")
	req.SetPathValue("seq", "3")
	s.handleStepHosts(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestStepHostsGETRejectsBadFlowID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "-1", "", "12x"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/flows/"+bad+"/steps/3/hosts", nil)
		req.SetPathValue("flow_id", bad)
		req.SetPathValue("seq", "3")
		s.handleStepHosts(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("flow_id=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestStepHostsGETRejectsBadSeq(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "-1", "", "12x"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/flows/1/steps/"+bad+"/hosts", nil)
		req.SetPathValue("flow_id", "1")
		req.SetPathValue("seq", bad)
		s.handleStepHosts(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("seq=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestStepHostsPUTProxiesWithBearer(t *testing.T) {
	core := fakeStepHostsCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flows/1/steps/3/hosts",
		strings.NewReader(`{"providers":["asr-youtube"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("flow_id", "1")
	req.SetPathValue("seq", "3")
	s.handleSetStepHosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestStepHostsPUTReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flows/1/steps/3/hosts",
		strings.NewReader(`{"providers":["asr-youtube"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("flow_id", "1")
	req.SetPathValue("seq", "3")
	s.handleSetStepHosts(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestStepHostsPUTNeverLeaksToken(t *testing.T) {
	core := fakeStepHostsCore(t, "topsecret")
	s := &server{coreURL: core.URL, token: "topsecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/flows/1/steps/3/hosts",
		strings.NewReader(`{"providers":["asr-youtube"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("flow_id", "1")
	req.SetPathValue("seq", "3")
	s.handleSetStepHosts(rec, req)

	if strings.Contains(rec.Body.String(), "topsecret") {
		t.Errorf("response leaked the surface token: %s", rec.Body.String())
	}
}

func TestStepHostsPUTRejectsBadFlowID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "-1", "", "12x"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/api/flows/"+bad+"/steps/3/hosts",
			strings.NewReader(`{"providers":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("flow_id", bad)
		req.SetPathValue("seq", "3")
		s.handleSetStepHosts(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("flow_id=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestStepHostsPUTRejectsBadSeq(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "-1", "", "12x"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/api/flows/1/steps/"+bad+"/hosts",
			strings.NewReader(`{"providers":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("flow_id", "1")
		req.SetPathValue("seq", bad)
		s.handleSetStepHosts(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("seq=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}
