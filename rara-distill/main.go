package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	addon "rara-addon"
)

// library holds the Fabric-style curation assets, embedded into the binary so the
// recipe is self-contained (and hashable) at runtime. Editing a markdown file here
// changes the curation behaviour — and, via recipe_sha256, triggers reprocessing.
//
//go:embed patterns contexts strategies
var library embed.FS

// Engine identifiers (CURATE_ENGINE) and their default models. The stored/​hashed
// engine string is the combined "engine/model" form.
const (
	engineGemini  = "gemini"
	engineClaude  = "claude"
	engineGroq    = "groq"
	engineLiteLLM = "litellm"

	defaultGeminiModel  = "gemini-3.1-pro-preview"
	defaultClaudeModel  = "claude-sonnet-4-6"
	defaultGroqModel    = "openai/gpt-oss-120b"
	defaultLiteLLMModel = "claude-sonnet-4-6" // a gateway alias; LiteLLM maps it to a backend
)

// Distillation status values (the distillations.status column).
const (
	statusDone   = "done"   // curated successfully
	statusFailed = "failed" // curation error (retried up to the cap)
)

// structured_status values (observability of the structured extraction).
const (
	structOK          = "ok"           // parsed with content
	structEmpty       = "empty"        // parsed but every field empty
	structParseFailed = "parse_failed" // the model output was not the expected JSON
)

// capDestilar is the logical task this app serves (the capability name in the schema; never
// renamed — only the app name "distill" is evocative). The reconciler routes destilar steps to
// this worker's provider; addon.Run claims them by (capability, assigned_provider).
const capDestilar = "destilar"

const (
	// curateTimeout bounds the LLM work on a single item (a long transcript plus a
	// multi-stage session can take a while), so a hung provider call cannot block the
	// claim loop indefinitely.
	curateTimeout = 5 * time.Minute

	// saveTimeout bounds the per-item database write.
	saveTimeout = 30 * time.Second

	// maxCurateRetries bounds transient-error (429/5xx) retries within a single LLM
	// call before giving up.
	maxCurateRetries = 4

	// HTTP literals shared across the engine clients.
	headerContentType = "Content-Type"
	mimeJSON          = "application/json"
)

// curateRetryBase is the base backoff for transient retries (doubles each attempt)
// when the response carries no Retry-After header. A var so tests can shrink it.
var curateRetryBase = 2 * time.Second

// curateClient is used for the (slower) curation calls — a long transcript plus a
// large completion takes longer than a metadata call, so it gets a generous timeout.
var curateClient = &http.Client{Timeout: 240 * time.Second}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// SourceDoc is a transcript pending distillation, read from the upstream
// transcripts table (joined with the collector tables for the title).
type SourceDoc struct {
	YoutubeVideoID string
	SourceType     string // youtube | url | local
	SourceRef      string
	SourceKey      string // stable dedup key, never empty
	Title          string
	Transcript     string // raw transcript text (flat; hashed for staleness)
	// TranscriptTimestamped is the transcript with per-segment "[seconds] text"
	// prefixes, built from transcript_segments. Fed to the model (when available) so
	// it can anchor claims[].ts_start; falls back to Transcript when there are no
	// segments. Not hashed — staleness tracks the flat Transcript.
	TranscriptTimestamped string
	SourceSHA256          string // hash of the transcript text
}

// Entity is one named thing extracted from the source.
type Entity struct {
	Name string `json:"name"`
	Type string `json:"type"` // person | tech | org | concept
}

// Claim is a notable factual claim with supporting evidence and an optional
// timestamp (seconds) into the source.
type Claim struct {
	Text     string  `json:"text"`
	Evidence string  `json:"evidence"`
	TsStart  float64 `json:"ts_start"`
}

// Structured is the queryable extraction — the "compile once" payload Kura uses for
// RAG/graph without re-deriving it from the markdown.
type Structured struct {
	Concepts    []string `json:"concepts"`
	Insights    []string `json:"insights"`
	References  []string `json:"references"`
	Connections []string `json:"connections"`
	Entities    []Entity `json:"entities"`
	Claims      []Claim  `json:"claims"`
}

// isEmpty reports whether the extraction carries no items at all.
func (s Structured) isEmpty() bool {
	return len(s.Concepts) == 0 && len(s.Insights) == 0 && len(s.References) == 0 &&
		len(s.Connections) == 0 && len(s.Entities) == 0 && len(s.Claims) == 0
}

// CurationOutput is the single JSON object the LLM returns for one pass.
type CurationOutput struct {
	ContentMarkdown string     `json:"content_markdown"`
	DocContext      string     `json:"doc_context"`
	Structured      Structured `json:"structured"`
}

// parsedCuration is the result of parsing a model response: the curation plus a
// structured_status describing how well the extraction came through.
type parsedCuration struct {
	Content    string
	DocContext string
	Structured Structured
	Status     string // structOK | structEmpty | structParseFailed
}

// Distillation is one row of the distillations table.
type Distillation struct {
	YoutubeVideoID   string
	SourceType       string
	SourceRef        string
	SourceKey        string
	Pattern          string // final stage pattern
	Context          string // context name (empty = none)
	Strategy         string // strategy name (empty = none)
	SessionPatterns  string // CSV chain (empty = single pass)
	Engine           string // combined engine/model
	Title            string
	Content          string
	Structured       []byte // JSON ('{}' when none/parse failed)
	StructuredStatus string
	DocContext       string
	SourceSHA256     string
	RecipeSHA256     string
	Status           string // done | failed
	Error            string
}

// Config is the runtime configuration, sourced from environment variables.
type Config struct {
	DatabaseURL     string
	Engine          string
	GeminiAPIKey    string
	AnthropicAPIKey string
	GroqAPIKey      string
	GeminiModel     string
	ClaudeModel     string
	GroqModel       string
	LiteLLMBaseURL  string // LITELLM_BASE_URL: the self-hosted gateway's OpenAI-compatible base
	LiteLLMAPIKey   string // LITELLM_API_KEY: optional gateway master key (omitted if empty)
	LiteLLMModel    string // LITELLM_MODEL: the gateway model alias
	// Patterns/ContextName/StrategyName are the DEFAULT recipe (the fallback used when a
	// flow's destilar step carries no `recipe` in its flow_steps.options). The per-item
	// recipe normally comes from config — see recipeResolver.
	Patterns     string // CSV
	ContextName  string
	StrategyName string
}

// ---------------------------------------------------------------------------
// Interfaces (the seams that make the pipeline unit-testable with zero I/O)
// ---------------------------------------------------------------------------

// DistillStore is the DOMAIN persistence seam the handler needs (distinct from the CONTRACT
// store, which is the SDK's addon.NewPgxStore over item_steps/providers/items). The real
// implementation (appDB) talks to Neon; tests use an in-memory mock.
//
//   - RecipeOptions reads the per-step config (flow_steps.options) so the recipe is data, not a
//     hardcoded mode — the old `news` lane (summarize_news+software-ai) is now just a flow whose
//     destilar step carries that recipe.
//   - LoadSourceDoc reads the to-text artifact for an item (the upstream transcripts row).
//   - SaveDistillation upserts the domain row and returns its id (the OutputRef recorded on the step).
type DistillStore interface {
	RecipeOptions(ctx context.Context, flowID, seq int) (json.RawMessage, error)
	LoadSourceDoc(ctx context.Context, sourceRef string) (SourceDoc, bool, error)
	SaveDistillation(ctx context.Context, d Distillation) (int, error)
}

// Curator is the LLM seam — the single point where Gemini, Claude or Groq are
// swapped, selected by NewCurator. It returns the model's raw response text (a JSON
// object); parsing/degradation lives in the orchestration so it is unit-testable.
type Curator interface {
	Curate(ctx context.Context, systemPrompt, input string) (string, error)
}

// ---------------------------------------------------------------------------
// Recipe (the Fabric-style curation config: patterns + context + strategy)
// ---------------------------------------------------------------------------

// Recipe is the resolved curation config for a run: the ordered pattern chain plus
// optional context/strategy, with the embedded source loaded and a recipe hash that
// changes whenever any input to the curation changes.
type Recipe struct {
	Patterns     []string // ordered chain; len 1 = single pass, >1 = session
	ContextName  string
	StrategyName string
	RecipeSHA    string

	patternSrc  map[string]string
	contextSrc  string
	strategySrc string
}

// keyPattern is the COALESCE(session_patterns, pattern) value for this recipe: the
// CSV chain for a session, the single pattern otherwise.
func (r Recipe) keyPattern() string {
	if len(r.Patterns) > 1 {
		return strings.Join(r.Patterns, ",")
	}
	return r.Patterns[0]
}

// sessionPatterns is the value stored in the session_patterns column (empty for a
// single pass).
func (r Recipe) sessionPatterns() string {
	if len(r.Patterns) > 1 {
		return strings.Join(r.Patterns, ",")
	}
	return ""
}

// buildSystemPrompt assembles the system prompt for one stage: the strategy
// (optional) wraps the pattern, and the context (optional) is appended as reference
// material. The JSON-output contract lives inside the pattern itself.
func (r Recipe) buildSystemPrompt(pattern string) string {
	var b strings.Builder
	if r.strategySrc != "" {
		b.WriteString(r.strategySrc)
		b.WriteString("\n\n")
	}
	b.WriteString(r.patternSrc[pattern])
	if r.contextSrc != "" {
		b.WriteString("\n\n# REFERENCE CONTEXT\n\n")
		b.WriteString(r.contextSrc)
	}
	return b.String()
}

// NewRecipe resolves the recipe from config (the default-recipe env fields). It delegates to
// newRecipeFromSpec; kept as the env-shaped entry point so the curation library's tests are
// unchanged.
func NewRecipe(cfg Config) (Recipe, error) {
	return newRecipeFromSpec(splitCSV(cfg.Patterns), cfg.ContextName, cfg.StrategyName)
}

// newRecipeFromSpec resolves a recipe from an explicit pattern chain + optional context/strategy:
// it loads every embedded asset (failing fast if one is missing) and computes the recipe hash from
// the asset bytes (pattern chain + context + strategy). The engine is not part of it. This is the
// shared core of both the env default (NewRecipe) and the per-item config recipe (recipeResolver).
func newRecipeFromSpec(patterns []string, contextName, strategyName string) (Recipe, error) {
	chain := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if v := strings.TrimSpace(p); v != "" {
			chain = append(chain, v)
		}
	}
	if len(chain) == 0 {
		chain = []string{"extract_wisdom"}
	}

	r := Recipe{
		Patterns:     chain,
		ContextName:  contextName,
		StrategyName: strategyName,
		patternSrc:   make(map[string]string, len(chain)),
	}

	patternBytes := make([][]byte, 0, len(chain))
	for _, p := range chain {
		b, err := library.ReadFile("patterns/" + p + "/system.md")
		if err != nil {
			return Recipe{}, fmt.Errorf("unknown pattern %q (no patterns/%s/system.md)", p, p)
		}
		r.patternSrc[p] = string(b)
		patternBytes = append(patternBytes, b)
	}

	var contextBytes, strategyBytes []byte
	if contextName != "" {
		b, err := library.ReadFile("contexts/" + contextName + ".md")
		if err != nil {
			return Recipe{}, fmt.Errorf("unknown context %q (no contexts/%s.md)", contextName, contextName)
		}
		r.contextSrc = string(b)
		contextBytes = b
	}
	if strategyName != "" {
		b, err := library.ReadFile("strategies/" + strategyName + ".md")
		if err != nil {
			return Recipe{}, fmt.Errorf("unknown strategy %q (no strategies/%s.md)", strategyName, strategyName)
		}
		r.strategySrc = string(b)
		strategyBytes = b
	}

	r.RecipeSHA = hashRecipe(patternBytes, contextBytes, strategyBytes)
	return r, nil
}

// ---------------------------------------------------------------------------
// recipeResolver — the recipe as per-item CONFIG (flow_steps.options.recipe)
// ---------------------------------------------------------------------------

// recipeSpec is the recipe carried in a flow's destilar step config
// (flow_steps.options -> {"recipe": {...}}). Absent/empty patterns fall back to the worker's
// env default. This is how the old `news` lane is expressed without a hardcoded mode.
type recipeSpec struct {
	Patterns []string `json:"patterns"`
	Context  string   `json:"context"`
	Strategy string   `json:"strategy"`
}

// stepOptions is the (partial) shape of flow_steps.options the worker reads; other keys
// (e.g. {"gate":"skip"}) are ignored here.
type stepOptions struct {
	Recipe *recipeSpec `json:"recipe"`
}

// recipeResolver turns a flow step's options into a Recipe, falling back to the env default when
// the step carries none.
type recipeResolver struct {
	defPatterns []string
	defContext  string
	defStrategy string
}

func newRecipeResolver(defPatterns []string, defContext, defStrategy string) *recipeResolver {
	return &recipeResolver{defPatterns: defPatterns, defContext: defContext, defStrategy: defStrategy}
}

// resolve picks the recipe for an item: the step's options.recipe when present (non-empty
// patterns), else the env default. Building a recipe is a few embedded-asset reads + a hash —
// cheap next to the per-item LLM call — so it's rebuilt each time rather than cached.
func (rr *recipeResolver) resolve(optsRaw json.RawMessage) (Recipe, error) {
	patterns, contextName, strategy := rr.defPatterns, rr.defContext, rr.defStrategy
	if len(optsRaw) > 0 {
		var o stepOptions
		if err := json.Unmarshal(optsRaw, &o); err != nil {
			return Recipe{}, fmt.Errorf("parse flow_step options: %w", err)
		}
		if o.Recipe != nil {
			// A recipe override must name its patterns; a recipe block with none (e.g.
			// {"recipe":{"context":"software-ai"}}) is a config error — surfaced loudly rather
			// than silently falling back to the default and ignoring whatever it did set.
			if len(o.Recipe.Patterns) == 0 {
				return Recipe{}, fmt.Errorf("flow_step recipe has no patterns (set recipe.patterns or omit the recipe key)")
			}
			patterns, contextName, strategy = o.Recipe.Patterns, o.Recipe.Context, o.Recipe.Strategy
		}
	}
	return newRecipeFromSpec(patterns, contextName, strategy)
}

// hashRecipe is a pure function over the inputs that define WHAT a distillation must
// contain — the pattern chain, context, and strategy — so a change to ANY of them
// yields a new hash and triggers reprocessing. The engine/model is deliberately NOT
// hashed: swapping models or providers (gemini ↔ claude ↔ a newer gemini) must not
// invalidate an otherwise-good corpus. Which model produced a row is still recorded in
// the `engine` column for provenance; it just isn't a staleness trigger. The pattern
// bytes are concatenated in chain order — the whole chain matters, not just the final
// stage.
func hashRecipe(patterns [][]byte, context, strategy []byte) string {
	h := sha256.New()
	for _, p := range patterns {
		h.Write(p)
		h.Write([]byte{0}) // separator so concatenation is unambiguous
	}
	h.Write([]byte("ctx:"))
	h.Write(context)
	h.Write([]byte("strat:"))
	h.Write(strategy)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Pure helpers (directly unit-tested)
// ---------------------------------------------------------------------------

// splitCSV splits a comma-separated list, trimming spaces and dropping empties.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseCuration turns a model response into a parsedCuration, degrading gracefully:
// native JSON mode returns the object directly; if that fails we try a fenced
// ```json block; if that also fails we keep the raw text as content and flag
// parse_failed so the failure is visible (never silent) but the row still lands.
func parseCuration(raw string) parsedCuration {
	s := strings.TrimSpace(raw)

	var out CurationOutput
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		if frag := extractJSONFence(s); frag != "" {
			if json.Unmarshal([]byte(frag), &out) == nil {
				return finishCuration(out)
			}
		}
		// Could not extract structure — preserve the human-readable text.
		return parsedCuration{Content: s, Status: structParseFailed}
	}
	return finishCuration(out)
}

// finishCuration classifies a successfully parsed output as ok or empty.
func finishCuration(out CurationOutput) parsedCuration {
	status := structOK
	if out.Structured.isEmpty() {
		status = structEmpty
	}
	return parsedCuration{
		Content:    out.ContentMarkdown,
		DocContext: out.DocContext,
		Structured: out.Structured,
		Status:     status,
	}
}

// extractJSONFence pulls the body of the first ```json ... ``` (or bare ``` ... ```)
// fenced block from a string, or returns "" when there is none.
func extractJSONFence(s string) string {
	start := strings.Index(s, "```")
	if start < 0 {
		return ""
	}
	rest := s[start+3:]
	// Skip an optional language tag (e.g. "json") up to the first newline.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		if tag := strings.TrimSpace(rest[:nl]); tag == "" || !strings.Contains(tag, "{") {
			rest = rest[nl+1:]
		}
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// truncate caps a string to limit runes for logging, appending an ellipsis when cut.
func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// parseRetryAfter reads a Retry-After header in delta-seconds form. Returns 0 when
// absent/unparseable, so the caller falls back to exponential backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

// ---------------------------------------------------------------------------
// Engine factory (CURATE_ENGINE)
// ---------------------------------------------------------------------------

// NewCurator selects the LLM engine by config, returning the curator and its
// combined "engine/model" display name (stored in the engine column and hashed into
// the recipe). This is the only place that knows about concrete engines.
func NewCurator(cfg Config) (Curator, string, error) {
	switch cfg.Engine {
	case "", engineGemini:
		if cfg.GeminiAPIKey == "" {
			return nil, "", fmt.Errorf("GEMINI_API_KEY is required for engine %q", engineGemini)
		}
		model := orDefault(cfg.GeminiModel, defaultGeminiModel)
		return newGeminiCurator(cfg.GeminiAPIKey, model), engineGemini + "/" + model, nil
	case engineClaude:
		if cfg.AnthropicAPIKey == "" {
			return nil, "", fmt.Errorf("ANTHROPIC_API_KEY is required for engine %q", engineClaude)
		}
		model := orDefault(cfg.ClaudeModel, defaultClaudeModel)
		return newClaudeCurator(cfg.AnthropicAPIKey, model), engineClaude + "/" + model, nil
	case engineGroq:
		if cfg.GroqAPIKey == "" {
			return nil, "", fmt.Errorf("GROQ_API_KEY is required for engine %q", engineGroq)
		}
		model := orDefault(cfg.GroqModel, defaultGroqModel)
		return newGroqCurator(cfg.GroqAPIKey, model), engineGroq + "/" + model, nil
	case engineLiteLLM:
		if cfg.LiteLLMBaseURL == "" {
			return nil, "", fmt.Errorf("LITELLM_BASE_URL is required for engine %q", engineLiteLLM)
		}
		model := orDefault(cfg.LiteLLMModel, defaultLiteLLMModel)
		return newLiteLLMCurator(cfg.LiteLLMBaseURL, cfg.LiteLLMAPIKey, model), engineLiteLLM + "/" + model, nil
	default:
		return nil, "", fmt.Errorf("unknown CURATE_ENGINE %q (use %q, %q, %q or %q)",
			cfg.Engine, engineGemini, engineClaude, engineGroq, engineLiteLLM)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------
// Orchestration (engine/database-agnostic; unit-tested via mocks)
// ---------------------------------------------------------------------------

// distillDoc runs the full curation for one source: it walks the pattern chain
// (each stage sees the previous stage's output) and assembles the final
// Distillation. It never returns an error — failures are captured as a "failed"
// Distillation so the batch can persist them and carry on.
func distillDoc(ctx context.Context, cur Curator, engineName string, r Recipe, doc SourceDoc) Distillation {
	d := Distillation{
		YoutubeVideoID:   doc.YoutubeVideoID,
		SourceType:       doc.SourceType,
		SourceRef:        doc.SourceRef,
		SourceKey:        doc.SourceKey,
		Title:            doc.Title,
		Engine:           engineName,
		Context:          r.ContextName,
		Strategy:         r.StrategyName,
		SessionPatterns:  r.sessionPatterns(),
		Pattern:          r.Patterns[len(r.Patterns)-1], // final stage
		SourceSHA256:     doc.SourceSHA256,
		RecipeSHA256:     r.RecipeSHA,
		Status:           statusDone,
		Structured:       []byte("{}"),
		StructuredStatus: structEmpty,
	}

	// Prefer the timestamped transcript so the model can fill claims[].ts_start; fall
	// back to the flat text when no segments are available.
	base := doc.Transcript
	if doc.TranscriptTimestamped != "" {
		base = doc.TranscriptTimestamped
	}

	var prev parsedCuration
	for i, p := range r.Patterns {
		input := base
		if i > 0 {
			input = base + "\n\n---\nPrevious stage output:\n" + prev.Content
		}
		raw, err := cur.Curate(ctx, r.buildSystemPrompt(p), input)
		if err != nil {
			d.Status, d.Error = statusFailed, err.Error()
			return d
		}
		prev = parseCuration(raw)
	}

	d.Content = prev.Content
	d.DocContext = prev.DocContext
	d.StructuredStatus = prev.Status
	if prev.Status != structParseFailed {
		if b, err := json.Marshal(prev.Structured); err == nil {
			d.Structured = b
		}
	}
	return d
}

// distillHandler is the domain logic behind addon.Run: distill ONE claimed item. The SDK owns the
// claim/heartbeat/result/requeue/poke around it; this only does the work.
//
//  1. resolve the recipe for the item's destilar step (flow_steps.options.recipe, else the default);
//  2. load the item's to-text artifact (the upstream transcripts row) by SourceRef;
//  3. run the curation (distillDoc, unchanged);
//  4. persist the distillation (also when it failed — for observability) and report the OutputRef.
//
// A missing input is transient (addon.ErrRetryable): the upstream to-text step may not have landed
// yet, so the SDK requeues up to the cap rather than failing the item for good. A curation failure
// is terminal: the failed row is recorded, and the step is failed.
func distillHandler(store DistillStore, cur Curator, engineName string, rr *recipeResolver) addon.Handler {
	return func(ctx context.Context, item addon.Item, step addon.Step) (addon.Result, error) {
		optsRaw, err := store.RecipeOptions(ctx, item.FlowID, step.Seq)
		if err != nil {
			return addon.Result{}, fmt.Errorf("destilar %s: recipe options: %w", item.SourceRef, err)
		}
		recipe, err := rr.resolve(optsRaw)
		if err != nil {
			return addon.Result{}, fmt.Errorf("destilar %s: recipe: %w", item.SourceRef, err)
		}

		doc, found, err := store.LoadSourceDoc(ctx, item.SourceRef)
		if err != nil {
			return addon.Result{}, fmt.Errorf("destilar %s: load input: %w", item.SourceRef, err)
		}
		if !found {
			return addon.Result{}, fmt.Errorf("destilar %s: input transcript not ready: %w", item.SourceRef, addon.ErrRetryable)
		}

		dctx, cancel := context.WithTimeout(ctx, curateTimeout)
		d := distillDoc(dctx, cur, engineName, recipe, doc)
		cancel()

		// Each attempt gets its own full saveTimeout budget — the retry must not inherit the
		// first attempt's already-consumed deadline.
		saveOnce := func(dist Distillation) (int, error) {
			sctx, cancel := context.WithTimeout(ctx, saveTimeout)
			defer cancel()
			return store.SaveDistillation(sctx, dist)
		}
		id, saveErr := saveOnce(d)
		if isInvalidJSONError(saveErr) {
			// The structured payload was JSON-valid to Go (json.Valid passed in sanitizeStructured)
			// but the jsonb column refused it — an escape PG cannot store (e.g. a NUL escape from a
			// transcript). Persist the distillation without the toxic extraction rather than
			// terminally failing the item. Log only size + a digest, never the raw bytes — they are
			// transcript-derived and may be sensitive — so occurrences stay diagnosable/dedupable.
			// Log only the SQLSTATE, never the error message — a PgError can echo the rejected
			// value (transcript-derived, possibly sensitive) in its Message/Detail.
			var pgErr *pgconn.PgError
			sqlState := ""
			if errors.As(saveErr, &pgErr) {
				sqlState = pgErr.Code
			}
			sum := sha256.Sum256(d.Structured)
			log.Printf("distill %s: structured rejected by jsonb (SQLSTATE %s); retrying with structured={} (len=%d sha256=%s)",
				doc.SourceKey, sqlState, len(d.Structured), hex.EncodeToString(sum[:8]))
			d.Structured = []byte("{}")
			d.StructuredStatus = structParseFailed
			id, saveErr = saveOnce(d)
		}
		if saveErr != nil {
			return addon.Result{}, fmt.Errorf("destilar %s: save: %w", doc.SourceKey, saveErr)
		}

		if d.Status == statusFailed {
			// Row persisted for observability. Surface as retryable so the SDK requeues up to
			// MaxAttempts (matching the 1.0 retry-to-cap, where a failed distillation was
			// re-selected until attempt_count hit the ceiling). postWithRetry already absorbs
			// transient 429/5xx within a call; this covers a timeout or a sustained outage so a
			// blip does not terminally fail the item on the first miss. A persistent failure still
			// terminates — just after the bounded retries.
			return addon.Result{}, fmt.Errorf("destilar %s: %w: %s", doc.SourceKey, addon.ErrRetryable, d.Error)
		}
		log.Printf("distilled %s (%s, structured=%s) -> distillation %d", doc.SourceKey, doc.Title, d.StructuredStatus, id)
		return addon.Result{OutputRef: strconv.Itoa(id)}, nil
	}
}

// ---------------------------------------------------------------------------
// Shared HTTP helper for the LLM engines (transient 429/5xx retry)
// ---------------------------------------------------------------------------

// postWithRetry sends a request built by build (rebuilt per attempt so the body can
// be replayed) and returns the 200 body, retrying only transient failures (429,
// 5xx) up to the cap.
func postWithRetry(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := curateClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			return body, rerr
		}

		body, _ := io.ReadAll(resp.Body)
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()
		lastErr = fmt.Errorf("LLM API error (status %d): %s", resp.StatusCode, truncate(string(body), 500))

		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !transient || attempt >= maxCurateRetries {
			return nil, lastErr
		}
		wait := retryAfter
		if wait <= 0 {
			wait = curateRetryBase << attempt
		}
		log.Printf("LLM transient error (status %d); retrying in %s (attempt %d/%d)\n",
			resp.StatusCode, wait, attempt+1, maxCurateRetries)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// ---------------------------------------------------------------------------
// Real curator: Gemini (native JSON mode via response_mime_type)
// ---------------------------------------------------------------------------

type geminiCurator struct {
	apiKey   string
	model    string
	endpoint string // overridable for tests
}

func newGeminiCurator(apiKey, model string) *geminiCurator {
	return &geminiCurator{
		apiKey:   apiKey,
		model:    model,
		endpoint: "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent",
	}
}

func (g *geminiCurator) Curate(ctx context.Context, systemPrompt, input string) (string, error) {
	reqBody := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []any{map[string]any{"text": systemPrompt}},
		},
		"contents": []any{map[string]any{
			"parts": []any{map[string]any{"text": input}},
		}},
		"generationConfig": map[string]any{
			"response_mime_type": mimeJSON,
			"response_schema":    curationResponseSchema(),
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	body, err := postWithRetry(ctx, func() (*http.Request, error) {
		// Pass the API key via header, never the query string: a transport error
		// embeds the full URL in its message, which would leak the key into logs.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		req.Header.Set(headerContentType, mimeJSON)
		req.Header.Set("x-goog-api-key", g.apiKey)
		return req, nil
	})
	if err != nil {
		return "", err
	}

	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &gr); err != nil {
		return "", err
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini returned no candidates")
	}
	return gr.Candidates[0].Content.Parts[0].Text, nil
}

// ---------------------------------------------------------------------------
// Real curator: Anthropic Claude (forced tool-use → guaranteed JSON object)
// ---------------------------------------------------------------------------

type claudeCurator struct {
	apiKey   string
	model    string
	endpoint string // overridable for tests
}

func newClaudeCurator(apiKey, model string) *claudeCurator {
	return &claudeCurator{
		apiKey:   apiKey,
		model:    model,
		endpoint: "https://api.anthropic.com/v1/messages",
	}
}

func (c *claudeCurator) Curate(ctx context.Context, systemPrompt, input string) (string, error) {
	const toolName = "emit_curation"
	// A curation carries content_markdown + the full structured extraction; 8192 was
	// tight enough to truncate long docs (a cut tool_use block then fails to parse →
	// parse_failed). Sonnet supports far more, so give the response real headroom.
	const maxTokens = 16384
	reqBody := map[string]any{
		"model":      c.model,
		"max_tokens": maxTokens,
		"system":     systemPrompt,
		"messages": []any{
			map[string]any{"role": "user", "content": input},
		},
		"tools": []any{map[string]any{
			"name":         toolName,
			"description":  "Emit the curated knowledge document.",
			"input_schema": curationResponseSchema(),
		}},
		"tool_choice": map[string]any{"type": "tool", "name": toolName},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	body, err := postWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		req.Header.Set(headerContentType, mimeJSON)
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req, nil
	})
	if err != nil {
		return "", err
	}

	var cr struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", err
	}
	for _, blk := range cr.Content {
		if blk.Type == "tool_use" && len(blk.Input) > 0 {
			return string(blk.Input), nil
		}
	}
	return "", fmt.Errorf("claude returned no tool_use block")
}

// ---------------------------------------------------------------------------
// Real curator: Groq (OpenAI-compatible chat, response_format json_object)
// ---------------------------------------------------------------------------

type groqCurator struct {
	apiKey   string
	model    string
	endpoint string // overridable for tests
}

func newGroqCurator(apiKey, model string) *groqCurator {
	return &groqCurator{
		apiKey:   apiKey,
		model:    model,
		endpoint: "https://api.groq.com/openai/v1/chat/completions",
	}
}

func (g *groqCurator) Curate(ctx context.Context, systemPrompt, input string) (string, error) {
	reqBody := map[string]any{
		"model": g.model,
		"messages": []any{
			map[string]any{"role": "system", "content": systemPrompt},
			map[string]any{"role": "user", "content": input},
		},
		"response_format": map[string]any{"type": "json_object"},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	body, err := postWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		req.Header.Set(headerContentType, mimeJSON)
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
		return req, nil
	})
	if err != nil {
		return "", err
	}

	var gr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &gr); err != nil {
		return "", err
	}
	if len(gr.Choices) == 0 {
		return "", fmt.Errorf("groq returned no choices")
	}
	return gr.Choices[0].Message.Content, nil
}

// ---------------------------------------------------------------------------
// litellm curator — the self-hosted LiteLLM gateway (OpenAI-compatible).
//
// This is the anti-lock-in seam (ARCHITECTURE-2.0.md, "Lock-in posture" #2): instead of
// each engine carrying a hardcoded vendor endpoint, distill speaks ONE OpenAI-compatible
// dialect to a gateway it owns, and the actual model/provider behind it (Claude, Gemini,
// Groq, a local model) is a gateway config alias — no vendor markup, swappable without a
// distill change. Wire-identical to the Groq curator (both are OpenAI chat completions with
// response_format json_object); the only differences are the configurable base URL and an
// OPTIONAL bearer key (a self-hosted gateway may run keyless or behind a master key). The
// curation logic is untouched — this is purely the model-call seam.
// ---------------------------------------------------------------------------

type liteLLMCurator struct {
	apiKey   string // optional gateway master key; the Authorization header is omitted when empty
	model    string // gateway model alias
	endpoint string // full OpenAI chat-completions URL (base + /chat/completions); overridable for tests
}

func newLiteLLMCurator(baseURL, apiKey, model string) *liteLLMCurator {
	return &liteLLMCurator{
		apiKey:   apiKey,
		model:    model,
		endpoint: strings.TrimRight(baseURL, "/") + "/chat/completions",
	}
}

func (c *liteLLMCurator) Curate(ctx context.Context, systemPrompt, input string) (string, error) {
	reqBody := map[string]any{
		"model": c.model,
		"messages": []any{
			map[string]any{"role": "system", "content": systemPrompt},
			map[string]any{"role": "user", "content": input},
		},
		"response_format": map[string]any{"type": "json_object"},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	body, err := postWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		req.Header.Set(headerContentType, mimeJSON)
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return req, nil
	})
	if err != nil {
		return "", err
	}

	var lr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", err
	}
	if len(lr.Choices) == 0 {
		return "", fmt.Errorf("litellm gateway returned no choices")
	}
	return lr.Choices[0].Message.Content, nil
}

// curationResponseSchema is the JSON schema for the curation object, shared by the
// engines that accept a response/tool schema (Gemini, Claude).
func curationResponseSchema() map[string]any {
	strArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content_markdown": map[string]any{"type": "string"},
			"doc_context":      map[string]any{"type": "string"},
			"structured": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"concepts":    strArray,
					"insights":    strArray,
					"references":  strArray,
					"connections": strArray,
					"entities": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string"},
								"type": map[string]any{"type": "string"},
							},
						},
					},
					"claims": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text":     map[string]any{"type": "string"},
								"evidence": map[string]any{"type": "string"},
								"ts_start": map[string]any{"type": "number"},
							},
						},
					},
				},
			},
		},
		"required": []string{"content_markdown", "doc_context", "structured"},
	}
}

// ---------------------------------------------------------------------------
// Real domain database: Neon PostgreSQL via pgxpool
//
// appDB is the DOMAIN store (distillations write + the upstream transcripts/flow_steps reads).
// The CONTRACT store (item_steps/providers/items) is the SDK's addon.NewPgxStore over the same
// pool. A pool (not a single conn) backs both because the SDK heartbeats from a background
// goroutine while the drain loop claims — and *pgxpool.Pool is safe for concurrent use.
// ---------------------------------------------------------------------------

type appDB struct{ pool *pgxpool.Pool }

var _ DistillStore = (*appDB)(nil)

// RecipeOptions reads a flow step's options blob (flow_steps.options) so the recipe can be config
// rather than a hardcoded mode. A missing step row yields (nil, nil) → the worker's env default.
func (db *appDB) RecipeOptions(ctx context.Context, flowID, seq int) (json.RawMessage, error) {
	const q = `SELECT options FROM flow_steps WHERE flow_id = $1 AND seq = $2`
	var raw []byte
	switch err := db.pool.QueryRow(ctx, q, flowID, seq).Scan(&raw); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// LoadSourceDoc reads the to-text artifact to distill for one item, keyed by the spine's
// source_ref (= the row's source_key, COALESCE(youtube_video_id, source_ref) — universal across
// lanes, since the extrair workers also write transcripts keyed on source_ref). The title comes
// from the collector tables and the timestamped transcript from transcript_segments (falling back
// to the flat text). Returns found=false when no done, non-empty transcript exists yet.
func (db *appDB) LoadSourceDoc(ctx context.Context, sourceRef string) (SourceDoc, bool, error) {
	const q = `
		WITH src AS (
			SELECT
				t.id AS transcript_id,
				t.youtube_video_id,
				t.source_type,
				t.source_ref,
				COALESCE(t.youtube_video_id, t.source_ref) AS source_key,
				COALESCE(cv.title, pv.title, '')           AS title,
				t.transcript,
				encode(sha256(convert_to(t.transcript, 'UTF8')), 'hex') AS source_sha256
			FROM transcripts t
			LEFT JOIN LATERAL (
				SELECT title FROM channel_videos
				WHERE youtube_video_id = t.youtube_video_id LIMIT 1
			) cv ON TRUE
			LEFT JOIN LATERAL (
				SELECT title FROM playlist_videos
				WHERE youtube_video_id = t.youtube_video_id LIMIT 1
			) pv ON TRUE
			WHERE t.status = 'done'
			  AND t.transcript IS NOT NULL
			  AND length(btrim(t.transcript)) > 0
			  AND COALESCE(t.youtube_video_id, t.source_ref) = $1
			ORDER BY t.id DESC
			LIMIT 1
		)
		SELECT src.youtube_video_id, src.source_type, src.source_ref, src.source_key,
		       src.title, src.transcript,
		       -- Per-segment "[seconds] text", so the model can anchor claims to a
		       -- timestamp. Falls back to the flat transcript when there are no segments.
		       COALESCE(
		           (SELECT string_agg('[' || floor(s.start_seconds)::int || '] ' || s.text, E'\n' ORDER BY s.seq)
		            FROM transcript_segments s WHERE s.transcript_id = src.transcript_id),
		           src.transcript
		       ) AS transcript_ts,
		       src.source_sha256
		FROM src`

	var doc SourceDoc
	var ytID *string
	switch err := db.pool.QueryRow(ctx, q, sourceRef).Scan(&ytID, &doc.SourceType, &doc.SourceRef,
		&doc.SourceKey, &doc.Title, &doc.Transcript, &doc.TranscriptTimestamped, &doc.SourceSHA256); {
	case errors.Is(err, pgx.ErrNoRows):
		return SourceDoc{}, false, nil
	case err != nil:
		return SourceDoc{}, false, err
	}
	if ytID != nil {
		doc.YoutubeVideoID = *ytID
	}
	return doc, true, nil
}

// isInvalidJSONError reports whether err is Postgres rejecting a jsonb value that Go considered
// valid JSON: SQLSTATE 22P02 (invalid_text_representation) or 22P05 (untranslatable_character,
// e.g. a NUL escape PG cannot store as text). `structured` is the only jsonb the save writes,
// so such an error means that column — the handler retries with structured='{}' to persist the
// rest of the distillation. json.Valid in sanitizeStructured catches malformed bytes but not
// these PG-specific refusals, so this is the backstop that keeps a toxic extraction from
// terminally failing the item.
func isInvalidJSONError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "22P02" || pgErr.Code == "22P05"
	}
	return false
}

// sanitizeStructured ensures dist.Structured is valid JSON for the jsonb column.
// Empty bytes → '{}' (status unchanged). Invalid JSON → '{}' + structParseFailed + warning log.
// ponytail: defensive guard against 22P02 when the LLM delivers non-JSON structured bytes.
func sanitizeStructured(dist *Distillation) {
	if len(dist.Structured) == 0 {
		dist.Structured = []byte("{}")
		return
	}
	if !json.Valid(dist.Structured) {
		log.Printf("warn: source_key=%s structured is not valid JSON; saving {} (bytes=%d)",
			dist.SourceKey, len(dist.Structured))
		dist.Structured = []byte("{}")
		dist.StructuredStatus = structParseFailed
	}
}

// SaveDistillation upserts the distillation and returns its id (the OutputRef recorded on the
// step). Idempotent on (source_key, COALESCE(session_patterns, pattern)): a re-run replaces the
// row, incrementing attempt_count on consecutive failures and resetting it on success.
func (db *appDB) SaveDistillation(ctx context.Context, dist Distillation) (int, error) {
	sanitizeStructured(&dist)
	initialAttempt := 0
	if dist.Status == statusFailed {
		initialAttempt = 1
	}
	const upsert = `
		INSERT INTO distillations
			(youtube_video_id, source_type, source_ref, source_key, pattern, context,
			 strategy, session_patterns, engine, title, content, structured,
			 structured_status, doc_context, source_sha256, recipe_sha256, status, error,
			 attempt_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (source_key, COALESCE(session_patterns, pattern)) DO UPDATE SET
			youtube_video_id  = EXCLUDED.youtube_video_id,
			source_type       = EXCLUDED.source_type,
			source_ref        = EXCLUDED.source_ref,
			pattern           = EXCLUDED.pattern,
			context           = EXCLUDED.context,
			strategy          = EXCLUDED.strategy,
			session_patterns  = EXCLUDED.session_patterns,
			engine            = EXCLUDED.engine,
			title             = EXCLUDED.title,
			content           = EXCLUDED.content,
			structured        = EXCLUDED.structured,
			structured_status = EXCLUDED.structured_status,
			doc_context       = EXCLUDED.doc_context,
			source_sha256     = EXCLUDED.source_sha256,
			recipe_sha256     = EXCLUDED.recipe_sha256,
			status            = EXCLUDED.status,
			error             = EXCLUDED.error,
			attempt_count     = CASE WHEN EXCLUDED.status = 'failed'
			                         THEN distillations.attempt_count + 1
			                         ELSE 0 END,
			updated_at        = CURRENT_TIMESTAMP
		RETURNING id`
	var id int
	err := db.pool.QueryRow(ctx, upsert,
		nullStr(dist.YoutubeVideoID),
		dist.SourceType,
		dist.SourceRef,
		dist.SourceKey,
		dist.Pattern,
		nullStr(dist.Context),
		nullStr(dist.Strategy),
		nullStr(dist.SessionPatterns),
		dist.Engine,
		nullStr(dist.Title),
		nullStr(dist.Content),
		dist.Structured,
		dist.StructuredStatus,
		nullStr(dist.DocContext),
		dist.SourceSHA256,
		dist.RecipeSHA256,
		dist.Status,
		nullStr(dist.Error),
		initialAttempt,
	).Scan(&id)
	return id, err
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// Config & entrypoint
// ---------------------------------------------------------------------------

func loadConfig() Config {
	return Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Engine:          os.Getenv("CURATE_ENGINE"),
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		GroqAPIKey:      os.Getenv("GROQ_API_KEY"),
		GeminiModel:     os.Getenv("GEMINI_MODEL"),
		ClaudeModel:     os.Getenv("CLAUDE_MODEL"),
		GroqModel:       os.Getenv("GROQ_MODEL"),
		LiteLLMBaseURL:  os.Getenv("LITELLM_BASE_URL"),
		LiteLLMAPIKey:   os.Getenv("LITELLM_API_KEY"),
		LiteLLMModel:    os.Getenv("LITELLM_MODEL"),
		Patterns:        os.Getenv("DISTILL_PATTERNS"),
		ContextName:     os.Getenv("DISTILL_CONTEXT"),
		StrategyName:    os.Getenv("DISTILL_STRATEGY"),
	}
}

// main wires the bridge-total claim-worker: the SDK (addon.Run) owns the queue protocol; this
// buildDistillPoolConfig parses the DSN and forces simple protocol so pgx never caches
// prepared statements — required when DATABASE_URL points to a PgBouncer pooler endpoint.
func buildDistillPoolConfig(dbURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return cfg, nil
}

// process only supplies the destilar domain (distillHandler). It serves ONE provider
// (DISTILL_PROVIDER, e.g. distill | distill-vpc) so it claims only the steps the reconciler
// routed to it. Default is on_demand (drain once and exit, the woken Cloud Run job); a resident
// deploy opts into the long-running loop + symmetric activation via WORK_POLL_INTERVAL and/or
// POKE_ADDR + POKE_TOKEN.
func main() {
	cfg := loadConfig()
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}
	provider := os.Getenv("DISTILL_PROVIDER")
	if provider == "" {
		log.Fatalf("DISTILL_PROVIDER is required (the provider this worker serves, e.g. distill | distill-vpc)")
	}

	cur, engineName, err := NewCurator(cfg)
	if err != nil {
		log.Fatalf("Curator init failed: %v", err)
	}

	// The recipe normally comes from the flow step config; these are the fallback default.
	rr := newRecipeResolver(splitCSV(cfg.Patterns), cfg.ContextName, cfg.StrategyName)
	if _, err := rr.resolve(nil); err != nil {
		log.Fatalf("default recipe init failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := buildDistillPoolConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to parse DATABASE_URL") // don't echo err — may contain DSN credentials
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()
	log.Printf("rara-distill worker %s/%s ready [engine %s]", capDestilar, provider, engineName)

	ac := addon.Config{
		Capability:   capDestilar,
		Provider:     provider,
		Store:        addon.NewPgxStore(pool),
		MaxAttempts:  addon.DefaultMaxAttempts,
		PollInterval: addon.EnvDuration("WORK_POLL_INTERVAL", 0),
		PokeAddr:     os.Getenv("POKE_ADDR"),
		PokeToken:    os.Getenv("POKE_TOKEN"),
	}
	if err := addon.Run(ctx, ac, distillHandler(&appDB{pool: pool}, cur, engineName, rr)); err != nil {
		log.Fatalf("distill worker %s/%s: %v", capDestilar, provider, err)
	}
	log.Printf("rara-distill worker %s/%s: queue drained", capDestilar, provider)
}
