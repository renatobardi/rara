package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	addon "rara-addon"
)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestSplitCSV(t *testing.T) {
	cases := map[string][]string{
		"":                           nil,
		"extract_wisdom":             {"extract_wisdom"},
		"summary,extract_wisdom":     {"summary", "extract_wisdom"},
		" summary , extract_wisdom ": {"summary", "extract_wisdom"},
		"a,,b,":                      {"a", "b"},
	}
	for in, want := range cases {
		got := splitCSV(in)
		if len(got) != len(want) {
			t.Errorf("splitCSV(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":     0,
		"  ":   0,
		"3":    3 * time.Second,
		"0.05": 50 * time.Millisecond,
		"-1":   0,
		"soon": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 500); got != "short" {
		t.Errorf("truncate short = %q, want unchanged", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q, want abc…", got)
	}
	if got := truncate("áéíóú", 2); got != "áé…" {
		t.Errorf("truncate multibyte = %q, want áé…", got)
	}
}

// ---------------------------------------------------------------------------
// parseCuration: native JSON, fenced JSON, malformed (graceful + visible)
// ---------------------------------------------------------------------------

func TestParseCurationNativeObject(t *testing.T) {
	raw := `{"content_markdown":"# Note","doc_context":"A talk about X.","structured":{"concepts":["x"],"insights":["do x"]}}`
	got := parseCuration(raw)
	if got.Status != structOK {
		t.Errorf("status = %q, want %q", got.Status, structOK)
	}
	if got.Content != "# Note" {
		t.Errorf("content = %q", got.Content)
	}
	if got.DocContext != "A talk about X." {
		t.Errorf("doc_context = %q", got.DocContext)
	}
	if len(got.Structured.Concepts) != 1 || got.Structured.Concepts[0] != "x" {
		t.Errorf("structured.concepts = %v", got.Structured.Concepts)
	}
}

func TestParseCurationFencedBlock(t *testing.T) {
	raw := "Here you go:\n```json\n{\"content_markdown\":\"# N\",\"doc_context\":\"d\",\"structured\":{\"insights\":[\"i\"]}}\n```\nthanks"
	got := parseCuration(raw)
	if got.Status != structOK {
		t.Errorf("status = %q, want ok (fenced should be recovered)", got.Status)
	}
	if got.Content != "# N" {
		t.Errorf("content = %q", got.Content)
	}
}

func TestParseCurationEmptyStructured(t *testing.T) {
	raw := `{"content_markdown":"# N","doc_context":"d","structured":{}}`
	got := parseCuration(raw)
	if got.Status != structEmpty {
		t.Errorf("status = %q, want %q", got.Status, structEmpty)
	}
	if got.Content != "# N" {
		t.Errorf("content = %q, want preserved", got.Content)
	}
}

func TestParseCurationMalformedPreservesText(t *testing.T) {
	raw := "totally not json, just prose"
	got := parseCuration(raw)
	if got.Status != structParseFailed {
		t.Errorf("status = %q, want %q", got.Status, structParseFailed)
	}
	if got.Content != raw {
		t.Errorf("content = %q, want raw preserved", got.Content)
	}
}

// ---------------------------------------------------------------------------
// Recipe hashing: every chain stage (not just the last) affects the hash
// ---------------------------------------------------------------------------

func TestHashRecipeChangesWithFirstStage(t *testing.T) {
	base := hashRecipe([][]byte{[]byte("summary-v1"), []byte("wisdom")}, nil, nil)
	editFirst := hashRecipe([][]byte{[]byte("summary-v2"), []byte("wisdom")}, nil, nil)
	if base == editFirst {
		t.Error("editing the FIRST chain stage must change recipe hash (else silent skip in sessions)")
	}
	editLast := hashRecipe([][]byte{[]byte("summary-v1"), []byte("wisdom2")}, nil, nil)
	if base == editLast {
		t.Error("editing the last chain stage must change recipe hash")
	}
}

func TestHashRecipeChangesWithContextStrategy(t *testing.T) {
	base := hashRecipe([][]byte{[]byte("p")}, nil, nil)
	if base == hashRecipe([][]byte{[]byte("p")}, []byte("ctx"), nil) {
		t.Error("adding a context must change the hash")
	}
	if base == hashRecipe([][]byte{[]byte("p")}, nil, []byte("strat")) {
		t.Error("adding a strategy must change the hash")
	}
}

// TestHashRecipeGolden pins the exact output of the hashing scheme for a fixed input.
// The corpus's staleness detection depends on this hash being STABLE: if a refactor
// silently changes how the bytes are combined, every stored recipe_sha256 would stop
// matching and the whole corpus would be re-distilled. This known-answer test fails
// loudly if the scheme drifts. (Value computed as sha256("p" + 0x00 + "ctx:" + "strat:").)
func TestHashRecipeGolden(t *testing.T) {
	const want = "f928010e9d27eb393dda903318795444d7e4620e4fb71196ea9e3a711fc7bd8c"
	if got := hashRecipe([][]byte{[]byte("p")}, nil, nil); got != want {
		t.Errorf("hash scheme changed: got %s want %s — this invalidates every stored "+
			"recipe_sha256; intentional? then re-stamp the corpus and update this constant", got, want)
	}
}

// TestRecipeHashIndependentOfEngine asserts the production guarantee that two recipes
// built identically (same pattern/context/strategy) hash the same regardless of which
// engine/model will run them. NewRecipe no longer even takes an engine, so the property
// is structural — this guards against it being reintroduced.
func TestRecipeHashIndependentOfEngine(t *testing.T) {
	a, err := NewRecipe(Config{Patterns: "extract_wisdom", GeminiModel: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("NewRecipe a: %v", err)
	}
	b, err := NewRecipe(Config{Patterns: "extract_wisdom", Engine: "claude", ClaudeModel: "claude-opus-4-8"})
	if err != nil {
		t.Fatalf("NewRecipe b: %v", err)
	}
	if a.RecipeSHA != b.RecipeSHA {
		t.Errorf("recipe hash must not depend on engine/model: %s != %s", a.RecipeSHA, b.RecipeSHA)
	}
}

// TestHashRecipeChainOrderMatters: the same two patterns in a different order are a
// different recipe (the chain is ordered).
func TestHashRecipeChainOrderMatters(t *testing.T) {
	ab := hashRecipe([][]byte{[]byte("a"), []byte("b")}, nil, nil)
	ba := hashRecipe([][]byte{[]byte("b"), []byte("a")}, nil, nil)
	if ab == ba {
		t.Error("chain order must affect the recipe hash")
	}
}

// ---------------------------------------------------------------------------
// Recipe building from the embedded library
// ---------------------------------------------------------------------------

func TestNewRecipeLoadsEmbeddedAssets(t *testing.T) {
	r, err := NewRecipe(Config{Patterns: "extract_wisdom", ContextName: "software-ai", StrategyName: "cot"})
	if err != nil {
		t.Fatalf("NewRecipe: %v", err)
	}
	if len(r.Patterns) != 1 || r.Patterns[0] != "extract_wisdom" {
		t.Errorf("patterns = %v", r.Patterns)
	}
	if r.RecipeSHA == "" {
		t.Error("recipe sha empty")
	}
	prompt := r.buildSystemPrompt("extract_wisdom")
	if !strings.Contains(prompt, "knowledge curator") {
		t.Error("system prompt missing pattern body")
	}
	if !strings.Contains(prompt, "REFERENCE CONTEXT") || !strings.Contains(prompt, "Platform Engineering") {
		t.Error("system prompt missing injected context")
	}
	if !strings.Contains(prompt, "Chain of Thought") {
		t.Error("system prompt missing strategy wrapper")
	}
}

func TestNewRecipeUnknownAssetsFail(t *testing.T) {
	if _, err := NewRecipe(Config{Patterns: "nope"}); err == nil {
		t.Error("expected error for unknown pattern")
	}
	if _, err := NewRecipe(Config{Patterns: "summary", ContextName: "nope"}); err == nil {
		t.Error("expected error for unknown context")
	}
	if _, err := NewRecipe(Config{Patterns: "summary", StrategyName: "nope"}); err == nil {
		t.Error("expected error for unknown strategy")
	}
}

func TestNewRecipeDefaultsToExtractWisdom(t *testing.T) {
	r, err := NewRecipe(Config{Patterns: ""})
	if err != nil {
		t.Fatalf("NewRecipe: %v", err)
	}
	if len(r.Patterns) != 1 || r.Patterns[0] != "extract_wisdom" {
		t.Errorf("default patterns = %v, want [extract_wisdom]", r.Patterns)
	}
}

func TestRecipeKeyPattern(t *testing.T) {
	single, _ := NewRecipe(Config{Patterns: "extract_wisdom"})
	if single.keyPattern() != "extract_wisdom" || single.sessionPatterns() != "" {
		t.Errorf("single: key=%q session=%q", single.keyPattern(), single.sessionPatterns())
	}
	session, _ := NewRecipe(Config{Patterns: "summary,extract_wisdom"})
	if session.keyPattern() != "summary,extract_wisdom" || session.sessionPatterns() != "summary,extract_wisdom" {
		t.Errorf("session: key=%q session=%q", session.keyPattern(), session.sessionPatterns())
	}
}

// ---------------------------------------------------------------------------
// Engine factory
// ---------------------------------------------------------------------------

func TestNewCuratorFactory(t *testing.T) {
	// Default ("") and explicit gemini both select Gemini, given a key.
	for _, engine := range []string{"", engineGemini} {
		cur, name, err := NewCurator(Config{Engine: engine, GeminiAPIKey: "k"})
		if err != nil || cur == nil {
			t.Fatalf("engine %q: err=%v cur=%v", engine, err, cur)
		}
		if name != "gemini/"+defaultGeminiModel {
			t.Errorf("engine %q: name = %q", engine, name)
		}
	}

	cur, name, err := NewCurator(Config{Engine: engineClaude, AnthropicAPIKey: "k"})
	if err != nil || cur == nil || name != "claude/"+defaultClaudeModel {
		t.Fatalf("claude: err=%v cur=%v name=%q", err, cur, name)
	}

	cur, name, err = NewCurator(Config{Engine: engineGroq, GroqAPIKey: "k"})
	if err != nil || cur == nil || name != "groq/"+defaultGroqModel {
		t.Fatalf("groq: err=%v cur=%v name=%q", err, cur, name)
	}

	// LiteLLM gateway: selected by base URL (the key is optional), provenance "litellm/<model>".
	cur, name, err = NewCurator(Config{Engine: engineLiteLLM, LiteLLMBaseURL: "http://gw:4000/v1"})
	if err != nil || cur == nil || name != "litellm/"+defaultLiteLLMModel {
		t.Fatalf("litellm: err=%v cur=%v name=%q", err, cur, name)
	}

	// Model override is reflected in the engine string.
	_, name, _ = NewCurator(Config{Engine: engineGemini, GeminiAPIKey: "k", GeminiModel: "gemini-2.5-pro"})
	if name != "gemini/gemini-2.5-pro" {
		t.Errorf("model override name = %q", name)
	}

	// Missing keys → error.
	if _, _, err := NewCurator(Config{Engine: engineGemini}); err == nil {
		t.Error("expected error for gemini without key")
	}
	if _, _, err := NewCurator(Config{Engine: engineClaude}); err == nil {
		t.Error("expected error for claude without key")
	}
	if _, _, err := NewCurator(Config{Engine: engineGroq}); err == nil {
		t.Error("expected error for groq without key")
	}
	if _, _, err := NewCurator(Config{Engine: engineLiteLLM}); err == nil {
		t.Error("expected error for litellm without a base URL")
	}
	// Unknown engine → error.
	if _, _, err := NewCurator(Config{Engine: "ollama", GeminiAPIKey: "k"}); err == nil {
		t.Error("expected error for unknown engine")
	}
}

// TestLiteLLMCuratorEndpointJoin: the gateway base URL is joined to the OpenAI
// chat-completions path, tolerating a trailing slash.
func TestLiteLLMCuratorEndpointJoin(t *testing.T) {
	for _, base := range []string{"http://gw:4000/v1", "http://gw:4000/v1/"} {
		if got := newLiteLLMCurator(base, "", "m").endpoint; got != "http://gw:4000/v1/chat/completions" {
			t.Errorf("base %q -> endpoint %q, want .../v1/chat/completions", base, got)
		}
	}
}

// TestLiteLLMCuratorRequestAndResponse: the curator sends an OpenAI chat-completions
// request (model + system/user messages + json_object format) and returns the message
// content. With a key set it sends a bearer Authorization header.
func TestLiteLLMCuratorRequestAndResponse(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"content_markdown\":\"ok\"}"}}]}`))
	}))
	defer srv.Close()

	c := &liteLLMCurator{apiKey: "secret", model: "claude-sonnet-4-6", endpoint: srv.URL + "/chat/completions"}
	out, err := c.Curate(context.Background(), "sys", "in")
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if !strings.Contains(out, "content_markdown") {
		t.Errorf("unexpected output: %q", out)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want bearer secret", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotBody["model"] != "claude-sonnet-4-6" {
		t.Errorf("request model = %v, want claude-sonnet-4-6", gotBody["model"])
	}
	if _, ok := gotBody["messages"].([]any); !ok {
		t.Errorf("request missing messages array: %v", gotBody["messages"])
	}
}

// TestLiteLLMCuratorOmitsAuthWhenKeyless: a self-hosted gateway may run keyless — no
// Authorization header is sent when the key is empty.
func TestLiteLLMCuratorOmitsAuthWhenKeyless(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	c := &liteLLMCurator{apiKey: "", model: "m", endpoint: srv.URL + "/chat/completions"}
	if _, err := c.Curate(context.Background(), "s", "i"); err != nil {
		t.Fatalf("curate: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("keyless gateway should send no Authorization header, got %q", gotAuth)
	}
}

// ---------------------------------------------------------------------------
// Shared HTTP retry (429/5xx) via the Groq curator over httptest
// ---------------------------------------------------------------------------

func TestCurateRetriesOn429ThenSucceeds(t *testing.T) {
	curateRetryBase = time.Millisecond // shrink backoff for the test
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"content_markdown\":\"ok\"}"}}]}`))
	}))
	defer srv.Close()

	g := &groqCurator{apiKey: "k", model: "m", endpoint: srv.URL}
	out, err := g.Curate(context.Background(), "sys", "in")
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if !strings.Contains(out, "content_markdown") {
		t.Errorf("unexpected output: %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 calls (429 then 200), got %d", got)
	}
}

func TestCurateGivesUpAfterMaxRetries(t *testing.T) {
	curateRetryBase = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	g := &groqCurator{apiKey: "k", model: "m", endpoint: srv.URL}
	if _, err := g.Curate(context.Background(), "s", "i"); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got, want := atomic.LoadInt32(&calls), int32(maxCurateRetries+1); got != want {
		t.Errorf("expected %d calls, got %d", want, got)
	}
}

func TestCurateDoesNotRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	g := &groqCurator{apiKey: "k", model: "m", endpoint: srv.URL}
	if _, err := g.Curate(context.Background(), "s", "i"); err == nil {
		t.Fatal("expected error for 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("400 must not be retried; expected 1 call, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Test doubles: the LLM (Curator) and the domain store (DistillStore)
// ---------------------------------------------------------------------------

// curateCall records one invocation of the mock curator.
type curateCall struct {
	system string
	input  string
}

// MockCurator returns preconfigured responses in call order and records every call,
// so tests can assert prompt assembly and session chaining.
type MockCurator struct {
	results []string
	calls   []curateCall
	err     error
	idx     int
}

func (m *MockCurator) Curate(ctx context.Context, system, input string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.calls = append(m.calls, curateCall{system: system, input: input})
	r := ""
	if m.idx < len(m.results) {
		r = m.results[m.idx]
	} else if len(m.results) > 0 {
		r = m.results[len(m.results)-1]
	}
	m.idx++
	return r, nil
}

// curationJSON builds a valid model response with the given markdown and one concept.
func curationJSON(markdown, docContext string) string {
	b, _ := json.Marshal(CurationOutput{
		ContentMarkdown: markdown,
		DocContext:      docContext,
		Structured:      Structured{Concepts: []string{"c"}, Insights: []string{"i"}},
	})
	return string(b)
}

// MockDistillStore is the in-memory DistillStore for handler tests: a recipe-options blob, a set
// of source docs keyed by SourceRef, and a record of every saved Distillation (with a synthetic
// returned id). Error fields force each seam to fail.
type MockDistillStore struct {
	recipeOptions json.RawMessage
	recipeErr     error
	docs          map[string]SourceDoc // by SourceRef
	loadErr       error
	saved         []Distillation
	saveErr       error
}

func newMockStore() *MockDistillStore { return &MockDistillStore{docs: map[string]SourceDoc{}} }

func (m *MockDistillStore) RecipeOptions(ctx context.Context, flowID, seq int) (json.RawMessage, error) {
	return m.recipeOptions, m.recipeErr
}

func (m *MockDistillStore) LoadSourceDoc(ctx context.Context, sourceRef string) (SourceDoc, bool, error) {
	if m.loadErr != nil {
		return SourceDoc{}, false, m.loadErr
	}
	d, ok := m.docs[sourceRef]
	return d, ok, nil
}

func (m *MockDistillStore) SaveDistillation(ctx context.Context, d Distillation) (int, error) {
	if m.saveErr != nil {
		return 0, m.saveErr
	}
	m.saved = append(m.saved, d)
	return 1000 + len(m.saved), nil // first save -> 1001 (deterministic OutputRef)
}

// videoRef is a tiny helper mirroring the canonical watch URL for test readability.
func videoRef(id string) string { return "https://www.youtube.com/watch?v=" + id }

// ---------------------------------------------------------------------------
// distillDoc — the engine/db-agnostic curation core (driven directly)
// ---------------------------------------------------------------------------

func mustRecipe(t *testing.T, cfg Config) Recipe {
	t.Helper()
	r, err := NewRecipe(cfg)
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	return r
}

// TestDistillDocHappyPath: a transcript is curated into a done distillation with the structured
// payload, doc_context and engine carried through.
func TestDistillDocHappyPath(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom"})
	cur := &MockCurator{results: []string{curationJSON("# Curated note", "A talk about hello.")}}
	d := distillDoc(context.Background(), cur, "mock/engine", r,
		SourceDoc{YoutubeVideoID: "vid1", SourceType: "youtube", SourceRef: videoRef("vid1"), SourceKey: "vid1", Title: "Talk", Transcript: "hello world", SourceSHA256: "h1"})

	if d.Status != statusDone {
		t.Errorf("status = %q, want done", d.Status)
	}
	if d.StructuredStatus != structOK {
		t.Errorf("structured_status = %q, want ok", d.StructuredStatus)
	}
	if d.Content != "# Curated note" {
		t.Errorf("content = %q", d.Content)
	}
	if d.DocContext != "A talk about hello." {
		t.Errorf("doc_context = %q", d.DocContext)
	}
	var s Structured
	if err := json.Unmarshal(d.Structured, &s); err != nil || len(s.Concepts) != 1 {
		t.Errorf("structured persisted wrong: %s (err %v)", d.Structured, err)
	}
	if d.Engine != "mock/engine" {
		t.Errorf("engine = %q", d.Engine)
	}
	if d.Pattern != "extract_wisdom" {
		t.Errorf("pattern = %q", d.Pattern)
	}
}

// TestDistillDocMalformedOutputIsVisibleNotFatal: a non-JSON response keeps the row (done) but
// flags structured_status=parse_failed, keeps structured='{}' and preserves the raw text.
func TestDistillDocMalformedOutputIsVisibleNotFatal(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom"})
	cur := &MockCurator{results: []string{"oops, not json"}}
	d := distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{SourceKey: "vid1", Transcript: "x"})

	if d.Status != statusDone {
		t.Errorf("status = %q, want done", d.Status)
	}
	if d.StructuredStatus != structParseFailed {
		t.Errorf("structured_status = %q, want parse_failed", d.StructuredStatus)
	}
	if string(d.Structured) != "{}" {
		t.Errorf("structured = %s, want {}", d.Structured)
	}
	if d.Content != "oops, not json" {
		t.Errorf("content = %q, want raw text preserved", d.Content)
	}
}

// TestDistillDocCurateErrorMarksFailed: a curator error is captured as a failed distillation
// carrying the error (the handler then fails the step).
func TestDistillDocCurateErrorMarksFailed(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom"})
	cur := &MockCurator{err: errors.New("gemini 500")}
	d := distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{SourceKey: "vid1", Transcript: "x"})

	if d.Status != statusFailed {
		t.Errorf("status = %q, want failed", d.Status)
	}
	if d.Error == "" {
		t.Error("expected failed distillation to carry an error")
	}
}

// TestDistillDocContextInjectedIntoPrompt: the configured context appears in the system prompt.
func TestDistillDocContextInjectedIntoPrompt(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom", ContextName: "software-ai"})
	cur := &MockCurator{results: []string{curationJSON("# ok", "d")}}
	distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{SourceKey: "vid1", Transcript: "x"})

	if len(cur.calls) != 1 {
		t.Fatalf("expected 1 curate call, got %d", len(cur.calls))
	}
	if !strings.Contains(cur.calls[0].system, "Platform Engineering") {
		t.Error("context not injected into system prompt")
	}
}

// TestDistillDocStrategyWrapsPattern: the configured strategy appears in the system prompt.
func TestDistillDocStrategyWrapsPattern(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom", StrategyName: "cot"})
	cur := &MockCurator{results: []string{curationJSON("# ok", "d")}}
	distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{SourceKey: "vid1", Transcript: "x"})

	if !strings.Contains(cur.calls[0].system, "Chain of Thought") {
		t.Error("strategy not applied to system prompt")
	}
}

// TestDistillDocSessionChainsOutput: a two-pattern session runs twice; stage 2's input contains
// the original transcript AND stage 1's output, and the final row uses stage 2's output.
func TestDistillDocSessionChainsOutput(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "summary,extract_wisdom"})
	cur := &MockCurator{results: []string{
		curationJSON("STAGE-ONE-SUMMARY", "d1"),
		curationJSON("# Final note", "d2"),
	}}
	d := distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{SourceKey: "vid1", Transcript: "original transcript"})

	if len(cur.calls) != 2 {
		t.Fatalf("expected 2 curate calls (session), got %d", len(cur.calls))
	}
	if !strings.Contains(cur.calls[1].input, "original transcript") ||
		!strings.Contains(cur.calls[1].input, "STAGE-ONE-SUMMARY") {
		t.Errorf("stage 2 input missing prior output: %q", cur.calls[1].input)
	}
	if d.Content != "# Final note" {
		t.Errorf("final content = %q, want stage-2 output", d.Content)
	}
	if d.SessionPatterns != "summary,extract_wisdom" {
		t.Errorf("session_patterns = %q", d.SessionPatterns)
	}
}

// TestDistillDocUsesTimestampedTranscript: when timestamped segments are available, the model is
// fed the [seconds]-prefixed transcript so it can populate claims[].ts_start.
func TestDistillDocUsesTimestampedTranscript(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom"})
	cur := &MockCurator{results: []string{curationJSON("# n", "d")}}
	distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{
		SourceKey: "vid1", Transcript: "flat text", TranscriptTimestamped: "[0] hello\n[12] world",
	})

	in := cur.calls[0].input
	if !strings.Contains(in, "[12] world") {
		t.Errorf("first-stage input should use the timestamped transcript, got %q", in)
	}
	if strings.Contains(in, "flat text") {
		t.Errorf("timestamped transcript present but flat text was used: %q", in)
	}
}

// TestDistillDocFallsBackToFlatTranscript: with no segments, the flat transcript is used.
func TestDistillDocFallsBackToFlatTranscript(t *testing.T) {
	r := mustRecipe(t, Config{Patterns: "extract_wisdom"})
	cur := &MockCurator{results: []string{curationJSON("# n", "d")}}
	distillDoc(context.Background(), cur, "mock/engine", r, SourceDoc{SourceKey: "vid1", Transcript: "flat only"})

	if !strings.Contains(cur.calls[0].input, "flat only") {
		t.Errorf("expected flat transcript fallback, got %q", cur.calls[0].input)
	}
}

// ---------------------------------------------------------------------------
// recipeResolver — recipe as per-item config (flow_steps.options.recipe)
// ---------------------------------------------------------------------------

// TestRecipeResolverFromConfig: a step's options.recipe selects the pattern/context, overriding
// the worker's env default. This is the old `news` lane expressed as config.
func TestRecipeResolverFromConfig(t *testing.T) {
	rr := newRecipeResolver([]string{"extract_wisdom"}, "", "") // default differs from the configured recipe
	opts := json.RawMessage(`{"recipe":{"patterns":["summarize_news"],"context":"software-ai"}}`)
	r, err := rr.resolve(opts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.keyPattern() != "summarize_news" {
		t.Errorf("pattern = %q, want summarize_news", r.keyPattern())
	}
	prompt := r.buildSystemPrompt("summarize_news")
	if !strings.Contains(prompt, "news editor") {
		t.Error("expected the summarize_news pattern body")
	}
	if !strings.Contains(prompt, "REFERENCE CONTEXT") {
		t.Error("expected the software-ai context injected")
	}
}

// TestRecipeResolverFallsBackToDefault: no recipe in options (nil, empty, or a non-recipe key like
// {"gate":"skip"}) → the worker's env default recipe.
func TestRecipeResolverFallsBackToDefault(t *testing.T) {
	rr := newRecipeResolver(nil, "", "") // empty default -> extract_wisdom
	for _, opts := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"gate":"skip"}`)} {
		r, err := rr.resolve(opts)
		if err != nil {
			t.Fatalf("resolve(%s): %v", opts, err)
		}
		if r.keyPattern() != "extract_wisdom" {
			t.Errorf("opts %s -> pattern %q, want extract_wisdom", opts, r.keyPattern())
		}
	}
}

// TestRecipeResolverBadOptions: malformed options JSON is an error (not a silent default).
func TestRecipeResolverBadOptions(t *testing.T) {
	rr := newRecipeResolver(nil, "", "")
	if _, err := rr.resolve(json.RawMessage(`{not json`)); err == nil {
		t.Error("expected error for malformed options JSON")
	}
}

// TestRecipeResolverRecipeWithoutPatternsErrors: a recipe block present but missing patterns
// (e.g. {"recipe":{"context":"software-ai"}}) is a config error, surfaced loudly rather than
// silently falling back to the default and dropping the context it did set.
func TestRecipeResolverRecipeWithoutPatternsErrors(t *testing.T) {
	rr := newRecipeResolver([]string{"extract_wisdom"}, "", "")
	if _, err := rr.resolve(json.RawMessage(`{"recipe":{"context":"software-ai"}}`)); err == nil {
		t.Error("expected error for a recipe with no patterns")
	}
}

// ---------------------------------------------------------------------------
// distillHandler — the bridge-total claim-worker domain (mock Store + fake LLM)
// ---------------------------------------------------------------------------

// TestHandlerHappyPath: claim an item -> distill -> save -> OutputRef is the new distillation id.
func TestHandlerHappyPath(t *testing.T) {
	store := newMockStore()
	store.docs["vid1"] = SourceDoc{YoutubeVideoID: "vid1", SourceType: "youtube", SourceRef: videoRef("vid1"), SourceKey: "vid1", Title: "Talk", Transcript: "hello world", SourceSHA256: "h1"}
	cur := &MockCurator{results: []string{curationJSON("# Curated", "A talk.")}}
	h := distillHandler(store, cur, "mock/engine", newRecipeResolver(nil, "", ""))

	res, err := h(context.Background(), addon.Item{ID: 1, SourceRef: "vid1", FlowID: 7}, addon.Step{ItemID: 1, Seq: 5, Capability: capDestilar})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.OutputRef != "1001" {
		t.Errorf("OutputRef = %q, want 1001 (the saved distillation id)", res.OutputRef)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d distillations, want 1", len(store.saved))
	}
	if d := store.saved[0]; d.Status != statusDone || d.Content != "# Curated" || d.Pattern != "extract_wisdom" {
		t.Errorf("saved row = %+v", d)
	}
}

// TestHandlerInputNotReadyIsRetryable: the to-text artifact isn't there yet -> ErrRetryable so the
// SDK requeues (the upstream step may lag), and nothing is saved.
func TestHandlerInputNotReadyIsRetryable(t *testing.T) {
	store := newMockStore() // no docs
	h := distillHandler(store, &MockCurator{}, "mock/engine", newRecipeResolver(nil, "", ""))

	_, err := h(context.Background(), addon.Item{SourceRef: "missing", FlowID: 1}, addon.Step{Seq: 1})
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("err = %v, want wrapping addon.ErrRetryable", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("nothing should be saved on a transient miss, saved %d", len(store.saved))
	}
}

// TestHandlerDistillFailedPersistsAndRetries: a curation failure persists the failed row (for
// observability) AND surfaces as retryable, so the SDK requeues up to MaxAttempts (the 1.0
// retry-to-cap) instead of terminally failing the item on the first blip.
func TestHandlerDistillFailedPersistsAndRetries(t *testing.T) {
	store := newMockStore()
	store.docs["vid1"] = SourceDoc{SourceKey: "vid1", SourceRef: "vid1", Transcript: "x"}
	cur := &MockCurator{err: errors.New("llm 500")}
	h := distillHandler(store, cur, "mock/engine", newRecipeResolver(nil, "", ""))

	_, err := h(context.Background(), addon.Item{SourceRef: "vid1", FlowID: 1}, addon.Step{Seq: 1})
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("err = %v, want wrapping addon.ErrRetryable (requeue up to the cap)", err)
	}
	if len(store.saved) != 1 || store.saved[0].Status != statusFailed {
		t.Errorf("expected one persisted failed row, got %+v", store.saved)
	}
}

// TestHandlerSaveErrorPropagates: a store write failure propagates (the step is not silently
// marked done) and is not mistaken for a transient input miss.
func TestHandlerSaveErrorPropagates(t *testing.T) {
	store := newMockStore()
	store.docs["vid1"] = SourceDoc{SourceKey: "vid1", SourceRef: "vid1", Transcript: "x"}
	store.saveErr = errors.New("neon write timeout")
	cur := &MockCurator{results: []string{curationJSON("# ok", "d")}}
	h := distillHandler(store, cur, "mock/engine", newRecipeResolver(nil, "", ""))

	_, err := h(context.Background(), addon.Item{SourceRef: "vid1", FlowID: 1}, addon.Step{Seq: 1})
	if err == nil || !strings.Contains(err.Error(), "neon write timeout") {
		t.Errorf("err = %v, want the save error propagated", err)
	}
}

// TestHandlerRecipeOptionsErrorPropagates: a failure reading the per-step recipe config propagates
// (the handler does not fall back to the default on a real DB error).
func TestHandlerRecipeOptionsErrorPropagates(t *testing.T) {
	store := newMockStore()
	store.recipeErr = errors.New("flow_steps read failed")
	store.docs["vid1"] = SourceDoc{SourceKey: "vid1", SourceRef: "vid1", Transcript: "x"}
	h := distillHandler(store, &MockCurator{}, "mock/engine", newRecipeResolver(nil, "", ""))

	_, err := h(context.Background(), addon.Item{SourceRef: "vid1", FlowID: 1}, addon.Step{Seq: 1})
	if err == nil || !strings.Contains(err.Error(), "flow_steps read failed") {
		t.Errorf("err = %v, want the recipe-options error propagated", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("nothing should be saved when the recipe could not be resolved, saved %d", len(store.saved))
	}
}

// TestHandlerRecipeFromConfig: the handler reads the recipe from the step's options (the news
// recipe) and the curator receives that pattern's prompt; the saved row records the pattern.
func TestHandlerRecipeFromConfig(t *testing.T) {
	store := newMockStore()
	store.recipeOptions = json.RawMessage(`{"recipe":{"patterns":["summarize_news"],"context":"software-ai"}}`)
	store.docs["u"] = SourceDoc{SourceType: "news", SourceRef: "u", SourceKey: "u", Title: "GPT-5", Transcript: "OpenAI shipped GPT-5."}
	cur := &MockCurator{results: []string{curationJSON("## TL;DR", "OpenAI news.")}}
	// Default recipe is extract_wisdom; the config must override it.
	h := distillHandler(store, cur, "mock/engine", newRecipeResolver([]string{"extract_wisdom"}, "", ""))

	if _, err := h(context.Background(), addon.Item{SourceRef: "u", FlowID: 9}, addon.Step{Seq: 5}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(cur.calls) == 0 {
		t.Fatal("curator was not called")
	}
	if !strings.Contains(cur.calls[0].system, "news editor") {
		t.Error("expected the summarize_news system prompt (recipe from config)")
	}
	if !strings.Contains(cur.calls[0].system, "REFERENCE CONTEXT") {
		t.Error("expected the software-ai context injected")
	}
	if d := store.saved[0]; d.Pattern != "summarize_news" || d.SourceType != "news" {
		t.Errorf("saved row recipe wrong: pattern=%q source_type=%q", d.Pattern, d.SourceType)
	}
}

// TestHandlerRecipeDefaultWhenNoConfig: with no options recipe, the worker's env default applies.
func TestHandlerRecipeDefaultWhenNoConfig(t *testing.T) {
	store := newMockStore() // recipeOptions nil
	store.docs["v"] = SourceDoc{SourceKey: "v", SourceRef: "v", Transcript: "x"}
	cur := &MockCurator{results: []string{curationJSON("# ok", "d")}}
	h := distillHandler(store, cur, "mock/engine", newRecipeResolver(nil, "", "")) // default -> extract_wisdom

	if _, err := h(context.Background(), addon.Item{SourceRef: "v", FlowID: 1}, addon.Step{Seq: 1}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if store.saved[0].Pattern != "extract_wisdom" {
		t.Errorf("pattern = %q, want extract_wisdom (default)", store.saved[0].Pattern)
	}
}

// ---------------------------------------------------------------------------
// Integration test (real SQL) — opt-in via DISTILL_TEST_DATABASE_URL.
//
// LoadSourceDoc reads ONE to-text artifact by source_key (the spine's source_ref), with the title
// joined from the collector tables and the timestamped transcript built from transcript_segments.
// This runs the actual query against Postgres to guard the COALESCE source_key match, the
// done/non-empty filter, the not-found path, and the [seconds] build. Skipped unless
// DISTILL_TEST_DATABASE_URL points at a throwaway Postgres.
// ---------------------------------------------------------------------------

func TestLoadSourceDocIntegration(t *testing.T) {
	dsn := os.Getenv("DISTILL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DISTILL_TEST_DATABASE_URL to a throwaway Postgres to run the integration test")
	}
	ctx := context.Background()

	const schema = "distill_it_test"
	// Bootstrap connection: create the throwaway schema + tables + fixtures on a single conn.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %.60q: %v", sql, err)
		}
	}
	exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	exec("SET search_path TO " + schema)

	exec(`CREATE TABLE transcripts (
		id SERIAL PRIMARY KEY, youtube_video_id TEXT, source_type TEXT NOT NULL,
		source_ref TEXT NOT NULL, transcript TEXT, status TEXT NOT NULL)`)
	exec(`CREATE TABLE transcript_segments (
		id SERIAL PRIMARY KEY, transcript_id INT NOT NULL, seq INT NOT NULL,
		start_seconds NUMERIC(10,3) NOT NULL, end_seconds NUMERIC(10,3) NOT NULL,
		text TEXT NOT NULL)`)
	exec(`CREATE TABLE channel_videos (youtube_video_id TEXT, title TEXT)`)
	exec(`CREATE TABLE playlist_videos (youtube_video_id TEXT, title TEXT)`)

	// A youtube transcript (source_key = youtube_video_id) WITH segments, plus an email-style
	// transcript (source_key = source_ref, no youtube id) WITHOUT segments, plus an empty one.
	var ytTID int
	if err := conn.QueryRow(ctx,
		`INSERT INTO transcripts (youtube_video_id, source_type, source_ref, transcript, status)
		 VALUES ('vid1', 'youtube', $1, 'hello world body', 'done') RETURNING id`,
		videoRef("vid1")).Scan(&ytTID); err != nil {
		t.Fatalf("insert yt transcript: %v", err)
	}
	exec("INSERT INTO channel_videos (youtube_video_id, title) VALUES ('vid1', 'A Talk')")
	exec(`INSERT INTO transcript_segments (transcript_id, seq, start_seconds, end_seconds, text)
		VALUES ($1, 0, 0, 5, 'hello'), ($1, 1, 12, 18, 'world')`, ytTID)

	exec(`INSERT INTO transcripts (youtube_video_id, source_type, source_ref, transcript, status)
		VALUES (NULL, 'email', 'msg-42', 'cleaned email body', 'done')`)
	exec(`INSERT INTO transcripts (youtube_video_id, source_type, source_ref, transcript, status)
		VALUES (NULL, 'email', 'msg-empty', '', 'done')`)

	// The appDB reads via a pool; pin search_path on every pooled connection to the test schema.
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	db := &appDB{pool: pool}

	// YouTube source: matched by source_key = youtube_video_id, with the [seconds] build.
	doc, found, err := db.LoadSourceDoc(ctx, "vid1")
	if err != nil || !found {
		t.Fatalf("load vid1: found=%v err=%v", found, err)
	}
	if doc.SourceKey != "vid1" || doc.Title != "A Talk" || doc.YoutubeVideoID != "vid1" {
		t.Errorf("vid1 doc = %+v", doc)
	}
	if doc.TranscriptTimestamped != "[0] hello\n[12] world" {
		t.Errorf("vid1 timestamped = %q, want the [seconds] build", doc.TranscriptTimestamped)
	}

	// Email source: matched by source_key = source_ref (NULL youtube id), flat transcript fallback.
	doc, found, err = db.LoadSourceDoc(ctx, "msg-42")
	if err != nil || !found {
		t.Fatalf("load msg-42: found=%v err=%v", found, err)
	}
	if doc.SourceKey != "msg-42" || doc.YoutubeVideoID != "" || doc.SourceType != "email" {
		t.Errorf("msg-42 doc = %+v", doc)
	}
	if doc.TranscriptTimestamped != "cleaned email body" {
		t.Errorf("msg-42 timestamped = %q, want flat fallback", doc.TranscriptTimestamped)
	}

	// Empty and unknown sources are not ready to distill.
	if _, found, _ := db.LoadSourceDoc(ctx, "msg-empty"); found {
		t.Error("an empty transcript must not be returned (nothing to distill)")
	}
	if _, found, _ := db.LoadSourceDoc(ctx, "nope"); found {
		t.Error("an unknown source_ref must not be found")
	}
}
