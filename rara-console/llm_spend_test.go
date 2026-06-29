package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

// assertForwarded parses the query string core received and asserts it matches
// want exactly — catching extra, duplicated, or double-encoded params that a
// substring check would miss.
func assertForwarded(t *testing.T, raw string, want url.Values) {
	t.Helper()
	got, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse forwarded query %q: %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded query = %v, want %v (raw=%q)", got, want, raw)
	}
}

// fakeSpendCore serves /v1/llm-spend and records the forwarded query string.
func fakeSpendCore(t *testing.T, token string) (*httptest.Server, *string) {
	t.Helper()
	captured := new(string)
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		*captured = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/llm-spend", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, captured
}

func doLLMSpend(s *server, rawQuery string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	target := "/api/llm-spend"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest("GET", target, nil)
	s.handleLLMSpend(rec, req)
	return rec
}

func TestLLMSpendNoQueryForwarded(t *testing.T) {
	core, captured := fakeSpendCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doLLMSpend(s, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if *captured != "" {
		t.Errorf("expected no query forwarded, got %q", *captured)
	}
}

func TestLLMSpendForwardsDays(t *testing.T) {
	core, captured := fakeSpendCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doLLMSpend(s, "days=30")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertForwarded(t, *captured, url.Values{"days": {"30"}})
}

func TestLLMSpendForwardsModel(t *testing.T) {
	core, captured := fakeSpendCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doLLMSpend(s, "model=groq-llama")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertForwarded(t, *captured, url.Values{"model": {"groq-llama"}})
}

// TestLLMSpendForwardsModelPercentEncoded: a model alias with characters that need
// percent-encoding (e.g. "gemini/2.5 flash") is decoded once on the way in and re-encoded
// cleanly to core — never doubly-encoded or injected raw into the query string.
func TestLLMSpendForwardsModelPercentEncoded(t *testing.T) {
	core, captured := fakeSpendCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doLLMSpend(s, "model="+url.QueryEscape("gemini/2.5 flash"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Assert the raw forwarded query too — proves it's re-escaped once (gemini%2F2.5+flash),
	// not passed through raw or double-encoded, which a decoded-values check would miss.
	wantRaw := "model=" + url.QueryEscape("gemini/2.5 flash")
	if *captured != wantRaw {
		t.Fatalf("forwarded raw query = %q, want %q", *captured, wantRaw)
	}
	assertForwarded(t, *captured, url.Values{"model": {"gemini/2.5 flash"}})
}

func TestLLMSpendForwardsModelAndDays(t *testing.T) {
	core, captured := fakeSpendCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doLLMSpend(s, "model=gemini-flash&days=7")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertForwarded(t, *captured, url.Values{"model": {"gemini-flash"}, "days": {"7"}})
}

func TestLLMSpendRejectsInvalidDays(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}
	for _, raw := range []string{"days=abc", "days=0", "days=-1", "days=400"} {
		rec := doLLMSpend(s, raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400; body=%s", raw, rec.Code, rec.Body.String())
		}
	}
}

// --- CORR-INFER-#4: timeseries + by-provider proxies ------------------------

// fakeChartCore serves the two chart paths and records the forwarded query string.
func fakeChartCore(t *testing.T, token string) (*httptest.Server, *string) {
	t.Helper()
	captured := new(string)
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		*captured = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/llm-spend/timeseries", h)
	mux.HandleFunc("GET /v1/llm-spend/by-provider", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, captured
}

func doChart(s *server, handler http.HandlerFunc, base, rawQuery string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	target := base
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	handler(rec, httptest.NewRequest("GET", target, nil))
	return rec
}

func TestLLMSpendChartsForwardDays(t *testing.T) {
	core, captured := fakeChartCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		base    string
	}{
		{"timeseries", s.handleLLMSpendTimeseries, "/api/llm-spend/timeseries"},
		{"by-provider", s.handleLLMSpendByProvider, "/api/llm-spend/by-provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			*captured = ""
			rec := doChart(s, c.handler, c.base, "days=30")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			assertForwarded(t, *captured, url.Values{"days": {"30"}})
		})
	}
}

func TestLLMSpendChartsNoQueryForwarded(t *testing.T) {
	core, captured := fakeChartCore(t, "tok")
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}
	*captured = "sentinel"
	rec := doChart(s, s.handleLLMSpendTimeseries, "/api/llm-spend/timeseries", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if *captured != "" {
		t.Errorf("expected no query forwarded, got %q", *captured)
	}
}

func TestLLMSpendChartsRejectInvalidDays(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "x", client: http.DefaultClient}
	handlers := map[string]http.HandlerFunc{
		"/api/llm-spend/timeseries":  s.handleLLMSpendTimeseries,
		"/api/llm-spend/by-provider": s.handleLLMSpendByProvider,
	}
	for base, handler := range handlers {
		for _, raw := range []string{"days=abc", "days=0", "days=-1", "days=400"} {
			rec := doChart(s, handler, base, raw)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s %q: status = %d, want 400; body=%s", base, raw, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestLLMSpendChartsReturn502OnCoreError(t *testing.T) {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	mux.HandleFunc("GET /v1/llm-spend/timeseries", fail)
	mux.HandleFunc("GET /v1/llm-spend/by-provider", fail)
	core := httptest.NewServer(mux)
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	if rec := doChart(s, s.handleLLMSpendTimeseries, "/api/llm-spend/timeseries", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("timeseries status = %d, want 502", rec.Code)
	}
	if rec := doChart(s, s.handleLLMSpendByProvider, "/api/llm-spend/by-provider", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("by-provider status = %d, want 502", rec.Code)
	}
}

func TestLLMSpendReturns502OnCoreError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/llm-spend", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	core := httptest.NewServer(mux)
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "tok", client: core.Client()}

	rec := doLLMSpend(s, "")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
