package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// Engine identifiers (TRANSCRIBE_ENGINE) and their display names (stored per row).
const (
	engineGroq   = "groq"
	engineGemini = "gemini"
	engineLocal  = "local" // whisper.cpp on this machine (no API quota)

	groqModelName       = "groq/whisper-large-v3"
	geminiModelName     = "gemini/gemini-2.5-flash"
	whisperCppModelName = "whispercpp/whisper-large-v3"
)

// Transcript status values (the transcripts.status column).
const (
	statusDone   = "done"   // transcribed with content
	statusFailed = "failed" // acquisition or ASR error (retried up to the cap)
	statusEmpty  = "empty"  // transcribed but no speech (silent/music) — terminal
)

const (
	// remoteChunkSeconds is the ffmpeg segment length for the API engines: each
	// 10-minute chunk of 16 kHz mono audio stays well under Groq's 25 MB upload
	// limit, so we never need GCS/URL uploads. localChunkSeconds is larger because
	// whisper.cpp has no upload limit and each chunk costs a separate process
	// spawn + 3 GB model load — bigger chunks mean far fewer reloads, bounded at
	// ~1 h of PCM in memory. Both are the per-chunk global timestamp offset step.
	remoteChunkSeconds = 600
	localChunkSeconds  = 3600

	defaultBatchSize = 25

	// remoteSourceTimeout / localSourceTimeout bound the work on a single video
	// (download + transcribe), so a stuck yt-dlp/ffmpeg or a hung upload cannot
	// block the whole batch. Local large-v3 is wall-clock work (~0.1x real-time on
	// Apple Silicon, but slower on long/noisy audio), so it gets a larger budget
	// than the network-bound API engines. Override either with SOURCE_TIMEOUT_MINUTES.
	remoteSourceTimeout = 20 * time.Minute
	localSourceTimeout  = 60 * time.Minute

	// saveTimeout bounds the per-video database write, so a hung DB connection
	// cannot stall the whole batch on its own (the batch ctx is unbounded).
	saveTimeout = 30 * time.Second

	// maxFailedAttempts caps how many times a failing video is retried. Past this,
	// PendingVideos skips it so permanently-broken videos (deleted/private) stop
	// burning yt-dlp/ASR calls every run. A 'done' save resets the counter.
	maxFailedAttempts = 5

	// whisperCppBeamSize is the default beam-search width for the local whisper.cpp
	// engine. 1 = greedy (fastest); override with WHISPER_CPP_BEAM_SIZE. large-v3
	// quality at beam=1 is already on par with Groq for clear speech; use beam=5
	// only if you see accuracy regressions on noisy/accented audio.
	whisperCppBeamSize = 1

	// whisperCppThreads is the default CPU thread count for the local whisper.cpp
	// engine. Override with WHISPER_CPP_THREADS. With Metal enabled (M-series Mac)
	// most compute runs on the GPU; threads affect CPU pre/post-processing only.
	whisperCppThreads = 8

	// localCircuitBreakerThreshold disables the local primary (routing to the Groq
	// fallback) after this many consecutive per-chunk failures, so a fully broken
	// local setup doesn't waste a process spawn on every chunk of every video.
	localCircuitBreakerThreshold = 3

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

// sourceTimeout is the active per-video budget. main sets it from the engine via
// resolveSourceTimeout; it is a var (not const) so tests use the default and main
// can raise it for the slower local engine.
var sourceTimeout = remoteSourceTimeout

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
	// TransientFailure marks a 'failed' save as caused by a transient ASR error (a
	// 429 rate limit or a 5xx) rather than a permanent one (deleted/private video,
	// bad request). SaveTranscript keeps attempt_count unchanged for these, so an
	// exhausted daily quota never counts toward the retry cap.
	TransientFailure bool
}

// VideoRef is a video pending transcription, discovered from the collector tables.
type VideoRef struct {
	YoutubeVideoID string
	URL            string
}

// Config is the runtime configuration, sourced from environment variables.
type Config struct {
	DatabaseURL        string
	Engine             string
	GroqAPIKey         string
	GeminiAPIKey       string
	WhisperCppBin      string // whisper.cpp CLI (engine "local")
	WhisperCppModel    string // ggml model file for whisper.cpp
	WhisperCppVADModel string // optional silero VAD model for whisper.cpp
	WhisperCppBeam     int    // beam-search width (1=greedy/fast, 5=quality)
	WhisperCppThreads  int    // CPU threads for pre/post-processing
	Cookies            string
	BatchSize          int
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

// chunkProgressLabel formats a per-chunk progress line so a long source shows
// life in the logs instead of going silent for the minutes it takes whisper.cpp
// to grind through up to an hour of audio per chunk. Newlines in the id (a
// url/local ref) are flattened so a crafted ref can't forge extra log lines.
func chunkProgressLabel(id string, n, total int) string {
	id = strings.NewReplacer("\n", " ", "\r", " ").Replace(id)
	return fmt.Sprintf("  %s: transcribing chunk %d/%d", id, n, total)
}

// shouldLogChunkProgress decides whether to emit per-chunk progress. The local
// engine is always logged — each chunk is minutes of wall-clock work, so even a
// single chunk would otherwise sit silent — as is any multi-chunk source. Fast
// single-chunk API transcriptions stay quiet to avoid log noise on the nightly run.
func shouldLogChunkProgress(engineName string, totalChunks int) bool {
	return totalChunks > 1 || engineName == whisperCppModelName
}

// resolveSourceTimeout picks the per-video budget: a SOURCE_TIMEOUT_MINUTES env
// override (any engine), else the engine default — larger for local, whose
// transcription is wall-clock work rather than a network round-trip.
func resolveSourceTimeout(engine string) time.Duration {
	if v := os.Getenv("SOURCE_TIMEOUT_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	if engine == engineLocal {
		return localSourceTimeout
	}
	return remoteSourceTimeout
}

// chunkSecondsFor picks the audio segment length: small for the API engines
// (Groq's 25 MB upload limit), large for local (no limit, and fewer chunks means
// fewer whisper.cpp process spawns + model reloads).
func chunkSecondsFor(engine string) int {
	if engine == engineLocal {
		return localChunkSeconds
	}
	return remoteChunkSeconds
}

// existsFile returns a descriptive error when path is not a readable file, so the
// engine factory can fail fast on a misconfigured binary/model path instead of
// failing every chunk at runtime.
func existsFile(label, path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s %q: %w", label, path, err)
	}
	return nil
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
	case engineLocal:
		if cfg.WhisperCppBin == "" || cfg.WhisperCppModel == "" {
			return nil, "", fmt.Errorf("WHISPER_CPP_BIN and WHISPER_CPP_MODEL are required for engine %q", engineLocal)
		}
		// Fail fast on a misconfigured path now, instead of failing every chunk at
		// runtime (and, under the hybrid, silently burning Groq quota on fallback).
		if err := existsFile("WHISPER_CPP_BIN", cfg.WhisperCppBin); err != nil {
			return nil, "", err
		}
		if err := existsFile("WHISPER_CPP_MODEL", cfg.WhisperCppModel); err != nil {
			return nil, "", err
		}
		if cfg.WhisperCppVADModel != "" {
			if err := existsFile("WHISPER_CPP_VAD_MODEL", cfg.WhisperCppVADModel); err != nil {
				return nil, "", err
			}
		}
		local := newWhisperCppTranscriber(cfg)
		// Hybrid: with a Groq key present, run local as primary and Groq as the
		// fallback so a per-chunk local failure still completes via the API. The
		// stored engine name stays the local one (the primary path). A circuit
		// breaker disables local after repeated failures (see fallbackTranscriber).
		if cfg.GroqAPIKey != "" {
			return &fallbackTranscriber{
				primary:            local,
				secondary:          newGroqTranscriber(cfg.GroqAPIKey),
				primaryName:        whisperCppModelName,
				secondaryName:      groqModelName,
				maxPrimaryFailures: localCircuitBreakerThreshold,
			}, whisperCppModelName, nil
		}
		return local, whisperCppModelName, nil
	default:
		return nil, "", fmt.Errorf("unknown engine %q (set TRANSCRIBE_ENGINE or --engine to %q, %q or %q)", cfg.Engine, engineGroq, engineGemini, engineLocal)
	}
}

// ---------------------------------------------------------------------------
// Orchestration (engine/acquirer-agnostic; unit-tested via mocks)
// ---------------------------------------------------------------------------

// transientError marks an ASR failure as transient (retryable) — a rate limit
// (HTTP 429) or a server-side 5xx — as opposed to a permanent failure (deleted/
// private video, bad request, auth). The transcribers wrap their final error in
// this type so transcribeSource can flag the Transcript and SaveTranscript can
// keep the retry counter unchanged: a daily-quota exhaustion must not retire an
// otherwise-fine video.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// isTransient reports whether err (or anything it wraps) is a transientError.
func isTransient(err error) bool {
	var te *transientError
	return errors.As(err, &te)
}

// asrResult is one successful ASR attempt's parsed output.
type asrResult struct {
	text     string
	language string
	segs     []Segment
}

// asrAttempt performs a single ASR HTTP call. On success it returns the parsed
// result and a nil error. On failure it returns the HTTP status code (0 for a
// transport-level error, treated as permanent), an optional Retry-After hint, and
// the error.
type asrAttempt func(ctx context.Context) (res asrResult, status int, retryAfter time.Duration, err error)

// retryTransientASR runs attempt with exponential backoff, retrying only
// transient failures — an HTTP 429 (rate limit) or a 5xx — up to maxASRRetries.
// Permanent failures (a 4xx other than 429, a transport error, a parse error)
// return immediately. A transient failure that survives the retries, or whose
// backoff is cut short by context cancellation, is wrapped in *transientError so
// it does not count toward the per-video retry cap: an exhausted daily quota must
// not retire an otherwise-fine video.
func retryTransientASR(ctx context.Context, engine string, attempt asrAttempt) (string, string, []Segment, error) {
	for n := 0; ; n++ {
		res, status, retryAfter, err := attempt(ctx)
		if err == nil {
			return res.text, res.language, res.segs, nil
		}
		transient := status == http.StatusTooManyRequests || status >= 500
		if !transient || n >= maxASRRetries {
			if transient {
				return "", "", nil, &transientError{err}
			}
			return "", "", nil, err
		}
		wait := retryAfter
		if wait <= 0 {
			wait = asrRetryBase << n // 2s, 4s, 8s, 16s
		}
		log.Printf("%s transient error (status %d); retrying in %s (attempt %d/%d)\n",
			engine, status, wait, n+1, maxASRRetries)
		select {
		case <-ctx.Done():
			// The backoff we're serving is for a transient failure; if the context
			// expires mid-wait, the root cause is still that transient error, so
			// surface it as transient rather than counting it against the cap.
			return "", "", nil, &transientError{err}
		case <-time.After(wait):
		}
	}
}

// transcribeSource runs the full pipeline for one source: acquire audio, then
// transcribe every chunk and stitch the result. It never returns an error —
// failures are captured as a "failed" Transcript so the batch can persist them
// and carry on.
func transcribeSource(ctx context.Context, acq AudioAcquirer, tr Transcriber, engineName string, src Source) (Transcript, []Segment) {
	t := Transcript{
		SourceType: src.Type,
		SourceRef:  src.Ref,
		Engine:     engineName,
		Status:     statusDone,
	}
	if src.Type == "youtube" {
		t.YoutubeVideoID = extractVideoID(src.Ref)
	}

	chunks, err := acq.Acquire(ctx, src)
	if err != nil {
		t.Status, t.Error = statusFailed, err.Error()
		return t, nil
	}
	defer cleanupChunks(chunks)

	var (
		parts      []string
		segs       []Segment
		langCounts = map[string]int{}
	)
	// A label for progress logs: the youtube id when known, else the raw ref.
	label := t.YoutubeVideoID
	if label == "" {
		label = src.Ref
	}
	logProgress := shouldLogChunkProgress(engineName, len(chunks))
	for i, ch := range chunks {
		if logProgress {
			log.Println(chunkProgressLabel(label, i+1, len(chunks)))
		}
		text, lang, chunkSegs, err := tr.Transcribe(ctx, ch.Path)
		if err != nil {
			t.Status, t.Error = statusFailed, err.Error()
			t.TransientFailure = isTransient(err)
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
	// No speech at all (silent or music-only video): mark 'empty' so it is both
	// distinguishable from a real transcript and terminal — PendingVideos won't
	// keep reprocessing it every run.
	if len(segs) == 0 && t.Text == "" {
		t.Status = statusEmpty
	}
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
	ytDlp        string // absolute path to the yt-dlp binary
	ffmpeg       string // absolute path to the ffmpeg binary
	cookieFile   string // optional path to a cookies.txt file
	chunkSeconds int    // ffmpeg segment length and global offset step
}

func newYtDlpAcquirer(ytDlp, ffmpeg, cookieFile string, chunkSeconds int) *ytDlpAcquirer {
	return &ytDlpAcquirer{ytDlp: ytDlp, ffmpeg: ffmpeg, cookieFile: cookieFile, chunkSeconds: chunkSeconds}
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
		"-i", safeFFmpegInput(input),
		"-ar", "16000", "-ac", "1",
		"-f", "segment", "-segment_time", strconv.Itoa(a.chunkSeconds),
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
		chunks[i] = AudioChunk{Path: m, Offset: float64(i * a.chunkSeconds)}
	}
	return chunks, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// safeFFmpegInput stops a source ref from being interpreted by ffmpeg as an option
// (leading "-") or a protocol (e.g. "concat:", "subfile:"): a non-absolute path is
// forced to a literal relative path with "./". Absolute paths — our temp downloads
// in the batch path — pass through unchanged. This only matters for the manual
// --source mode with a local/url ref; the batch always feeds an absolute temp file.
func safeFFmpegInput(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return "./" + p
}

// truncate caps a string (e.g. a provider error body) to max runes for logging,
// appending an ellipsis when it was cut, so logs stay bounded.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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

	return retryTransientASR(ctx, "groq", func(ctx context.Context) (asrResult, int, time.Duration, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return asrResult{}, 0, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
		req.Header.Set("Content-Type", contentType)

		resp, err := transcribeClient.Do(req)
		if err != nil {
			return asrResult{}, 0, 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return asrResult{}, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")),
				fmt.Errorf("groq API error (status %d): %s", resp.StatusCode, truncate(string(body), 500))
		}

		var vj verboseJSON
		if err := json.NewDecoder(resp.Body).Decode(&vj); err != nil {
			return asrResult{}, 0, 0, err
		}
		segs := make([]Segment, len(vj.Segments))
		for i, s := range vj.Segments {
			segs[i] = Segment{Start: s.Start, End: s.End, Text: strings.TrimSpace(s.Text)}
		}
		return asrResult{text: vj.Text, language: vj.Language, segs: segs}, http.StatusOK, 0, nil
	})
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

type geminiTranscriber struct {
	apiKey   string
	endpoint string // overridable for tests; defaults to the Gemini API
}

func newGeminiTranscriber(apiKey string) *geminiTranscriber {
	return &geminiTranscriber{
		apiKey:   apiKey,
		endpoint: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
	}
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

	// A 429 (rate limit) or 5xx is transient and retried with backoff, on par with
	// the Groq engine, so a momentary quota blip does not fail an otherwise-fine
	// video. Permanent errors (4xx, parse failures) return immediately.
	return retryTransientASR(ctx, "gemini", func(ctx context.Context) (asrResult, int, time.Duration, error) {
		// Pass the API key via header, never the query string: a transport error from
		// Do() is a *url.Error that embeds the full URL in its message, which would
		// otherwise leak the key into logs.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return asrResult{}, 0, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", g.apiKey)

		resp, err := transcribeClient.Do(req)
		if err != nil {
			return asrResult{}, 0, 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return asrResult{}, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")),
				fmt.Errorf("gemini API error (status %d): %s", resp.StatusCode, truncate(string(body), 500))
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
			return asrResult{}, 0, 0, err
		}
		if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
			return asrResult{}, 0, 0, fmt.Errorf("gemini returned no candidates")
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
			return asrResult{}, 0, 0, fmt.Errorf("failed to parse gemini transcript JSON: %w", err)
		}

		segs := make([]Segment, len(parsed.Segments))
		texts := make([]string, len(parsed.Segments))
		for i, s := range parsed.Segments {
			segs[i] = Segment{Start: s.Start, End: s.End, Text: strings.TrimSpace(s.Text)}
			texts[i] = strings.TrimSpace(s.Text)
		}
		return asrResult{text: strings.Join(texts, " "), language: parsed.Language, segs: segs}, http.StatusOK, 0, nil
	})
}

// ---------------------------------------------------------------------------
// Local transcriber: whisper.cpp large-v3 (offline, no API quota)
// ---------------------------------------------------------------------------

// whisperCppTranscriber shells out to the whisper.cpp CLI for fully local ASR.
// whisper.cpp reads our 16 kHz mono mp3 chunks directly (miniaudio), so no
// transcode is needed; it transcribes with beam search (and optional silero VAD)
// for quality on par with Groq's whisper-large-v3.
type whisperCppTranscriber struct {
	bin      string // whisper.cpp CLI (e.g. whisper-cli)
	model    string // ggml model file (e.g. ggml-large-v3.bin)
	vadModel string // optional silero VAD model; "" disables VAD
	beam     int    // beam-search width (1=greedy/fast, 5=quality)
	threads  int    // CPU threads for pre/post-processing
}

func newWhisperCppTranscriber(cfg Config) *whisperCppTranscriber {
	return &whisperCppTranscriber{
		bin:      cfg.WhisperCppBin,
		model:    cfg.WhisperCppModel,
		vadModel: cfg.WhisperCppVADModel,
		beam:     cfg.WhisperCppBeam,
		threads:  cfg.WhisperCppThreads,
	}
}

func (w *whisperCppTranscriber) Transcribe(ctx context.Context, audioPath string) (string, string, []Segment, error) {
	work, err := os.MkdirTemp("", "whispercpp-*")
	if err != nil {
		return "", "", nil, err
	}
	defer os.RemoveAll(work)

	// -oj writes the transcript JSON to <outBase>.json.
	outBase := filepath.Join(work, "out")
	args := []string{
		"-m", w.model,
		"-f", audioPath,
		"-l", "auto",
		"-t", strconv.Itoa(w.threads),
		"-bs", strconv.Itoa(w.beam),
		"-oj", "-of", outBase,
	}
	// Optional VAD (silero): trims silence/music, cutting hallucinations on
	// non-speech audio.
	if w.vadModel != "" {
		args = append(args, "--vad", "--vad-model", w.vadModel)
	}
	if out, err := exec.CommandContext(ctx, w.bin, args...).CombinedOutput(); err != nil {
		return "", "", nil, fmt.Errorf("whisper.cpp failed: %w: %s", err, lastLine(out))
	}

	data, err := os.ReadFile(outBase + ".json")
	if err != nil {
		return "", "", nil, fmt.Errorf("whisper.cpp produced no JSON: %w", err)
	}
	return parseWhisperCppJSON(data)
}

// whisperCppJSON mirrors the relevant fields of whisper.cpp's -oj output.
type whisperCppJSON struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int `json:"from"` // milliseconds
			To   int `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// parseWhisperCppJSON converts whisper.cpp's -oj output into the engine-agnostic
// (text, language, segments) shape. Offsets are milliseconds → seconds; the full
// text is the segments joined (whisper.cpp emits no top-level transcript field).
func parseWhisperCppJSON(data []byte) (string, string, []Segment, error) {
	var wj whisperCppJSON
	if err := json.Unmarshal(data, &wj); err != nil {
		return "", "", nil, fmt.Errorf("failed to parse whisper.cpp JSON: %w", err)
	}
	segs := make([]Segment, len(wj.Transcription))
	texts := make([]string, len(wj.Transcription))
	for i, s := range wj.Transcription {
		segs[i] = Segment{
			Start: float64(s.Offsets.From) / 1000,
			End:   float64(s.Offsets.To) / 1000,
			Text:  strings.TrimSpace(s.Text),
		}
		texts[i] = strings.TrimSpace(s.Text)
	}
	return strings.Join(texts, " "), wj.Result.Language, segs, nil
}

// ---------------------------------------------------------------------------
// Hybrid fallback transcriber: primary engine, secondary on error
// ---------------------------------------------------------------------------

// fallbackTranscriber runs primary first and, only if it errors while the context
// is still live, retries the same chunk on secondary. It is the hybrid seam:
// local whisper.cpp as primary with Groq as the safety net.
//
// A circuit breaker disables the primary after maxPrimaryFailures consecutive
// failures (0 = breaker off), so a fully broken primary doesn't cost a spawn on
// every chunk. State is plain (no mutex): the pipeline transcribes one chunk at a
// time, never concurrently.
type fallbackTranscriber struct {
	primary, secondary Transcriber
	primaryName        string
	secondaryName      string
	maxPrimaryFailures int

	primaryFailures int
	primaryDisabled bool
}

func (f *fallbackTranscriber) Transcribe(ctx context.Context, audioPath string) (string, string, []Segment, error) {
	if !f.primaryDisabled {
		text, lang, segs, err := f.primary.Transcribe(ctx, audioPath)
		if err == nil {
			f.primaryFailures = 0
			return text, lang, segs, nil
		}
		// A cancelled/expired context will fail the secondary too, and is not a
		// primary fault — don't waste the call or count it toward the breaker.
		if ctx.Err() != nil {
			return "", "", nil, err
		}
		f.primaryFailures++
		log.Printf("primary ASR (%s) failed: %v; falling back to %s\n", f.primaryName, err, f.secondaryName)
		if f.maxPrimaryFailures > 0 && f.primaryFailures >= f.maxPrimaryFailures {
			f.primaryDisabled = true
			log.Printf("primary ASR (%s) disabled after %d consecutive failures; using %s for the rest of this run\n",
				f.primaryName, f.primaryFailures, f.secondaryName)
		}
	}
	return f.secondary.Transcribe(ctx, audioPath)
}

// ---------------------------------------------------------------------------
// Real database: Neon PostgreSQL via pgx
// ---------------------------------------------------------------------------

// pgxDatabase talks to Neon through a connection pool rather than a single
// long-lived connection: a batch can sit minutes between saves (a long video),
// and Neon's pooler drops idle connections — which used to surface as "conn
// closed" / "broken pipe" and lose an already-transcribed video. The pool
// validates and recreates connections on acquire, so each save gets a live one.
type pgxDatabase struct{ pool *pgxpool.Pool }

// PendingVideos returns videos present in either collector table that do not yet
// have a completed transcript, capped at limit.
func (d *pgxDatabase) PendingVideos(ctx context.Context, limit int) ([]VideoRef, error) {
	// Pending = videos in either collector table without a terminal transcript.
	// Terminal = 'done' (has content) or 'empty' (no speech) — neither is retried.
	// Failed videos past the retry cap are also excluded (so deleted/private videos
	// stop being retried forever).
	//
	// Ordering, in priority order:
	//   1. never-attempted videos before previously-failed ones, so a backlog of
	//      failures can never starve fresh work;
	//   2. among failed rows, fewest attempts first;
	//   3. then least-recently-tried first (updated_at ASC) — a round-robin so a
	//      large transient backlog (all at attempt_count=0, e.g. quota-starved
	//      videos awaiting the no-quota local engine) drains fairly instead of the
	//      same rows being re-tried every run while others wait forever.
	const query = `
		SELECT v.youtube_video_id, MIN(v.url) AS url
		FROM (
			SELECT youtube_video_id, url FROM channel_videos
			UNION ALL
			SELECT youtube_video_id, url FROM playlist_videos
		) v
		LEFT JOIN transcripts t ON t.youtube_video_id = v.youtube_video_id
		WHERE (t.youtube_video_id IS NULL OR t.status NOT IN ('done', 'empty'))
		  AND (t.status IS DISTINCT FROM 'failed' OR t.attempt_count < $2)
		GROUP BY v.youtube_video_id
		ORDER BY
			MIN(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END) ASC,
			MIN(COALESCE(t.attempt_count, 0)) ASC,
			MIN(t.updated_at) ASC NULLS FIRST
		LIMIT $1`
	rows, err := d.pool.Query(ctx, query, limit, maxFailedAttempts)
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
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// attempt_count tracks consecutive *permanent* failures: start a brand-new
	// permanent-failed row at 1, every other brand-new row (done/empty, or a
	// transient failure) at 0. On conflict, a permanent failure increments, a
	// transient failure (429/5xx) leaves the counter unchanged, and a done/empty
	// save resets to 0. Transient failures are deliberately excluded so an
	// exhausted daily quota never retires an otherwise-fine video.
	initialAttempt := 0
	if t.Status == statusFailed && !t.TransientFailure {
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
			attempt_count = CASE
			                  WHEN EXCLUDED.status = 'failed' AND $11 THEN transcripts.attempt_count
			                  WHEN EXCLUDED.status = 'failed'        THEN transcripts.attempt_count + 1
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
		t.TransientFailure,
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
	beam := whisperCppBeamSize
	if v := os.Getenv("WHISPER_CPP_BEAM_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			beam = n
		}
	}
	threads := whisperCppThreads
	if v := os.Getenv("WHISPER_CPP_THREADS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			threads = n
		}
	}
	return Config{
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		Engine:             os.Getenv("TRANSCRIBE_ENGINE"),
		GroqAPIKey:         os.Getenv("GROQ_API_KEY"),
		GeminiAPIKey:       os.Getenv("GEMINI_API_KEY"),
		WhisperCppBin:      resolveBin("WHISPER_CPP_BIN", "/opt/homebrew/bin/whisper-cli"),
		WhisperCppModel:    os.Getenv("WHISPER_CPP_MODEL"),
		WhisperCppVADModel: os.Getenv("WHISPER_CPP_VAD_MODEL"),
		WhisperCppBeam:     beam,
		WhisperCppThreads:  threads,
		Cookies:            os.Getenv("YT_DLP_COOKIES"),
		BatchSize:          batch,
	}
}

// applyOverrides lets a single manual run override the .env-sourced engine and
// batch size via CLI flags, without editing the file the scheduled run reads. An
// empty engine or non-positive limit leaves that field untouched.
func applyOverrides(cfg Config, engine string, limit int) Config {
	if engine != "" {
		cfg.Engine = engine
	}
	if limit > 0 {
		cfg.BatchSize = limit
	}
	return cfg
}

func main() {
	sourceFlag := flag.String("source", "", "Transcribe a single source (YouTube/url/local path) instead of the batch")
	engineFlag := flag.String("engine", "", "override TRANSCRIBE_ENGINE for this run (groq|gemini|local)")
	limitFlag := flag.Int("limit", 0, "override how many videos to process this run (default: BATCH_SIZE)")
	flag.Parse()

	cfg := applyOverrides(loadConfig(), *engineFlag, *limitFlag)
	if *engineFlag != "" || *limitFlag > 0 {
		log.Printf("CLI overrides applied: engine=%s limit=%d (this run only)\n", cfg.Engine, cfg.BatchSize)
	}
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	tr, engineName, err := NewTranscriber(cfg)
	if err != nil {
		log.Fatalf("Transcriber init failed: %v", err)
	}

	// Engine-aware tuning: local transcription is slower wall-clock work and has no
	// upload limit, so it gets a larger per-video budget and bigger audio chunks
	// (fewer whisper.cpp model reloads).
	sourceTimeout = resolveSourceTimeout(cfg.Engine)

	cookieFile, cleanup, err := writeCookieFile(cfg.Cookies)
	if err != nil {
		log.Fatalf("Failed to materialize cookies: %v", err)
	}
	defer cleanup()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to parse DATABASE_URL: %v", err)
	}
	// A single-worker batch needs only a connection or two; cap the pool to stay
	// well under Neon's connection limit, and recycle idle connections before
	// Neon's pooler drops them. MaxConnIdleTime applies to the *gap between
	// database calls* — a long local chunk sits minutes between saves, so 5min
	// recycles the connection proactively before Neon's pooler does it first.
	poolCfg.MaxConns = 2
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.MaxConnLifetime = 30 * time.Minute

	// NewWithConfig validates config only; Ping below forces a real connection
	// so bad creds/host still fail fast at startup. context.Background() is
	// intentional: the pool lives until defer pool.Close(), not until any
	// request ctx is cancelled.
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatalf("Failed to create database pool: %v", err)
	}
	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = pool.Ping(connectCtx)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()
	log.Println("Connected to database successfully")

	db := &pgxDatabase{pool: pool}
	// Resolve the external binaries to absolute paths (no $PATH lookup, which
	// could be hijacked). Local runs set YT_DLP_BIN / FFMPEG_BIN (Homebrew paths)
	// via ~/.rara-scribe/.env; the fallbacks below are last-resort defaults.
	acq := newYtDlpAcquirer(
		resolveBin("YT_DLP_BIN", "/opt/homebrew/bin/yt-dlp"),
		resolveBin("FFMPEG_BIN", "/opt/homebrew/bin/ffmpeg"),
		cookieFile,
		chunkSecondsFor(cfg.Engine),
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
