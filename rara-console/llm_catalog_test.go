package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A trimmed litellm model_prices file: two chat models, one non-chat (embedding), and the
// bogus sample_spec doc entry the real file ships with — the normalizer must drop the last two.
// sample_spec types max_tokens as a descriptive STRING (as the real file does), so this fixture
// also guards the per-entry decode: a strict whole-map decode would fail on it and abort everything.
const fakeCatalogJSON = `{
  "sample_spec": {"max_tokens": "LEGACY parameter, use max_input/output_tokens", "input_cost_per_token": 0.0, "litellm_provider": "one of", "mode": "one of: chat, embedding"},
  "groq/llama-3.3-70b-versatile": {"litellm_provider": "groq", "input_cost_per_token": 5.9e-7, "output_cost_per_token": 7.9e-7, "max_tokens": 32768, "max_input_tokens": 128000, "mode": "chat"},
  "text-embedding-3-small": {"litellm_provider": "openai", "input_cost_per_token": 2e-8, "mode": "embedding"},
  "gemini/gemini-2.0-flash": {"litellm_provider": "gemini", "input_cost_per_token": 1e-7, "output_cost_per_token": 4e-7, "max_tokens": 8192, "max_input_tokens": 1048576, "mode": "chat"}
}`

func fakeCatalogServer(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(fakeCatalogJSON)); err != nil {
			t.Errorf("fakeCatalogServer: write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCatalogServer(catalogURL string, ttl time.Duration) *server {
	return &server{
		catalogURL:    catalogURL,
		catalogTTL:    ttl,
		catalog:       &catalogCache{},
		previewClient: &http.Client{Timeout: 5 * time.Second},
		// retryAfter 0: every expired call re-attempts the fetch, so the failure/stale paths below
		// actually exercise fetchCatalog. The throttle itself is covered by its own test.
	}
}

func getCatalog(t *testing.T, s *server) []llmCatalogEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleLLMCatalog(rec, httptest.NewRequest("GET", "/api/llm-catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	var out []llmCatalogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode catalog: %v (body=%s)", err, rec.Body)
	}
	return out
}

// Normalizes to the slim shape and keeps only chat models (drops sample_spec + embedding).
func TestLLMCatalogNormalizesChatOnly(t *testing.T) {
	core := fakeCatalogServer(t, nil)
	s := newCatalogServer(core.URL, time.Hour)

	out := getCatalog(t, s)
	if len(out) != 2 {
		t.Fatalf("want 2 chat entries, got %d: %+v", len(out), out)
	}
	// Sorted by upstream, so gemini precedes groq.
	if out[0].Upstream != "gemini/gemini-2.0-flash" || out[1].Upstream != "groq/llama-3.3-70b-versatile" {
		t.Errorf("unexpected/unsorted upstreams: %+v", out)
	}
	groq := out[1]
	if groq.Provider != "groq" || groq.InputCostPerToken != 5.9e-7 || groq.OutputCostPerToken != 7.9e-7 ||
		groq.MaxTokens != 32768 || groq.Mode != "chat" {
		t.Errorf("groq entry not normalized correctly: %+v", groq)
	}
}

// The slim DTO must expose only the agreed fields — never the full litellm blob (extra keys like
// max_output_tokens, cache costs, supports_* flags, etc.).
func TestLLMCatalogExposesOnlySlimFields(t *testing.T) {
	core := fakeCatalogServer(t, nil)
	s := newCatalogServer(core.URL, time.Hour)

	rec := httptest.NewRecorder()
	s.handleLLMCatalog(rec, httptest.NewRequest("GET", "/api/llm-catalog", nil))

	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	allowed := map[string]bool{
		"upstream": true, "provider": true, "input_cost_per_token": true,
		"output_cost_per_token": true, "max_tokens": true, "mode": true,
	}
	for _, row := range raw {
		for k := range row {
			if !allowed[k] {
				t.Errorf("catalog row leaked field %q: %v", k, row)
			}
		}
	}
}

// A second request inside the TTL is served from cache without re-hitting upstream.
func TestLLMCatalogCacheHitNoRefetch(t *testing.T) {
	var hits int64
	core := fakeCatalogServer(t, &hits)
	s := newCatalogServer(core.URL, time.Hour)

	getCatalog(t, s)
	getCatalog(t, s)
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("want 1 upstream fetch (second served from cache), got %d", got)
	}
}

// After a failed refetch, retries are throttled to catalogRetryAfter so a rapid burst of requests
// during an upstream outage serves stale data without re-attempting (and serializing on) the fetch.
func TestLLMCatalogThrottlesRefetchAfterFailure(t *testing.T) {
	var hits int64
	var fail atomic.Bool // read in the handler goroutine, written here — must be synchronized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		if fail.Load() {
			http.Error(w, "upstream down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(fakeCatalogJSON)); err != nil {
			t.Errorf("throttle test server: write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	s := newCatalogServer(srv.URL, 0) // TTL 0 → cache is always "expired"
	s.catalogRetryAfter = time.Hour   // but a failed attempt parks retries for an hour

	getCatalog(t, s) // primes the cache (hit #1)
	fail.Store(true)
	getCatalog(t, s) // TTL expired → attempts, fails, serves stale (hit #2), arms the throttle
	getCatalog(t, s) // within retry window → must NOT attempt, serves stale
	getCatalog(t, s)
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("want 2 upstream attempts (1 prime + 1 failed, rest throttled), got %d", got)
	}
}

// A failed refetch after the TTL expires serves the stale cache instead of erroring.
func TestLLMCatalogServesStaleOnFetchFailure(t *testing.T) {
	var hits int64
	core := fakeCatalogServer(t, &hits)
	s := newCatalogServer(core.URL, 0) // TTL 0 → every call is "expired", forcing a refetch attempt

	first := getCatalog(t, s)
	if len(first) != 2 {
		t.Fatalf("primer fetch failed: %+v", first)
	}
	core.Close() // upstream now unreachable

	second := getCatalog(t, s)
	if len(second) != 2 {
		t.Errorf("did not serve stale cache after upstream failure: %+v", second)
	}
}

// With no cache to fall back on, an upstream failure is a 502 (never a 200 with empty data).
func TestLLMCatalogBadGatewayWhenNoCache(t *testing.T) {
	core := fakeCatalogServer(t, nil)
	url := core.URL
	core.Close() // dead upstream, cold cache
	s := newCatalogServer(url, time.Hour)

	rec := httptest.NewRecorder()
	s.handleLLMCatalog(rec, httptest.NewRequest("GET", "/api/llm-catalog", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("want 502 on cold-cache upstream failure, got %d", rec.Code)
	}
}
