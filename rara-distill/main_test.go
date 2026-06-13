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
// Mocks
// ---------------------------------------------------------------------------

// mockTranscript mirrors a row of the upstream transcripts table.
type mockTranscript struct {
	doc    SourceDoc
	status string // done | failed | empty
}

// MockDatabase encodes the *intended* PendingDocs behaviour: filter to done/non-empty
// transcripts, skip a doc only when a fresh distillation exists for the recipe (done,
// same source hash, same recipe hash), and skip failures past the retry cap;
// SaveDistillation upserts on (source_key, COALESCE(session_patterns, pattern)).
//
// It cannot reproduce SQL three-valued (NULL) logic, so it is not a substitute for
// exercising the real query — see TestPendingDocsIntegration, which runs the actual
// SQL against Postgres and guards the never-distilled / failed-cap NULL edge cases.
type MockDatabase struct {
	transcripts   []mockTranscript
	distillations map[string]Distillation // keyed by source_key|keyPattern
	attempts      map[string]int
	saveErr       error
	pendingErr    error
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		distillations: make(map[string]Distillation),
		attempts:      make(map[string]int),
	}
}

func distKey(sourceKey, keyPattern string) string { return sourceKey + "|" + keyPattern }

func (m *MockDatabase) PendingDocs(ctx context.Context, limit int, keyPattern, recipeSHA string) ([]SourceDoc, error) {
	if m.pendingErr != nil {
		return nil, m.pendingErr
	}
	var fresh, retry []SourceDoc
	for _, mt := range m.transcripts {
		if mt.status != statusDone || strings.TrimSpace(mt.doc.Transcript) == "" {
			continue // mirror: only done, non-empty transcripts are eligible
		}
		key := distKey(mt.doc.SourceKey, keyPattern)
		d, ok := m.distillations[key]
		switch {
		case ok && d.Status == statusDone && d.SourceSHA256 == mt.doc.SourceSHA256 && d.RecipeSHA256 == recipeSHA:
			continue // fresh: nothing changed
		case ok && d.Status == statusFailed && m.attempts[key] >= maxFailedAttempts:
			continue // exhausted retries
		case ok && d.Status == statusFailed:
			retry = append(retry, mt.doc)
		default:
			fresh = append(fresh, mt.doc) // never distilled, or stale source/recipe
		}
	}
	out := append(fresh, retry...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MockDatabase) SaveDistillation(ctx context.Context, d Distillation) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	keyPattern := d.Pattern
	if d.SessionPatterns != "" {
		keyPattern = d.SessionPatterns
	}
	key := distKey(d.SourceKey, keyPattern)
	m.distillations[key] = d
	if d.Status == statusFailed {
		m.attempts[key]++
	} else {
		m.attempts[key] = 0
	}
	return nil
}

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

// ---------------------------------------------------------------------------
// Fluent harness
// ---------------------------------------------------------------------------

type DistillHarness struct {
	t          *testing.T
	db         *MockDatabase
	cur        *MockCurator
	recipe     Recipe
	engineName string
}

func NewDistillHarness(t *testing.T) *DistillHarness {
	t.Helper()
	r, err := NewRecipe(Config{Patterns: "extract_wisdom"})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	return &DistillHarness{
		t:          t,
		db:         newMockDatabase(),
		cur:        &MockCurator{},
		recipe:     r,
		engineName: "mock/engine",
	}
}

// WithRecipe overrides the recipe (e.g. to test context/strategy/sessions).
func (h *DistillHarness) WithRecipe(cfg Config) *DistillHarness {
	r, err := NewRecipe(cfg)
	if err != nil {
		h.t.Fatalf("recipe: %v", err)
	}
	h.recipe = r
	return h
}

// WithTranscript registers an upstream transcript (done, non-empty by default).
func (h *DistillHarness) WithTranscript(doc SourceDoc) *DistillHarness {
	h.db.transcripts = append(h.db.transcripts, mockTranscript{doc: doc, status: statusDone})
	return h
}

// WithRawTranscript registers an upstream transcript with an explicit status.
func (h *DistillHarness) WithRawTranscript(doc SourceDoc, status string) *DistillHarness {
	h.db.transcripts = append(h.db.transcripts, mockTranscript{doc: doc, status: status})
	return h
}

// WithResponses sets the curator responses returned in call order.
func (h *DistillHarness) WithResponses(responses ...string) *DistillHarness {
	h.cur.results = append(h.cur.results, responses...)
	return h
}

func (h *DistillHarness) WithCurateError(err error) *DistillHarness {
	h.cur.err = err
	return h
}

func (h *DistillHarness) Execute(ctx context.Context, limit int) error {
	return runBatch(ctx, h.db, h.cur, h.engineName, h.recipe, limit)
}

func (h *DistillHarness) get(sourceKey string) (Distillation, bool) {
	d, ok := h.db.distillations[distKey(sourceKey, h.recipe.keyPattern())]
	return d, ok
}

func (h *DistillHarness) AssertCount(expected int) {
	h.t.Helper()
	if len(h.db.distillations) != expected {
		h.t.Errorf("distillation count = %d, want %d", len(h.db.distillations), expected)
	}
}

func (h *DistillHarness) AssertStatus(sourceKey, status string) {
	h.t.Helper()
	d, ok := h.get(sourceKey)
	if !ok {
		h.t.Errorf("distillation %q not found", sourceKey)
		return
	}
	if d.Status != status {
		h.t.Errorf("distillation %q status = %q, want %q", sourceKey, d.Status, status)
	}
}

func (h *DistillHarness) AssertStructuredStatus(sourceKey, status string) {
	h.t.Helper()
	d, ok := h.get(sourceKey)
	if !ok {
		h.t.Errorf("distillation %q not found", sourceKey)
		return
	}
	if d.StructuredStatus != status {
		h.t.Errorf("distillation %q structured_status = %q, want %q", sourceKey, d.StructuredStatus, status)
	}
}

// ---------------------------------------------------------------------------
// Harness tests
// ---------------------------------------------------------------------------

// TestHappyPath: a single transcript is curated into one distillation with the
// structured payload and doc_context persisted.
func TestHappyPath(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{YoutubeVideoID: "vid1", SourceType: "youtube", SourceRef: videoRef("vid1"), SourceKey: "vid1", Title: "Talk", Transcript: "hello world", SourceSHA256: "h1"}).
		WithResponses(curationJSON("# Curated note", "A talk about hello."))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertCount(1)
	h.AssertStatus("vid1", statusDone)
	h.AssertStructuredStatus("vid1", structOK)

	d, _ := h.get("vid1")
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
}

// TestOnlyDoneNonEmptyAreQueued: failed/empty transcripts are excluded from the
// work queue.
func TestOnlyDoneNonEmptyAreQueued(t *testing.T) {
	h := NewDistillHarness(t).
		WithRawTranscript(SourceDoc{SourceKey: "failed1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}, statusFailed).
		WithRawTranscript(SourceDoc{SourceKey: "empty1", SourceType: "youtube", SourceRef: "r", Transcript: "", SourceSHA256: "h"}, statusDone).
		WithTranscript(SourceDoc{SourceKey: "good1", SourceType: "youtube", SourceRef: "r", Transcript: "real", SourceSHA256: "h"}).
		WithResponses(curationJSON("# ok", "d"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertCount(1) // only good1
	h.AssertStatus("good1", statusDone)
}

// TestMalformedOutputIsVisibleNotFatal: a non-JSON model response keeps the row
// (status done) but flags structured_status=parse_failed and keeps content='{}'.
func TestMalformedOutputIsVisibleNotFatal(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}).
		WithResponses("oops, not json")

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertStatus("vid1", statusDone)
	h.AssertStructuredStatus("vid1", structParseFailed)
	d, _ := h.get("vid1")
	if string(d.Structured) != "{}" {
		t.Errorf("structured = %s, want {}", d.Structured)
	}
	if d.Content != "oops, not json" {
		t.Errorf("content = %q, want raw text preserved", d.Content)
	}
}

// TestCurateErrorMarksFailed: a curator error is captured and the row is failed.
func TestCurateErrorMarksFailed(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}).
		WithCurateError(errors.New("gemini 500"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertStatus("vid1", statusFailed)
	d, _ := h.get("vid1")
	if d.Error == "" {
		t.Error("expected failed distillation to carry an error")
	}
}

// TestContextInjectedIntoPrompt: the configured context appears in the system prompt.
func TestContextInjectedIntoPrompt(t *testing.T) {
	h := NewDistillHarness(t).
		WithRecipe(Config{Patterns: "extract_wisdom", ContextName: "software-ai"}).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}).
		WithResponses(curationJSON("# ok", "d"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(h.cur.calls) != 1 {
		t.Fatalf("expected 1 curate call, got %d", len(h.cur.calls))
	}
	if !strings.Contains(h.cur.calls[0].system, "Platform Engineering") {
		t.Error("context not injected into system prompt")
	}
}

// TestStrategyWrapsPattern: the configured strategy appears in the system prompt.
func TestStrategyWrapsPattern(t *testing.T) {
	h := NewDistillHarness(t).
		WithRecipe(Config{Patterns: "extract_wisdom", StrategyName: "cot"}).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}).
		WithResponses(curationJSON("# ok", "d"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(h.cur.calls[0].system, "Chain of Thought") {
		t.Error("strategy not applied to system prompt")
	}
}

// TestSessionChainsOutput: a two-pattern session runs twice and the second stage's
// input contains the first stage's output.
func TestSessionChainsOutput(t *testing.T) {
	h := NewDistillHarness(t).
		WithRecipe(Config{Patterns: "summary,extract_wisdom"}).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "original transcript", SourceSHA256: "h"}).
		WithResponses(
			curationJSON("STAGE-ONE-SUMMARY", "d1"),
			curationJSON("# Final note", "d2"),
		)

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(h.cur.calls) != 2 {
		t.Fatalf("expected 2 curate calls (session), got %d", len(h.cur.calls))
	}
	// Stage 2 sees the original transcript AND stage 1's output.
	if !strings.Contains(h.cur.calls[1].input, "original transcript") ||
		!strings.Contains(h.cur.calls[1].input, "STAGE-ONE-SUMMARY") {
		t.Errorf("stage 2 input missing prior output: %q", h.cur.calls[1].input)
	}
	// The final distillation uses stage 2's output.
	d, _ := h.get("vid1")
	if d.Content != "# Final note" {
		t.Errorf("final content = %q, want stage-2 output", d.Content)
	}
	if d.SessionPatterns != "summary,extract_wisdom" {
		t.Errorf("session_patterns = %q", d.SessionPatterns)
	}
}

// TestFirstStageUsesTimestampedTranscript: when timestamped segments are available, the
// model is fed the [seconds]-prefixed transcript so it can populate claims[].ts_start.
func TestFirstStageUsesTimestampedTranscript(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{
			SourceKey: "vid1", SourceType: "youtube", SourceRef: "r",
			Transcript:            "flat text",
			TranscriptTimestamped: "[0] hello\n[12] world",
			SourceSHA256:          "h",
		}).
		WithResponses(curationJSON("# n", "d"))
	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	in := h.cur.calls[0].input
	if !strings.Contains(in, "[12] world") {
		t.Errorf("first-stage input should use the timestamped transcript, got %q", in)
	}
	if strings.Contains(in, "flat text") {
		t.Errorf("timestamped transcript present but flat text was used: %q", in)
	}
}

// TestFirstStageFallsBackToFlatTranscript: with no segments, the flat transcript is used.
func TestFirstStageFallsBackToFlatTranscript(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{
			SourceKey: "vid1", SourceType: "youtube", SourceRef: "r",
			Transcript: "flat only", SourceSHA256: "h",
		}).
		WithResponses(curationJSON("# n", "d"))
	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(h.cur.calls[0].input, "flat only") {
		t.Errorf("expected flat transcript fallback, got %q", h.cur.calls[0].input)
	}
}

// TestSessionDoesNotCollideWithStandalone: a standalone extract_wisdom and a session
// ending in extract_wisdom over the same source are two distinct rows.
func TestSessionDoesNotCollideWithStandalone(t *testing.T) {
	doc := SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}
	db := newMockDatabase()

	// Standalone first.
	hStandalone := &DistillHarness{t: t, db: db, cur: &MockCurator{results: []string{curationJSON("# a", "d")}}, engineName: "mock/engine"}
	hStandalone.recipe, _ = NewRecipe(Config{Patterns: "extract_wisdom"})
	hStandalone.db.transcripts = []mockTranscript{{doc: doc, status: statusDone}}
	if err := hStandalone.Execute(context.Background(), 25); err != nil {
		t.Fatalf("standalone execute: %v", err)
	}

	// Session over the same source, sharing the DB.
	hSession := &DistillHarness{t: t, db: db, cur: &MockCurator{results: []string{curationJSON("# s1", "d"), curationJSON("# s2", "d")}}, engineName: "mock/engine"}
	hSession.recipe, _ = NewRecipe(Config{Patterns: "summary,extract_wisdom"})
	if err := hSession.Execute(context.Background(), 25); err != nil {
		t.Fatalf("session execute: %v", err)
	}

	if len(db.distillations) != 2 {
		t.Errorf("want 2 distinct rows (standalone + session), got %d", len(db.distillations))
	}
}

// TestUrlSourceNoDuplicate: a url source (NULL youtube id) dedups by source_key — a
// re-run replaces the row rather than creating a second one.
func TestUrlSourceNoDuplicate(t *testing.T) {
	doc := SourceDoc{YoutubeVideoID: "", SourceType: "url", SourceRef: "https://x/y", SourceKey: "https://x/y", Transcript: "x", SourceSHA256: "h"}
	h := NewDistillHarness(t).
		WithTranscript(doc).
		WithResponses(curationJSON("# a", "d"), curationJSON("# b", "d"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	// Force a re-run by clearing the recipe match (simulate stale): bump the saved
	// row's recipe so it is pending again, then re-run.
	key := distKey(doc.SourceKey, h.recipe.keyPattern())
	saved := h.db.distillations[key]
	saved.RecipeSHA256 = "old"
	h.db.distillations[key] = saved

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if len(h.db.distillations) != 1 {
		t.Errorf("url source duplicated: want 1 row, got %d", len(h.db.distillations))
	}
}

// TestIdempotentReRun: re-running with the same recipe and unchanged source does not
// re-curate (PendingDocs excludes the fresh row).
func TestIdempotentReRun(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h1"}).
		WithResponses(curationJSON("# ok", "d"))

	ctx := context.Background()
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("first run: %v", err)
	}
	h.AssertCount(1)
	callsAfterFirst := len(h.cur.calls)

	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("second run: %v", err)
	}
	h.AssertCount(1)
	if len(h.cur.calls) != callsAfterFirst {
		t.Errorf("idempotent re-run should not re-curate: calls %d -> %d", callsAfterFirst, len(h.cur.calls))
	}
}

// TestRecipeChangeReprocesses: changing the recipe (e.g. editing a pattern, swapping
// engine) re-curates the same unchanged transcript.
func TestRecipeChangeReprocesses(t *testing.T) {
	doc := SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h1"}
	h := NewDistillHarness(t).
		WithTranscript(doc).
		WithResponses(curationJSON("# v1", "d"))

	ctx := context.Background()
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("first run: %v", err)
	}
	callsAfterFirst := len(h.cur.calls)

	// Simulate a recipe edit: a different recipe hash for the same patterns.
	h.recipe.RecipeSHA = "different-recipe-hash"
	h.cur.results = append(h.cur.results, curationJSON("# v2", "d"))

	pending, err := h.db.PendingDocs(ctx, 25, h.recipe.keyPattern(), h.recipe.RecipeSHA)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("recipe change should make the doc pending again, got %d", len(pending))
	}
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(h.cur.calls) <= callsAfterFirst {
		t.Error("recipe change must re-curate")
	}
	h.AssertCount(1) // still one row, updated in place
}

// TestSourceChangeReprocesses: an updated transcript (new source hash) re-curates.
func TestSourceChangeReprocesses(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{SourceKey: "vid1", SourceType: "youtube", SourceRef: "r", Transcript: "old", SourceSHA256: "h1"}).
		WithResponses(curationJSON("# ok", "d"), curationJSON("# ok2", "d"))

	ctx := context.Background()
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("first run: %v", err)
	}
	callsAfterFirst := len(h.cur.calls)

	// The transcript changed upstream.
	h.db.transcripts[0].doc.Transcript = "new content"
	h.db.transcripts[0].doc.SourceSHA256 = "h2"

	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(h.cur.calls) <= callsAfterFirst {
		t.Error("source change must re-curate")
	}
}

// TestExhaustedFailureLeavesBacklog: a doc that fails every run is retried up to the
// cap and then drops out of the pending set.
func TestExhaustedFailureLeavesBacklog(t *testing.T) {
	h := NewDistillHarness(t).
		WithTranscript(SourceDoc{SourceKey: "perma", SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"}).
		WithCurateError(errors.New("permanent boom"))

	ctx := context.Background()
	for i := 0; i < maxFailedAttempts; i++ {
		if err := h.Execute(ctx, 25); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	h.AssertStatus("perma", statusFailed)

	pending, err := h.db.PendingDocs(ctx, 25, h.recipe.keyPattern(), h.recipe.RecipeSHA)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("exhausted doc still pending: %+v", pending)
	}
}

// TestBatchSizeLimit: only `limit` docs are processed per run.
func TestBatchSizeLimit(t *testing.T) {
	h := NewDistillHarness(t)
	for _, id := range []string{"a", "b", "c"} {
		h.WithTranscript(SourceDoc{SourceKey: id, SourceType: "youtube", SourceRef: "r", Transcript: "x", SourceSHA256: "h"})
	}
	h.WithResponses(curationJSON("# ok", "d"))

	if err := h.Execute(context.Background(), 2); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertCount(2) // limited to 2 of 3
}

// videoRef is a tiny helper mirroring the canonical watch URL for test readability.
func videoRef(id string) string { return "https://www.youtube.com/watch?v=" + id }

// ---------------------------------------------------------------------------
// Integration test (real SQL) — opt-in via DISTILL_TEST_DATABASE_URL.
//
// The unit tests above use MockDatabase, which cannot reproduce SQL three-valued
// (NULL) logic. This test runs the actual PendingDocs query against Postgres to guard
// the NULL edge cases that a mock will always miss: a never-distilled transcript MUST
// be returned (the case a naive NOT (... ) clause silently drops), a fresh one MUST be
// skipped, a failure under the cap MUST be retried, and one past the cap MUST be
// excluded. Skipped unless DISTILL_TEST_DATABASE_URL points at a throwaway Postgres.
// ---------------------------------------------------------------------------

func TestPendingDocsIntegration(t *testing.T) {
	dsn := os.Getenv("DISTILL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DISTILL_TEST_DATABASE_URL to a throwaway Postgres to run the integration test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	const schema = "distill_it_test"
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
	exec(`CREATE TABLE distillations (
		id SERIAL PRIMARY KEY,
		youtube_video_id TEXT, source_type TEXT NOT NULL, source_ref TEXT NOT NULL,
		source_key TEXT NOT NULL, pattern TEXT NOT NULL, context TEXT, strategy TEXT,
		session_patterns TEXT, engine TEXT NOT NULL, title TEXT, content TEXT,
		structured JSONB NOT NULL DEFAULT '{}', structured_status TEXT NOT NULL DEFAULT 'ok',
		doc_context TEXT, source_sha256 TEXT NOT NULL, recipe_sha256 TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'done', error TEXT, attempt_count INT NOT NULL DEFAULT 0)`)
	exec("CREATE UNIQUE INDEX uq ON distillations (source_key, COALESCE(session_patterns, pattern))")

	all := []string{"never", "fresh_done", "failed_under", "failed_over"}
	for _, v := range all {
		var tid int
		if err := conn.QueryRow(ctx,
			`INSERT INTO transcripts (youtube_video_id, source_type, source_ref, transcript, status)
			 VALUES ($1, 'youtube', $2, $3, 'done') RETURNING id`,
			v, "https://y/"+v, "body of "+v).Scan(&tid); err != nil {
			t.Fatalf("insert transcript: %v", err)
		}
		exec("INSERT INTO channel_videos (youtube_video_id, title) VALUES ($1, $2)", v, "Title "+v)
		// Give "never" two timestamped segments so we can assert the [seconds] build.
		if v == "never" {
			exec(`INSERT INTO transcript_segments (transcript_id, seq, start_seconds, end_seconds, text)
				VALUES ($1, 0, 0, 5, 'hello'), ($1, 1, 12, 18, 'world')`, tid)
		}
	}

	const recipe = "recipe-hash-v1"
	const keyPattern = "extract_wisdom"

	srcHash := func(v string) string {
		var h string
		if err := conn.QueryRow(ctx,
			"SELECT encode(sha256(convert_to($1, 'UTF8')), 'hex')", "body of "+v).Scan(&h); err != nil {
			t.Fatalf("hash: %v", err)
		}
		return h
	}
	insDist := func(v, status string, attempts int) {
		exec(`INSERT INTO distillations
			(youtube_video_id, source_type, source_ref, source_key, pattern, engine,
			 source_sha256, recipe_sha256, status, attempt_count)
			VALUES ($1, 'youtube', $2, $1, $3, 'gemini/x', $4, $5, $6, $7)`,
			v, "https://y/"+v, keyPattern, srcHash(v), recipe, status, attempts)
	}
	// "never" gets no distillation row — the NULL edge that bug-class #1 dropped.
	insDist("fresh_done", statusDone, 0)                       // up to date -> excluded
	insDist("failed_under", statusFailed, maxFailedAttempts-1) // retryable -> included
	insDist("failed_over", statusFailed, maxFailedAttempts)    // cap reached -> excluded

	db := &pgxDatabase{conn: conn}
	docs, err := db.PendingDocs(ctx, 100, keyPattern, recipe)
	if err != nil {
		t.Fatalf("PendingDocs: %v", err)
	}
	got := map[string]bool{}
	byID := map[string]SourceDoc{}
	for _, d := range docs {
		got[d.YoutubeVideoID] = true
		byID[d.YoutubeVideoID] = d
	}
	want := map[string]bool{"never": true, "failed_under": true}
	for _, v := range all {
		if got[v] != want[v] {
			t.Errorf("PendingDocs include[%q] = %v, want %v", v, got[v], want[v])
		}
	}

	// "never" has segments -> the timestamped transcript is built with [seconds] markers.
	if ts := byID["never"].TranscriptTimestamped; ts != "[0] hello\n[12] world" {
		t.Errorf("never.TranscriptTimestamped = %q, want the [seconds]-prefixed build", ts)
	}
	// "failed_under" has no segments -> falls back to the flat transcript.
	if ts := byID["failed_under"].TranscriptTimestamped; ts != "body of failed_under" {
		t.Errorf("failed_under.TranscriptTimestamped = %q, want flat-transcript fallback", ts)
	}
}

// ---------------------------------------------------------------------------
// News lane (DISTILL_SOURCE=news)
// ---------------------------------------------------------------------------

// TestNewsLaneRecipe: the news source mode resolves to the summarize_news pattern
// with the software-ai context (no strategy), and a news item is curated under that
// recipe with its source_type carried through to the distillation.
func TestNewsLaneRecipe(t *testing.T) {
	newsCfg := recipeConfig(Config{Source: sourceModeNews})
	if newsCfg.Patterns != newsPattern {
		t.Fatalf("news recipe patterns = %q, want %q", newsCfg.Patterns, newsPattern)
	}
	if newsCfg.ContextName != newsContext {
		t.Errorf("news recipe context = %q, want %q", newsCfg.ContextName, newsContext)
	}
	if newsCfg.StrategyName != "" {
		t.Errorf("news recipe strategy = %q, want empty", newsCfg.StrategyName)
	}

	const key = "https://openai.com/blog/x"
	h := NewDistillHarness(t).WithRecipe(newsCfg).
		WithTranscript(SourceDoc{
			SourceType: "news", SourceRef: key, SourceKey: key, Title: "GPT-5",
			Transcript: "OpenAI shipped GPT-5.", SourceSHA256: "h1",
		}).
		WithResponses(curationJSON("## TL;DR\nGPT-5 shipped.", "OpenAI news."))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertCount(1)
	h.AssertStatus(key, statusDone)

	d, ok := h.get(key)
	if !ok {
		t.Fatal("news distillation not found")
	}
	if d.SourceType != "news" {
		t.Errorf("source_type = %q, want news", d.SourceType)
	}
	if d.Pattern != newsPattern {
		t.Errorf("pattern = %q, want %q", d.Pattern, newsPattern)
	}

	// The summarize_news system prompt must reach the curator, with the AI/ML context.
	if len(h.cur.calls) == 0 {
		t.Fatal("curator was not called")
	}
	if !strings.Contains(h.cur.calls[0].system, "news editor") {
		t.Error("expected the summarize_news system prompt to be used")
	}
	if !strings.Contains(h.cur.calls[0].system, "REFERENCE CONTEXT") {
		t.Error("expected the software-ai context to be injected")
	}
}
