package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Engine identifiers (TRANSCRIBE_ENGINE) and their display names (stored per row).
const (
	engineGroq   = "groq"
	engineGemini = "gemini"

	groqModelName   = "groq/whisper-large-v3"
	geminiModelName = "gemini/gemini-2.5-flash"
)

const (
	// chunkSeconds is the ffmpeg segment length. Each 10-minute chunk of 16 kHz
	// mono audio stays well under Groq's 25 MB upload limit, so we never need
	// GCS/URL uploads. It is also the per-chunk global timestamp offset step.
	chunkSeconds = 600

	defaultBatchSize = 25

	// sourceTimeout bounds the work on a single video (download + transcribe), so
	// a stuck yt-dlp/ffmpeg or a hung upload cannot block the whole batch.
	sourceTimeout = 20 * time.Minute

	// saveTimeout bounds the per-video database write, so a hung DB connection
	// cannot stall the whole batch on its own (the batch ctx is unbounded).
	saveTimeout = 30 * time.Second

	// maxFailedAttempts caps how many times a failing video is retried. Past this,
	// PendingVideos skips it so permanently-broken videos (deleted/private) stop
	// burning yt-dlp/ASR calls every run. A 'done' save resets the counter.
	maxFailedAttempts = 5

	// maxASRRetries bounds how many times a transient ASR error (429 rate limit or
	// 5xx) is retried within a single chunk before giving up. Groq's free tier is
	// 20 RPM, so large runs hit 429 routinely — these are transient and must not
	// fail an otherwise-fine video.
	maxASRRetries = 4
)

// asrRetryBase is the base backoff for transient ASR retries (doubles each
// attempt) when the response carries no Retry-After header. It is a var so tests
// can shrink it; production keeps the 2s default.
var asrRetryBase = 2 * time.Second

// transcribeClient is used for the (slower) ASR calls — uploading audio and
// waiting for transcription takes longer than the metadata calls in the other
// agents, so it gets a generous timeout of its own.
var transcribeClient = &http.Client{Timeout: 180 * time.Second}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Source is something to transcribe: a YouTube video, an arbitrary video URL,
// or a local file.
type Source struct {
	Type string // "youtube" | "url" | "local"
	Ref  string // watch url, page url, or file path
}

// AudioChunk is a single decoded audio segment on disk, plus its global start
// offset within the original media (seconds).
type AudioChunk struct {
	Path   string
	Offset float64
}

// Segment is one timestamped piece of transcript (global offsets, seconds).
type Segment struct {
	Start float64
	End   float64
	Text  string
}

// Transcript is one row of the transcripts table.
type Transcript struct {
	SourceType      string
	YoutubeVideoID  string
	SourceRef       string
	Language        string
	Engine          string
	Text            string
	DurationSeconds int
	Status          string // "done" | "failed"
	Error           string
}

// VideoRef is a video pending transcription, discovered from the collector tables.
type VideoRef struct {
	YoutubeVideoID string
	URL            string
}

// Config is the runtime configuration, sourced from environment variables.
type Config struct {
	DatabaseURL  string
	Engine       string
	GroqAPIKey   string
	GeminiAPIKey string
	Cookies      string
	BatchSize    int
}

// ---------------------------------------------------------------------------
// Interfaces (the seams that make the pipeline unit-testable with zero I/O)
// ---------------------------------------------------------------------------

// Database is the persistence seam. The real implementation talks to Neon; the
// tests use an in-memory mock that mirrors the SQL uniqueness constraints.
type Database interface {
	PendingVideos(ctx context.Context, limit int) ([]VideoRef, error)
	SaveTranscript(ctx context.Context, t Transcript, segs []Segment) error
}

// AudioAcquirer turns a Source into one or more decoded audio chunks on disk.
type AudioAcquirer interface {
	Acquire(ctx context.Context, src Source) ([]AudioChunk, error)
}

// Transcriber is the ASR engine seam — the single point where Groq, Gemini (or
// any future engine) are swapped, selected by NewTranscriber.
type Transcriber interface {
	Transcribe(ctx context.Context, audioPath string) (text, language string, segs []Segment, err error)
}

// ---------------------------------------------------------------------------
// Pure helpers (directly unit-tested)
// ---------------------------------------------------------------------------

var videoIDRe = regexp.MustCompile(`(?:youtu\.be/|/shorts/|/embed/|/live/|/v/|[?&]v=)([A-Za-z0-9_-]{11})`)

// detectSourceType classifies a raw reference into a Source type.
func detectSourceType(ref string) string {
	if u, err := url.Parse(ref); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		host := strings.ToLower(u.Host)
		if strings.Contains(host, "youtube.") || strings.Contains(host, "youtu.be") {
			return "youtube"
		}
		return "url"
	}
	return "local"
}

// extractVideoID pulls the 11-char YouTube id from any common URL shape, or
// accepts a bare id. Returns "" when none is found.
func extractVideoID(s string) string {
	if m := videoIDRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	if len(s) == 11 && !strings.ContainsAny(s, "/:?&") {
		return s
	}
	return ""
}

// videoURL builds the canonical watch URL for a video id.
func videoURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

// reindexSegments shifts a chunk's local segment timestamps onto the global
// timeline by adding the chunk's start offset.
func reindexSegments(segs []Segment, offset float64) []Segment {
	out := make([]Segment, len(segs))
	for i, s := range segs {
		out[i] = Segment{Start: s.Start + offset, End: s.End + offset, Text: s.Text}
	}
	return out
}

// durationFromSegments derives a duration (seconds) from the latest segment end.
func durationFromSegments(segs []Segment) int {
	var maxEnd float64
	for _, s := range segs {
		if s.End > maxEnd {
			maxEnd = s.End
		}
	}
	return int(math.Round(maxEnd))
}

// ---------------------------------------------------------------------------
// Engine factory (TRANSCRIBE_ENGINE)
// ---------------------------------------------------------------------------

// NewTranscriber selects the ASR engine by config, returning the engine and its
// display name (stored in the engine column). This is the only place that knows
// about concrete engines — the pipeline is engine-agnostic.
func NewTranscriber(cfg Config) (Transcriber, string, error) {
	switch cfg.Engine {
	case "", engineGroq:
		if cfg.GroqAPIKey == "" {
			return nil, "", fmt.Errorf("GROQ_API_KEY is required for engine %q", engineGroq)
		}
		return newGroqTranscriber(cfg.GroqAPIKey), groqModelName, nil
	case engineGemini:
		if cfg.GeminiAPIKey == "" {
			return nil, "", fmt.Errorf("GEMINI_API_KEY is required for engine %q", engineGemini)
		}
		return newGeminiTranscriber(cfg.GeminiAPIKey), geminiModelName, nil
	default:
		return nil, "", fmt.Errorf("unknown TRANSCRIBE_ENGINE %q (use %q or %q)", cfg.Engine, engineGroq, engineGemini)
	}
}

// ---------------------------------------------------------------------------
// Orchestration (engine/acquirer-agnostic; unit-tested via mocks)
// ---------------------------------------------------------------------------

// transcribeSource runs the full pipeline for one source: acquire audio, then
// transcribe every chunk and stitch the result. It never returns an error —
// failures are captured as a "failed" Transcript so the batch can persist them
// and carry on.
func transcribeSource(ctx context.Context, acq AudioAcquirer, tr Transcriber, engineName string, src Source) (Transcript, []Segment) {
	t := Transcript{
		SourceType: src.Type,
		SourceRef:  src.Ref,
		Engine:     engineName,
		Status:     "done",
	}
	if src.Type == "youtube" {
		t.YoutubeVideoID = extractVideoID(src.Ref)
	}

	chunks, err := acq.Acquire(ctx, src)
	if err != nil {
		t.Status, t.Error = "failed", err.Error()
		return t, nil
	}
	defer cleanupChunks(chunks)

	var (
		parts      []string
		segs       []Segment
		langCounts = map[string]int{}
	)
	for _, ch := range chunks {
		text, lang, chunkSegs, err := tr.Transcribe(ctx, ch.Path)
		if err != nil {
			t.Status, t.Error = "failed", err.Error()
			return t, nil
		}
		if lang != "" {
			langCounts[lang]++
		}
		if s := strings.TrimSpace(text); s != "" {
			parts = append(parts, s)
		}
		segs = append(segs, reindexSegments(chunkSegs, ch.Offset)...)
	}

	// Pick the language by majority vote across chunks rather than trusting the
	// first chunk — an intro of music/silence can make the first chunk mis-detect.
	t.Language = majorityLanguage(langCounts)
	t.Text = strings.Join(parts, "\n")
	t.DurationSeconds = durationFromSegments(segs)
	return t, segs
}

// majorityLanguage returns the most frequently detected language across the
// chunks. Ties break deterministically by language code (so the result never
// depends on map iteration order). Returns "" when no chunk reported a language.
func majorityLanguage(counts map[string]int) string {
	best, bestN := "", 0
	for lang, n := range counts {
		if n > bestN || (n == bestN && (best == "" || lang < best)) {
			best, bestN = lang, n
		}
	}
	return best
}

// runBatch transcribes the next batch of pending videos from the collector
// tables. A failure on one video is recorded and the batch continues.
func runBatch(ctx context.Context, db Database, acq AudioAcquirer, tr Transcriber, engineName string, limit int) error {
	refs, err := db.PendingVideos(ctx, limit)
	if err != nil {
		return fmt.Errorf("failed to load pending videos: %w", err)
	}
	if len(refs) == 0 {
		log.Println("No pending videos to transcribe")
		return nil
	}
	log.Printf("Transcribing %d pending video(s) with %s\n", len(refs), engineName)

	done, failed := 0, 0
	for _, ref := range refs {
		src := Source{Type: "youtube", Ref: ref.URL}
		srcCtx, cancel := context.WithTimeout(ctx, sourceTimeout)
		t, segs := transcribeSource(srcCtx, acq, tr, engineName, src)
		cancel()
		if t.YoutubeVideoID == "" {
			t.YoutubeVideoID = ref.YoutubeVideoID // fall back to the known id
		}
		// Bound the DB write on its own timeout — the batch ctx is unbounded, so a
		// hung connection here must not stall the whole run.
		saveCtx, cancelSave := context.WithTimeout(ctx, saveTimeout)
		err := db.SaveTranscript(saveCtx, t, segs)
		cancelSave()
		if err != nil {
			log.Printf("Warning: failed to save transcript for %s: %v\n", t.YoutubeVideoID, err)
			continue
		}
		if t.Status == "failed" {
			failed++
			log.Printf("Failed: %s — %s\n", t.YoutubeVideoID, t.Error)
		} else {
			done++
			log.Printf("Transcribed: %s (%s, %d segments)\n", t.YoutubeVideoID, t.Language, len(segs))
		}
	}

	log.Printf("Batch complete: %d done, %d failed\n", done, failed)
	return nil
}

// cleanupChunks removes the temp working directory holding the audio chunks.
// It only deletes paths under the OS temp dir, so mock paths (and a user's own
// local file) are never touched.
func cleanupChunks(chunks []AudioChunk) {
	if len(chunks) == 0 {
		return
	}
	dir := filepath.Dir(chunks[0].Path)
	if strings.HasPrefix(dir, os.TempDir()) {
		_ = os.RemoveAll(dir)
	}
}

// ---------------------------------------------------------------------------
// Real audio acquirer: yt-dlp (download) + ffmpeg (decode/resample/segment)
// ---------------------------------------------------------------------------

type ytDlpAcquirer struct {
	ytDlp      string // absolute path to the yt-dlp binary
	ffmpeg     string // absolute path to the ffmpeg binary
	cookieFile string // optional path to a cookies.txt file
}

func newYtDlpAcquirer(ytDlp, ffmpeg, cookieFile string) *ytDlpAcquirer {
	return &ytDlpAcquirer{ytDlp: ytDlp, ffmpeg: ffmpeg, cookieFile: cookieFile}
}

func (a *ytDlpAcquirer) Acquire(ctx context.Context, src Source) ([]AudioChunk, error) {
	workDir, err := os.MkdirTemp("", "scribe-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	input := src.Ref
	if src.Type != "local" {
		// Download best native audio format; ffmpeg conversion happens in the
		// next step via our own explicit binary path. Avoids yt-dlp postprocessing
		// which requires ffmpeg in PATH (broken under launchd's minimal environment).
		args := []string{
			"-x",
			"--no-playlist", "--no-progress",
			"--no-write-thumbnail", "--no-write-subs",
			// Force the web player client (full cookie support) and fall back to
			// mweb then android. The ios client uses OAuth tokens instead of
			// browser cookies and silently ignores the --cookies flag.
			"--extractor-args", "youtube:player_client=web,mweb,android",
			"-o", filepath.Join(workDir, "audio.%(ext)s"),
		}
		if a.cookieFile != "" {
			args = append(args, "--cookies", a.cookieFile)
		}
		// "--" ends option parsing: a ref starting with "-" is treated as a URL,
		// not as a yt-dlp flag (argument-injection guard).
		args = append(args, "--", src.Ref)
		if out, err := exec.CommandContext(ctx, a.ytDlp, args...).CombinedOutput(); err != nil {
			_ = os.RemoveAll(workDir)
			return nil, fmt.Errorf("yt-dlp failed: %w: %s", err, lastLine(out))
		}
		// Resolve the actual downloaded filename (extension varies by format).
		matches, err := filepath.Glob(filepath.Join(workDir, "audio.*"))
		if err != nil || len(matches) == 0 {
			_ = os.RemoveAll(workDir)
			return nil, fmt.Errorf("yt-dlp produced no audio file for %s", src.Ref)
		}
		input = matches[0]
	}

	// Decode to 16 kHz mono and split into fixed-length chunks.
	pattern := filepath.Join(workDir, "chunk_%03d.mp3")
	ffargs := []string{
		"-hide_banner", "-loglevel", "error",
		"-i", input,
		"-ar", "16000", "-ac", "1",
		"-f", "segment", "-segment_time", strconv.Itoa(chunkSeconds),
		"-reset_timestamps", "1",
		pattern,
	}
	if out, err := exec.CommandContext(ctx, a.ffmpeg, ffargs...).CombinedOutput(); err != nil {
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("ffmpeg failed: %w: %s", err, lastLine(out))
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "chunk_*.mp3"))
	if err != nil || len(matches) == 0 {
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("no audio chunks produced for %s", src.Ref)
	}
	sort.Strings(matches)

	chunks := make([]AudioChunk, len(matches))
	for i, m := range matches {
		chunks[i] = AudioChunk{Path: m, Offset: float64(i * chunkSeconds)}
	}
	return chunks, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// ---------------------------------------------------------------------------
// Real transcriber: Groq whisper-large-v3 (/audio/transcriptions, verbose_json)
// ---------------------------------------------------------------------------

type groqTranscriber struct {
	apiKey   string
	endpoint string // overridable for tests; defaults to the Groq API
}

func newGroqTranscriber(apiKey string) *groqTranscriber {
	return &groqTranscriber{
		apiKey:   apiKey,
		endpoint: "https://api.groq.com/openai/v1/audio/transcriptions",
	}
}

type verboseJSON struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

func (g *groqTranscriber) Transcribe(ctx context.Context, audioPath string) (string, string, []Segment, error) {
	// Build the multipart body once, in memory (chunks are < 25 MB), so it can be
	// re-sent on each retry attempt without reopening the file.
	reqBody, contentType, err := buildGroqMultipart(audioPath)
	if err != nil {
		return "", "", nil, err
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return "", "", nil, err
		}
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
		req.Header.Set("Content-Type", contentType)

		resp, err := transcribeClient.Do(req)
		if err != nil {
			return "", "", nil, err
		}

		if resp.StatusCode == http.StatusOK {
			var vj verboseJSON
			derr := json.NewDecoder(resp.Body).Decode(&vj)
			resp.Body.Close()
			if derr != nil {
				return "", "", nil, derr
			}
			segs := make([]Segment, len(vj.Segments))
			for i, s := range vj.Segments {
				segs[i] = Segment{Start: s.Start, End: s.End, Text: strings.TrimSpace(s.Text)}
			}
			return vj.Text, vj.Language, segs, nil
		}

		body, _ := io.ReadAll(resp.Body)
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()
		lastErr = fmt.Errorf("groq API error (status %d): %s", resp.StatusCode, string(body))

		// Retry only transient failures (429 rate limit, 5xx). 4xx other than 429
		// are permanent (bad request, auth) — fail immediately.
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !transient || attempt >= maxASRRetries {
			return "", "", nil, lastErr
		}

		wait := retryAfter
		if wait <= 0 {
			wait = asrRetryBase << attempt // 2s, 4s, 8s, 16s
		}
		log.Printf("groq transient error (status %d); retrying in %s (attempt %d/%d)\n",
			resp.StatusCode, wait, attempt+1, maxASRRetries)
		select {
		case <-ctx.Done():
			return "", "", nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// buildGroqMultipart assembles the transcription request body in memory so it can
// be replayed across retries.
func buildGroqMultipart(audioPath string) ([]byte, string, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", "whisper-large-v3"); err != nil {
		return nil, "", err
	}
	if err := mw.WriteField("response_format", "verbose_json"); err != nil {
		return nil, "", err
	}
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// parseRetryAfter reads a Retry-After header in delta-seconds form (Groq sends
// seconds, sometimes fractional). Returns 0 when absent/unparseable, so the
// caller falls back to its own exponential backoff.
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
// Real transcriber: Gemini 2.5 Flash (native audio, structured JSON output)
// ---------------------------------------------------------------------------

type geminiTranscriber struct{ apiKey string }

func newGeminiTranscriber(apiKey string) *geminiTranscriber {
	return &geminiTranscriber{apiKey: apiKey}
}

const geminiPrompt = "Transcribe this audio verbatim in its original spoken language. " +
	"Do not translate. Return the detected ISO 639-1 language code and timestamped " +
	"segments with start and end times in seconds."

func (g *geminiTranscriber) Transcribe(ctx context.Context, audioPath string) (string, string, []Segment, error) {
	data, err := os.ReadFile(audioPath)
	if err != nil {
		return "", "", nil, err
	}

	reqBody := map[string]any{
		"contents": []any{map[string]any{
			"parts": []any{
				map[string]any{"inline_data": map[string]any{
					"mime_type": "audio/mp3",
					"data":      base64.StdEncoding.EncodeToString(data),
				}},
				map[string]any{"text": geminiPrompt},
			},
		}},
		"generationConfig": map[string]any{
			"response_mime_type": "application/json",
			"response_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"language": map[string]any{"type": "string"},
					"segments": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"start": map[string]any{"type": "number"},
								"end":   map[string]any{"type": "number"},
								"text":  map[string]any{"type": "string"},
							},
							"required": []string{"start", "end", "text"},
						},
					},
				},
				"required": []string{"language", "segments"},
			},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", nil, err
	}

	// Pass the API key via header, never the query string: a transport error from
	// Do() is a *url.Error that embeds the full URL in its message, which would
	// otherwise leak the key into logs.
	const endpoint = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := transcribeClient.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", nil, fmt.Errorf("gemini API error (status %d): %s", resp.StatusCode, string(body))
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
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", "", nil, err
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", "", nil, fmt.Errorf("gemini returned no candidates")
	}

	var parsed struct {
		Language string `json:"language"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal([]byte(gr.Candidates[0].Content.Parts[0].Text), &parsed); err != nil {
		return "", "", nil, fmt.Errorf("failed to parse gemini transcript JSON: %w", err)
	}

	segs := make([]Segment, len(parsed.Segments))
	texts := make([]string, len(parsed.Segments))
	for i, s := range parsed.Segments {
		segs[i] = Segment{Start: s.Start, End: s.End, Text: strings.TrimSpace(s.Text)}
		texts[i] = strings.TrimSpace(s.Text)
	}
	return strings.Join(texts, " "), parsed.Language, segs, nil
}

// ---------------------------------------------------------------------------
// Real database: Neon PostgreSQL via pgx
// ---------------------------------------------------------------------------

type pgxDatabase struct{ conn *pgx.Conn }

// PendingVideos returns videos present in either collector table that do not yet
// have a completed transcript, capped at limit.
func (d *pgxDatabase) PendingVideos(ctx context.Context, limit int) ([]VideoRef, error) {
	// Pending = videos in either collector table without a 'done' transcript,
	// excluding failed videos that have exhausted the retry cap (so deleted/private
	// videos stop being retried forever). Never-attempted videos are ordered before
	// previously-failed ones so a handful of failures can never starve the backlog.
	const query = `
		SELECT v.youtube_video_id, MIN(v.url) AS url
		FROM (
			SELECT youtube_video_id, url FROM channel_videos
			UNION ALL
			SELECT youtube_video_id, url FROM playlist_videos
		) v
		LEFT JOIN transcripts t ON t.youtube_video_id = v.youtube_video_id
		WHERE (t.youtube_video_id IS NULL OR t.status <> 'done')
		  AND NOT (t.status = 'failed' AND t.attempt_count >= $2)
		GROUP BY v.youtube_video_id
		ORDER BY MIN(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END) ASC
		LIMIT $1`
	rows, err := d.conn.Query(ctx, query, limit, maxFailedAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []VideoRef
	for rows.Next() {
		var r VideoRef
		if err := rows.Scan(&r.YoutubeVideoID, &r.URL); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// SaveTranscript writes the header and its segments in a single transaction.
// Idempotent on youtube_video_id: a re-run replaces the transcript and its
// segments (e.g. retrying a previously failed video).
func (d *pgxDatabase) SaveTranscript(ctx context.Context, t Transcript, segs []Segment) error {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// attempt_count tracks consecutive failures: start a brand-new failed row at 1,
	// a done row at 0; on conflict, increment when the new save is 'failed' and
	// reset to 0 when it is 'done'.
	initialAttempt := 0
	if t.Status == "failed" {
		initialAttempt = 1
	}
	const upsert = `
		INSERT INTO transcripts
			(source_type, youtube_video_id, source_ref, language, engine, transcript, duration_seconds, status, error, attempt_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (youtube_video_id) DO UPDATE SET
			source_type = EXCLUDED.source_type,
			source_ref = EXCLUDED.source_ref,
			language = EXCLUDED.language,
			engine = EXCLUDED.engine,
			transcript = EXCLUDED.transcript,
			duration_seconds = EXCLUDED.duration_seconds,
			status = EXCLUDED.status,
			error = EXCLUDED.error,
			attempt_count = CASE WHEN EXCLUDED.status = 'failed'
			                     THEN transcripts.attempt_count + 1
			                     ELSE 0 END,
			updated_at = CURRENT_TIMESTAMP
		RETURNING id`
	var id int
	if err := tx.QueryRow(ctx, upsert,
		t.SourceType,
		nullStr(t.YoutubeVideoID),
		t.SourceRef,
		nullStr(t.Language),
		t.Engine,
		nullStr(t.Text),
		nullInt(t.DurationSeconds),
		t.Status,
		nullStr(t.Error),
		initialAttempt,
	).Scan(&id); err != nil {
		return err
	}

	// Replace any existing segments for this transcript. CopyFrom streams all
	// segments in one round-trip — far cheaper than per-row INSERTs for long
	// videos (hundreds of segments) at large batch sizes.
	if _, err := tx.Exec(ctx, `DELETE FROM transcript_segments WHERE transcript_id = $1`, id); err != nil {
		return err
	}
	if len(segs) > 0 {
		rows := make([][]any, len(segs))
		for i, s := range segs {
			rows[i] = []any{id, i, s.Start, s.End, s.Text}
		}
		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"transcript_segments"},
			[]string{"transcript_id", "seq", "start_seconds", "end_seconds", "text"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// ---------------------------------------------------------------------------
// Config & entrypoint
// ---------------------------------------------------------------------------

// resolveBin returns an absolute path to an external binary: the env override if
// set, otherwise the fixed container default. It deliberately avoids resolving
// the command name through $PATH.
func resolveBin(envVar, defaultPath string) string {
	if p := os.Getenv(envVar); p != "" {
		return p
	}
	return defaultPath
}

func loadConfig() Config {
	batch := defaultBatchSize
	if v := os.Getenv("BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			batch = n
		}
	}
	return Config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		Engine:       os.Getenv("TRANSCRIBE_ENGINE"),
		GroqAPIKey:   os.Getenv("GROQ_API_KEY"),
		GeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
		Cookies:      os.Getenv("YT_DLP_COOKIES"),
		BatchSize:    batch,
	}
}

func main() {
	sourceFlag := flag.String("source", "", "Transcribe a single source (YouTube/url/local path) instead of the batch")
	flag.Parse()

	cfg := loadConfig()
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	tr, engineName, err := NewTranscriber(cfg)
	if err != nil {
		log.Fatalf("Transcriber init failed: %v", err)
	}

	cookieFile, cleanup, err := writeCookieFile(cfg.Cookies)
	if err != nil {
		log.Fatalf("Failed to materialize cookies: %v", err)
	}
	defer cleanup()

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := pgx.Connect(connectCtx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())
	log.Println("Connected to database successfully")

	db := &pgxDatabase{conn: conn}
	// Resolve the external binaries to absolute paths (no $PATH lookup, which
	// could be hijacked). Local runs set YT_DLP_BIN / FFMPEG_BIN (Homebrew paths)
	// via ~/.rara-scribe/.env; the fallbacks below are last-resort defaults.
	acq := newYtDlpAcquirer(
		resolveBin("YT_DLP_BIN", "/opt/homebrew/bin/yt-dlp"),
		resolveBin("FFMPEG_BIN", "/opt/homebrew/bin/ffmpeg"),
		cookieFile,
	)
	ctx := context.Background()

	if *sourceFlag != "" {
		src := Source{Type: detectSourceType(*sourceFlag), Ref: *sourceFlag}
		log.Printf("Transcribing single source (%s): %s\n", src.Type, src.Ref)
		srcCtx, cancel := context.WithTimeout(ctx, sourceTimeout)
		t, segs := transcribeSource(srcCtx, acq, tr, engineName, src)
		cancel()
		if err := db.SaveTranscript(ctx, t, segs); err != nil {
			log.Fatalf("Failed to save transcript: %v", err)
		}
		if t.Status == "failed" {
			log.Fatalf("Transcription failed: %s", t.Error)
		}
		log.Printf("Done: %s (%s, %d segments)\n", t.SourceRef, t.Language, len(segs))
		return
	}

	if err := runBatch(ctx, db, acq, tr, engineName, cfg.BatchSize); err != nil {
		log.Fatalf("Batch failed: %v", err)
	}
	log.Println("Scribe job completed successfully")
}

// writeCookieFile materializes the YT_DLP_COOKIES secret to a temp file (yt-dlp
// needs a path). Returns the path and a cleanup func; both are no-ops when the
// secret is empty.
func writeCookieFile(cookies string) (string, func(), error) {
	if cookies == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "yt-cookies-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(cookies); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}
