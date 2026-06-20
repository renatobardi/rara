package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWorkersCore serves /v1/workers/metrics and /v1/route/preview. It records the raw query
// string of each request in `captured` so tests can assert what params the BFF forwarded.
func fakeWorkersCore(t *testing.T, token string) (*httptest.Server, *string) {
	t.Helper()
	captured := new(string)
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+token }
	handler := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authed(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			*captured = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workers/metrics", handler(`{"workers":[]}`))
	mux.HandleFunc("GET /v1/route/preview", handler(`{"winner":"distill-mac","candidates":[]}`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, captured
}

// fakeWorkersCoreErr serves both endpoints with a fixed error status.
func fakeWorkersCoreErr(t *testing.T, token string, status int) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"core error"}`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workers/metrics", h)
	mux.HandleFunc("GET /v1/route/preview", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doRoutePreview(s *server, rawQuery string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	target := "/api/route/preview"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest("GET", target, nil)
	s.handleRoutePreview(rec, req)
	return rec
}

func doWorkerMetrics(s *server, rawQuery string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	target := "/api/workers/metrics"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest("GET", target, nil)
	s.handleWorkerMetrics(rec, req)
	return rec
}

// --- route/preview ---

func TestRoutePreviewRejectsWhitespaceCapability(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}

	rec := doRoutePreview(s, "capability=+++") // "+++" decodes as "   " (spaces)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for whitespace-only capability; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capability is required") {
		t.Errorf("body missing error message: %s", rec.Body.String())
	}
}

func TestRoutePreviewRejectsEmptyCapability(t *testing.T) {
	// Dead URL confirms core is never called when capability is missing.
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}

	rec := doRoutePreview(s, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capability is required") {
		t.Errorf("body missing error message: %s", rec.Body.String())
	}
}

func TestRoutePreviewProxiesCapability(t *testing.T) {
	core, _ := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doRoutePreview(s, "capability=destilar")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "winner") {
		t.Errorf("body missing winner field: %s", rec.Body.String())
	}
}

func TestRoutePreviewForwardsExcludeMultiValue(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doRoutePreview(s, "capability=distill&exclude=worker-a&exclude=worker-b")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "exclude=worker-a") || !strings.Contains(*captured, "exclude=worker-b") {
		t.Errorf("exclude values not forwarded to core; core received query: %s", *captured)
	}
}

func TestRoutePreviewReturns502OnCoreError(t *testing.T) {
	core := fakeWorkersCoreErr(t, "tok", http.StatusInternalServerError)
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doRoutePreview(s, "capability=distill")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// --- workers/metrics ---

func TestWorkerMetricsNoQueryForwarded(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doWorkerMetrics(s, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if *captured != "" {
		t.Errorf("expected no query forwarded to core, got: %q", *captured)
	}
}

func TestWorkerMetricsForwardsDays(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doWorkerMetrics(s, "days=7")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "days=7") {
		t.Errorf("days=7 not forwarded; core received: %q", *captured)
	}
}

func TestWorkerMetricsForwardsMinBoundaryDays(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doWorkerMetrics(s, "days=1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "days=1") {
		t.Errorf("days=1 not forwarded; core received: %q", *captured)
	}
}

func TestWorkerMetricsForwardsMaxBoundaryDays(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doWorkerMetrics(s, "days=365")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "days=365") {
		t.Errorf("days=365 not forwarded; core received: %q", *captured)
	}
}

func TestWorkerMetricsRejectsNonNumericDays(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}

	rec := doWorkerMetrics(s, "days=abc")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerMetricsRejectsZeroDays(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}

	rec := doWorkerMetrics(s, "days=0")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerMetricsRejectsNegativeDays(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}

	rec := doWorkerMetrics(s, "days=-1")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRoutePreviewForwardsLane(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doRoutePreview(s, "capability=distill&lane=fast")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "lane=fast") {
		t.Errorf("lane param not forwarded; core received: %q", *captured)
	}
}

func TestRoutePreviewForwardsSensitivity(t *testing.T) {
	core, captured := fakeWorkersCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doRoutePreview(s, "capability=distill&sensitivity=high")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "sensitivity=high") {
		t.Errorf("sensitivity param not forwarded; core received: %q", *captured)
	}
}

func TestWorkerMetricsRejectsAboveMaxDays(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}

	rec := doWorkerMetrics(s, "days=400")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerMetricsReturns502OnCoreError(t *testing.T) {
	core := fakeWorkersCoreErr(t, "tok", http.StatusInternalServerError)
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doWorkerMetrics(s, "")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
