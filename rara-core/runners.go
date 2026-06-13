// runners.go — the I/O edge of the worker shims (Phase 1 deliverable #5).
//
// These concrete StepRunners are the thin adapters that actually invoke the existing
// scribe/distill binaries and read back the domain row they wrote. They are deliberately
// minimal glue: exec + one SELECT. Like the pgx writes in main.go, they are exercised by
// real deploys/integration, not unit tests — the claim/advance orchestration in worker.go
// is what the pure tests cover (via a fake StepRunner).
//
// Binary paths and engines are environment-configured so a deploy points the shim at the
// right artifact (SCRIBE_BIN on the Mac, DISTILL_BIN in the Cloud Run image) without code
// changes. None of this touches scribe/distill domain logic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// errNoOutputRow is a HARD failure: the worker ran but produced no usable domain row
// (e.g. scribe could not transcribe at all). errRetryable is a TRANSIENT miss: the row is
// expected to appear on a later attempt (e.g. distill's batch hasn't reached it yet), so
// the worker re-queues the step instead of failing the item. worker.go branches on these.
var (
	errNoOutputRow = errors.New("worker produced no output row")
	errRetryable   = errors.New("retryable: output not yet available")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// scribe shim (transcrever) — per-item entry: `--source <watch-url> --limit 1`.
// ---------------------------------------------------------------------------

type scribeRunner struct {
	conn *pgx.Conn
	bin  string // SCRIBE_BIN
}

func newScribeRunner(conn *pgx.Conn) *scribeRunner {
	return &scribeRunner{conn: conn, bin: envOr("SCRIBE_BIN", "scribe-job")}
}

func (r *scribeRunner) Run(ctx context.Context, item Item, _ ItemStep) (RunResult, error) {
	// Translate the spine's natural key into scribe's current single-source entrypoint.
	url := "https://www.youtube.com/watch?v=" + item.SourceRef
	cmd := exec.CommandContext(ctx, r.bin, "--source", url, "--limit", "1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return RunResult{}, fmt.Errorf("scribe %s: %w", url, err)
	}
	// Capture the transcript scribe wrote. 'empty' (no speech) is a benign no-content
	// outcome: the row exists but there is nothing to distill, so the item is filtered
	// rather than driven into a distill that must fail. No row at all is a hard failure.
	const q = `SELECT id, status FROM transcripts
	           WHERE youtube_video_id = $1 AND status IN ('done', 'empty')`
	var id int
	var status string
	switch err := r.conn.QueryRow(ctx, q, item.SourceRef).Scan(&id, &status); {
	case errors.Is(err, pgx.ErrNoRows):
		return RunResult{}, fmt.Errorf("transcrever %s: %w", item.SourceRef, errNoOutputRow)
	case err != nil:
		return RunResult{}, err
	}
	return RunResult{OutputRef: strconv.Itoa(id), Filtered: status == "empty"}, nil
}

// ---------------------------------------------------------------------------
// distill shim (destilar) — no per-item entry; trigger an idempotent batch drain.
// ---------------------------------------------------------------------------

type distillRunner struct {
	conn      *pgx.Conn
	bin       string // DISTILL_BIN
	batchSize string // forced high so one run drains the pending queue incl. this item
}

func newDistillRunner(conn *pgx.Conn) *distillRunner {
	return &distillRunner{
		conn:      conn,
		bin:       envOr("DISTILL_BIN", "etl-job"),
		batchSize: envOr("DISTILL_DRAIN_BATCH", "100"),
	}
}

func (r *distillRunner) Run(ctx context.Context, item Item, _ ItemStep) (RunResult, error) {
	// distill batch-pulls its own queue; force a large batch so this transcript is
	// included in the single run. Idempotent — re-distilling already-done rows is a no-op.
	cmd := exec.CommandContext(ctx, r.bin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DISTILL_BATCH_SIZE="+r.batchSize)
	if err := cmd.Run(); err != nil {
		return RunResult{}, fmt.Errorf("distill: %w", err)
	}
	// For a YouTube source, distillations.source_key is the youtube_video_id. A missing
	// row is TRANSIENT, not fatal: with a large backlog one drain may not have reached
	// this transcript yet, so re-queue (capped) rather than failing the item.
	const q = `SELECT id FROM distillations WHERE source_key = $1 AND status = 'done'`
	var id int
	switch err := r.conn.QueryRow(ctx, q, item.SourceRef).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return RunResult{}, fmt.Errorf("destilar %s: %w", item.SourceRef, errRetryable)
	case err != nil:
		return RunResult{}, err
	}
	return RunResult{OutputRef: strconv.Itoa(id)}, nil
}

// ---------------------------------------------------------------------------
// gate shim (gate_barato / gate_rico) — the I/O edge of the curation cascade.
//
// gate_barato judges METADATA (title + channel) before paying for ASR; gate_rico judges the
// full TEXT before paying for distillation. This runner loads the live interest_profile +
// rules (rara-core's own tables), reads the item's metadata/text (the worker domain tables,
// SELECT only — no FK, the 1.0 isolation convention holds), runs the PURE cascade (gates.go),
// and returns its verdict. The decision recording + routing happen in worker.go / the
// reconciler. Like the scribe/distill shims, this glue is exercised by integration, not unit
// tests — runCascade is what the pure tests cover (with a fake judge).
// ---------------------------------------------------------------------------

type gateRunner struct {
	db    Database  // the live profile + rules (rara-core tables)
	conn  *pgx.Conn // worker-domain metadata/text reads
	gate  string    // capGateBarato | capGateRico
	judge LLMJudge  // the borderline decider (LiteLLM)
}

func newGateRunner(db Database, conn *pgx.Conn, gate string, judge LLMJudge) *gateRunner {
	return &gateRunner{db: db, conn: conn, gate: gate, judge: judge}
}

func (r *gateRunner) Run(ctx context.Context, item Item, _ ItemStep) (RunResult, error) {
	prof, err := r.loadProfile(ctx)
	if err != nil {
		return RunResult{}, err
	}
	in, err := r.readInput(ctx, item)
	if err != nil {
		return RunResult{}, err
	}
	verdict, err := runCascade(ctx, r.gate, in, prof, r.judge)
	if err != nil {
		// Only the LLM-judge layer can error (rules + the profile match are pure); a judge
		// failure is treated as TRANSIENT — a gateway blip must not permanently fail a good
		// item. The worker re-queues it (errRetryable) up to the attempt ceiling, after which
		// it fails for good with the error recorded — mirroring distill's miss handling.
		return RunResult{}, fmt.Errorf("%s cascade %s: %v: %w", r.gate, item.SourceRef, err, errRetryable)
	}
	return RunResult{Gate: &verdict}, nil
}

// loadProfile reads the live interest_profile + enabled rules and parses them for the
// cascade. A not-yet-seeded profile is not fatal — the cascade still runs on rules + the LLM
// (the profile layer just contributes nothing).
func (r *gateRunner) loadProfile(ctx context.Context) (profileDoc, error) {
	rules, err := r.db.ListGateRules(ctx)
	if err != nil {
		return profileDoc{}, err
	}
	prof, found, err := r.db.GetLatestInterestProfile(ctx)
	if err != nil {
		return profileDoc{}, err
	}
	if !found {
		return parseProfile(InterestProfile{}, rules), nil
	}
	return parseProfile(prof, rules), nil
}

// readInput gathers what the gate judges: metadata for both gates, plus the full transcript
// for gate_rico.
func (r *gateRunner) readInput(ctx context.Context, item Item) (gateInput, error) {
	title, channel, err := r.readMetadata(ctx, item.SourceRef)
	if err != nil {
		return gateInput{}, err
	}
	in := gateInput{Title: title, Channel: channel}
	if r.gate == capGateRico {
		text, err := r.readText(ctx, item.SourceRef)
		if err != nil {
			return gateInput{}, err
		}
		in.Text = text
	}
	return in, nil
}

// readMetadata fetches the video's title and (best-effort) channel name. It prefers the
// harvested channel_videos row (which carries the channel via target_channels) and falls back
// to a shelved playlist_videos row (title only). A missing row yields empty strings rather
// than an error — the cascade then leans on the LLM, which will tend to defer on no signal.
func (r *gateRunner) readMetadata(ctx context.Context, videoID string) (title, channel string, err error) {
	const q = `
		SELECT title, channel FROM (
			SELECT cv.title AS title, tc.channel_name AS channel, 1 AS pri
			FROM channel_videos cv JOIN target_channels tc ON tc.id = cv.channel_id
			WHERE cv.youtube_video_id = $1
			UNION ALL
			SELECT pv.title, '' AS channel, 2 AS pri
			FROM playlist_videos pv WHERE pv.youtube_video_id = $1
		) m ORDER BY pri LIMIT 1`
	switch err := r.conn.QueryRow(ctx, q, videoID).Scan(&title, &channel); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", nil
	case err != nil:
		return "", "", err
	}
	return title, channel, nil
}

// readText fetches the completed transcript for gate_rico. A missing transcript at this
// point is a hard failure (gate_rico runs only after transcrever completed).
func (r *gateRunner) readText(ctx context.Context, videoID string) (string, error) {
	const q = `SELECT COALESCE(transcript, '') FROM transcripts WHERE youtube_video_id = $1 AND status = 'done'`
	var text string
	switch err := r.conn.QueryRow(ctx, q, videoID).Scan(&text); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("gate_rico %s: %w", videoID, errNoOutputRow)
	case err != nil:
		return "", err
	}
	return text, nil
}

// ---------------------------------------------------------------------------
// LLM-judge — the borderline decider, via the self-hosted LiteLLM gateway.
//
// The anti-lock-in seam (ARCHITECTURE-2.0 "Lock-in posture" #2): the gate speaks ONE
// OpenAI-compatible dialect to a gateway it owns; the model behind it (Claude/Gemini/Groq/
// local) is a gateway alias, swappable without a rara-core change. Wire-identical to distill's
// liteLLMCurator. It is consulted ONLY on the borderline middle (runCascade), so the cost is
// bounded to what rules + the profile could not decide.
// ---------------------------------------------------------------------------

type liteLLMJudge struct {
	apiKey   string // optional gateway master key; Authorization omitted when empty
	model    string // gateway model alias
	endpoint string // base + /chat/completions
	client   *http.Client
}

// newLiteLLMJudge builds the judge from the environment (LITELLM_BASE_URL/_API_KEY/_MODEL),
// erroring if the base URL is unset — a gate worker cannot judge without the gateway.
func newLiteLLMJudge() (*liteLLMJudge, error) {
	base := os.Getenv("LITELLM_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("LITELLM_BASE_URL is required for the gate LLM-judge")
	}
	return &liteLLMJudge{
		apiKey:   os.Getenv("LITELLM_API_KEY"),
		model:    envOr("LITELLM_MODEL", "claude-sonnet-4-6"),
		endpoint: strings.TrimRight(base, "/") + "/chat/completions",
		client:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (j *liteLLMJudge) Judge(ctx context.Context, gate string, in gateInput, prof profileDoc) (GateVerdict, error) {
	reqBody := map[string]any{
		"model": j.model,
		"messages": []any{
			map[string]any{"role": "system", "content": judgeSystemPrompt(gate, prof)},
			map[string]any{"role": "user", "content": judgeUserPrompt(gate, in)},
		},
		"response_format": map[string]any{"type": "json_object"},
		"temperature":     0,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return GateVerdict{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return GateVerdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if j.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+j.apiKey)
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return GateVerdict{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return GateVerdict{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return GateVerdict{}, fmt.Errorf("litellm judge: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var lr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return GateVerdict{}, err
	}
	if len(lr.Choices) == 0 {
		return GateVerdict{}, fmt.Errorf("litellm judge returned no choices")
	}
	return parseJudgeVerdict(lr.Choices[0].Message.Content)
}

// parseJudgeVerdict turns the model's JSON object into a verdict. An unrecognized or missing
// decision FAILS SAFE to defer (quarantine for human review) rather than a blind keep/drop —
// uncertainty must never silently drop content nor wave it through.
func parseJudgeVerdict(content string) (GateVerdict, error) {
	var jr struct {
		Decision string   `json:"decision"`
		Score    *float64 `json:"score"`
		Reason   string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(content), &jr); err != nil {
		return GateVerdict{}, fmt.Errorf("litellm judge: bad JSON %q: %w", content, err)
	}
	decision := strings.ToLower(strings.TrimSpace(jr.Decision))
	if !isValidDecision(decision) {
		decision = decisionDefer
	}
	score := jr.Score
	if score != nil && (*score < 0 || *score > 1) {
		score = nil // out-of-range confidence: drop it rather than violate the [0,1] CHECK
	}
	return GateVerdict{
		Decision: decision, Score: score, DecidedBy: decidedByLLM,
		Reason: strings.TrimSpace(jr.Reason),
	}, nil
}

// judgeSystemPrompt frames the curation task and injects the interest_profile as context.
func judgeSystemPrompt(gate string, prof profileDoc) string {
	var b strings.Builder
	b.WriteString("You are a curation gate for a personal knowledge pipeline. Decide whether to KEEP, DROP, or DEFER an item, given the user's interests.\n\n")
	b.WriteString("User interest profile:\n")
	b.WriteString("- Topics: " + joinOrNone(prof.Topics) + "\n")
	b.WriteString("- Authors/channels: " + joinOrNone(prof.Authors) + "\n")
	b.WriteString("- Anti-topics (avoid): " + joinOrNone(prof.AntiTopics) + "\n\n")
	if gate == capGateBarato {
		b.WriteString("You see only the item's metadata (title, channel).\n\n")
	} else {
		b.WriteString("You see the item's full transcript text (plus its title/channel).\n\n")
	}
	b.WriteString("Decide:\n")
	b.WriteString("- keep: clearly relevant to the topics/authors.\n")
	b.WriteString("- drop: clearly irrelevant, or an anti-topic.\n")
	b.WriteString("- defer: genuinely uncertain. Deferred items go to a human review queue, so prefer defer over a low-confidence keep or drop.\n\n")
	b.WriteString(`Respond ONLY as a JSON object: {"decision":"keep|drop|defer","score":0.0-1.0,"reason":"one short sentence"}. score is your confidence in [0,1].`)
	return b.String()
}

// maxJudgeTextChars caps how much transcript the gate_rico prompt carries. Relevance is
// decidable from a generous prefix; sending a multi-hour transcript whole would risk the
// model's context window and inflate cost for no curation benefit. The cheap profile-match
// layer still scans the full text (it is free) — this cap is only the LLM prompt.
const maxJudgeTextChars = 12000

// judgeUserPrompt is the item under judgement.
func judgeUserPrompt(gate string, in gateInput) string {
	var b strings.Builder
	b.WriteString("Title: " + in.Title + "\n")
	b.WriteString("Channel: " + in.Channel + "\n")
	if gate == capGateRico {
		text := in.Text
		if len(text) > maxJudgeTextChars {
			text = text[:maxJudgeTextChars] + "\n…[truncated]"
		}
		b.WriteString("\nTranscript:\n" + text + "\n")
	}
	return b.String()
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

// ---------------------------------------------------------------------------
// pgx SpineSource — the read side of ingest (channel_videos ∪ playlist_videos).
// ---------------------------------------------------------------------------

type pgxSpineSource struct{ conn *pgx.Conn }

// YouTubeVideos returns the deduped union of harvested channel videos and shelved playlist
// videos. A video present in both (or in many playlists) collapses to one row — the spine
// is globally keyed on youtube_video_id.
func (s *pgxSpineSource) YouTubeVideos(ctx context.Context) ([]YouTubeVideo, error) {
	const q = `
		SELECT youtube_video_id, MAX(title) AS title
		FROM (
			SELECT youtube_video_id, title FROM channel_videos
			UNION ALL
			SELECT youtube_video_id, title FROM playlist_videos
		) v
		WHERE youtube_video_id IS NOT NULL AND youtube_video_id <> ''
		GROUP BY youtube_video_id`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YouTubeVideo
	for rows.Next() {
		var v YouTubeVideo
		if err := rows.Scan(&v.VideoID, &v.Title); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
