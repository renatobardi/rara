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
