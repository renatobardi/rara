package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestDetectSourceType(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ": "youtube",
		"https://youtu.be/dQw4w9WgXcQ":                "youtube",
		"http://youtube.com/shorts/abc":               "youtube",
		"https://vimeo.com/123456789":                 "url",
		"https://example.com/video.mp4":               "url",
		"/Users/bardi/Videos/talk.mp4":                "local",
		"talk.mp4":                                    "local",
	}
	for ref, want := range cases {
		if got := detectSourceType(ref); got != want {
			t.Errorf("detectSourceType(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestExtractVideoID(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":     "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                    "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":      "dQw4w9WgXcQ",
		"https://www.youtube.com/embed/dQw4w9WgXcQ?rel=0": "dQw4w9WgXcQ",
		"https://www.youtube.com/live/dQw4w9WgXcQ":        "dQw4w9WgXcQ",
		"dQw4w9WgXcQ":                    "dQw4w9WgXcQ",
		"https://example.com/no-id-here": "",
	}
	for in, want := range cases {
		if got := extractVideoID(in); got != want {
			t.Errorf("extractVideoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVideoURL(t *testing.T) {
	if got, want := videoURL("abc123"), "https://www.youtube.com/watch?v=abc123"; got != want {
		t.Errorf("videoURL = %q, want %q", got, want)
	}
}

func TestChunkProgressLabel(t *testing.T) {
	got := chunkProgressLabel("8osZn55uK7c", 3, 4)
	if !strings.Contains(got, "8osZn55uK7c") || !strings.Contains(got, "3/4") {
		t.Errorf("chunkProgressLabel = %q, want id and 3/4", got)
	}
	// Newlines/CRs in the id (a url/local ref) are flattened so a crafted ref
	// cannot forge extra log lines.
	if dirty := chunkProgressLabel("a\nFAKE LOG\rb", 1, 1); strings.ContainsAny(dirty, "\n\r") {
		t.Errorf("label must not contain newlines: %q", dirty)
	}
}

func TestShouldLogChunkProgress(t *testing.T) {
	cases := []struct {
		engine string
		chunks int
		want   bool
	}{
		{whisperCppModelName, 1, true}, // local, single chunk → still slow, log it
		{whisperCppModelName, 3, true}, // local, multi-chunk
		{groqModelName, 1, false},      // fast single-chunk API → quiet
		{groqModelName, 4, true},       // multi-chunk → log regardless of engine
		{geminiModelName, 1, false},    // fast single-chunk API → quiet
	}
	for _, c := range cases {
		if got := shouldLogChunkProgress(c.engine, c.chunks); got != c.want {
			t.Errorf("shouldLogChunkProgress(%q, %d) = %v, want %v", c.engine, c.chunks, got, c.want)
		}
	}
}

func TestReindexSegments(t *testing.T) {
	in := []Segment{{Start: 0, End: 5, Text: "a"}, {Start: 5, End: 12.5, Text: "b"}}
	out := reindexSegments(in, 600)
	if out[0].Start != 600 || out[0].End != 605 {
		t.Errorf("segment 0 = %+v, want start=600 end=605", out[0])
	}
	if out[1].Start != 605 || out[1].End != 612.5 {
		t.Errorf("segment 1 = %+v, want start=605 end=612.5", out[1])
	}
	// Reindex must not mutate the input.
	if in[0].Start != 0 {
		t.Errorf("input mutated: %+v", in[0])
	}
}

func TestDurationFromSegments(t *testing.T) {
	segs := []Segment{{Start: 0, End: 10}, {Start: 10, End: 23.4}}
	if got := durationFromSegments(segs); got != 23 {
		t.Errorf("durationFromSegments = %d, want 23", got)
	}
	if got := durationFromSegments(nil); got != 0 {
		t.Errorf("durationFromSegments(nil) = %d, want 0", got)
	}
}

func TestSafeFFmpegInput(t *testing.T) {
	cases := map[string]string{
		"/tmp/scribe-x/audio.webm": "/tmp/scribe-x/audio.webm", // absolute → unchanged
		"talk.mp4":                 "./talk.mp4",               // relative → forced path
		"-malicious.mp4":           "./-malicious.mp4",         // leading dash neutralised
		"concat:a|b":               "./concat:a|b",             // protocol neutralised
	}
	for in, want := range cases {
		if got := safeFFmpegInput(in); got != want {
			t.Errorf("safeFFmpegInput(%q) = %q, want %q", in, got, want)
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
	// Rune-safe: must not split a multi-byte rune.
	if got := truncate("áéíóú", 2); got != "áé…" {
		t.Errorf("truncate multibyte = %q, want áé…", got)
	}
}

func TestMajorityLanguage(t *testing.T) {
	if got := majorityLanguage(map[string]int{"pt": 2, "en": 1}); got != "pt" {
		t.Errorf("majority = %q, want pt", got)
	}
	// Tie → deterministic by language code (alphabetical).
	if got := majorityLanguage(map[string]int{"pt": 1, "en": 1}); got != "en" {
		t.Errorf("tie = %q, want en (alphabetical)", got)
	}
	if got := majorityLanguage(map[string]int{}); got != "" {
		t.Errorf("empty = %q, want \"\"", got)
	}
	if got := majorityLanguage(map[string]int{"pt": 1}); got != "pt" {
		t.Errorf("single = %q, want pt", got)
	}
}

// ---------------------------------------------------------------------------
// Engine factory
// ---------------------------------------------------------------------------

func TestNewTranscriberFactory(t *testing.T) {
	// Default ("") and explicit groq both select Groq, given a key.
	for _, engine := range []string{"", engineGroq} {
		tr, name, err := NewTranscriber(Config{Engine: engine, GroqAPIKey: "k"})
		if err != nil || tr == nil {
			t.Fatalf("engine %q: unexpected err=%v tr=%v", engine, err, tr)
		}
		if name != groqModelName {
			t.Errorf("engine %q: name = %q, want %q", engine, name, groqModelName)
		}
	}

	// Gemini selected with its key.
	tr, name, err := NewTranscriber(Config{Engine: engineGemini, GeminiAPIKey: "k"})
	if err != nil || tr == nil || name != geminiModelName {
		t.Fatalf("gemini: err=%v tr=%v name=%q", err, tr, name)
	}

	// Missing key → error.
	if _, _, err := NewTranscriber(Config{Engine: engineGroq}); err == nil {
		t.Error("expected error for groq without key")
	}
	if _, _, err := NewTranscriber(Config{Engine: engineGemini}); err == nil {
		t.Error("expected error for gemini without key")
	}

	// Local (whisper.cpp): selected with bin+model that exist on disk (the factory
	// fail-fasts on missing paths). Without a Groq key it is the bare local engine;
	// with a key it is wrapped as primary with Groq fallback — but the stored
	// engine name is always the local one.
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "whisper-cli")
	modelPath := filepath.Join(tmp, "ggml-large-v3.bin")
	for _, p := range []string{binPath, modelPath} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loc, name, err := NewTranscriber(Config{Engine: engineLocal, WhisperCppBin: binPath, WhisperCppModel: modelPath})
	if err != nil || loc == nil || name != whisperCppModelName {
		t.Fatalf("local: err=%v tr=%v name=%q", err, loc, name)
	}
	if _, ok := loc.(*whisperCppTranscriber); !ok {
		t.Errorf("local without groq key: type = %T, want *whisperCppTranscriber", loc)
	}
	hyb, name, err := NewTranscriber(Config{Engine: engineLocal, WhisperCppBin: binPath, WhisperCppModel: modelPath, GroqAPIKey: "k"})
	if err != nil || name != whisperCppModelName {
		t.Fatalf("local+groq: err=%v name=%q", err, name)
	}
	if _, ok := hyb.(*fallbackTranscriber); !ok {
		t.Errorf("local with groq key: type = %T, want *fallbackTranscriber", hyb)
	}

	// Local without bin/model → error.
	if _, _, err := NewTranscriber(Config{Engine: engineLocal}); err == nil {
		t.Error("expected error for local without bin/model")
	}
	// Local with bin/model paths that don't exist → fail-fast error.
	if _, _, err := NewTranscriber(Config{Engine: engineLocal, WhisperCppBin: "/no/such/bin", WhisperCppModel: "/no/such/model"}); err == nil {
		t.Error("expected fail-fast error for nonexistent whisper.cpp paths")
	}

	// Unknown engine → error.
	if _, _, err := NewTranscriber(Config{Engine: "whisperx", GroqAPIKey: "k"}); err == nil {
		t.Error("expected error for unknown engine")
	}
}

// ---------------------------------------------------------------------------
// Local whisper.cpp JSON parsing
// ---------------------------------------------------------------------------

func TestParseWhisperCppJSON(t *testing.T) {
	const sample = `{
		"result": {"language": "pt"},
		"transcription": [
			{"offsets": {"from": 0, "to": 2500}, "text": " Olá mundo"},
			{"offsets": {"from": 2500, "to": 5000}, "text": " segundo segmento "}
		]
	}`
	text, lang, segs, err := parseWhisperCppJSON([]byte(sample))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if lang != "pt" {
		t.Errorf("language = %q, want pt", lang)
	}
	if text != "Olá mundo segundo segmento" {
		t.Errorf("text = %q", text)
	}
	if len(segs) != 2 {
		t.Fatalf("segs len = %d, want 2", len(segs))
	}
	// Offsets are milliseconds → seconds, text trimmed.
	if segs[0] != (Segment{Start: 0, End: 2.5, Text: "Olá mundo"}) {
		t.Errorf("seg0 = %+v", segs[0])
	}
	if segs[1] != (Segment{Start: 2.5, End: 5, Text: "segundo segmento"}) {
		t.Errorf("seg1 = %+v", segs[1])
	}

	// Malformed JSON → error.
	if _, _, _, err := parseWhisperCppJSON([]byte("{not json")); err == nil {
		t.Error("expected error for malformed JSON")
	}

	// Empty transcription → empty text/segs, no error (pipeline marks it 'empty').
	text, _, segs, err = parseWhisperCppJSON([]byte(`{"result":{"language":"en"},"transcription":[]}`))
	if err != nil || text != "" || len(segs) != 0 {
		t.Errorf("empty: err=%v text=%q segs=%d", err, text, len(segs))
	}
}

// ---------------------------------------------------------------------------
// Hybrid fallback transcriber
// ---------------------------------------------------------------------------

// recordingTranscriber returns a fixed result/error and counts its invocations,
// so a test can assert whether the fallback secondary was reached.
type recordingTranscriber struct {
	text, language string
	segs           []Segment
	err            error
	calls          int
}

func (r *recordingTranscriber) Transcribe(ctx context.Context, audioPath string) (string, string, []Segment, error) {
	r.calls++
	return r.text, r.language, r.segs, r.err
}

func TestFallbackTranscriber(t *testing.T) {
	ctx := context.Background()

	// Primary succeeds → secondary is never called.
	primary := &recordingTranscriber{text: "local", language: "pt"}
	secondary := &recordingTranscriber{text: "groq"}
	f := &fallbackTranscriber{primary: primary, secondary: secondary}
	text, lang, _, err := f.Transcribe(ctx, "chunk.mp3")
	if err != nil || text != "local" || lang != "pt" {
		t.Fatalf("primary-ok: err=%v text=%q lang=%q", err, text, lang)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary called %d times, want 0", secondary.calls)
	}

	// Primary fails → secondary's result is used.
	primary = &recordingTranscriber{err: errors.New("whisper.cpp crashed")}
	secondary = &recordingTranscriber{text: "groq", language: "en"}
	f = &fallbackTranscriber{primary: primary, secondary: secondary}
	text, lang, _, err = f.Transcribe(ctx, "chunk.mp3")
	if err != nil || text != "groq" || lang != "en" {
		t.Fatalf("fallback: err=%v text=%q lang=%q", err, text, lang)
	}
	if secondary.calls != 1 {
		t.Errorf("secondary called %d times, want 1", secondary.calls)
	}

	// Both fail → an error surfaces.
	primary = &recordingTranscriber{err: errors.New("local down")}
	secondary = &recordingTranscriber{err: errors.New("groq down")}
	f = &fallbackTranscriber{primary: primary, secondary: secondary}
	if _, _, _, err := f.Transcribe(ctx, "chunk.mp3"); err == nil {
		t.Error("expected error when both engines fail")
	}

	// Cancelled context → no fallback attempt (the secondary would fail too).
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	primary = &recordingTranscriber{err: errors.New("ctx canceled")}
	secondary = &recordingTranscriber{text: "groq"}
	f = &fallbackTranscriber{primary: primary, secondary: secondary}
	if _, _, _, err := f.Transcribe(cancelled, "chunk.mp3"); err == nil {
		t.Error("expected error on cancelled context")
	}
	if secondary.calls != 0 {
		t.Errorf("secondary called %d times on cancelled ctx, want 0", secondary.calls)
	}
}

// TestFallbackCircuitBreaker: after maxPrimaryFailures consecutive primary
// failures, the primary is disabled and every later chunk goes straight to the
// secondary (no wasted primary spawn on a fully-broken local engine).
func TestFallbackCircuitBreaker(t *testing.T) {
	ctx := context.Background()
	primary := &recordingTranscriber{err: errors.New("local broken")}
	secondary := &recordingTranscriber{text: "groq"}
	f := &fallbackTranscriber{
		primary: primary, secondary: secondary,
		maxPrimaryFailures: 2,
	}
	for i := 0; i < 3; i++ {
		text, _, _, err := f.Transcribe(ctx, "chunk.mp3")
		if err != nil || text != "groq" {
			t.Fatalf("call %d: err=%v text=%q", i, err, text)
		}
	}
	if primary.calls != 2 {
		t.Errorf("primary tried %d times, want 2 (breaker trips after 2)", primary.calls)
	}
	if secondary.calls != 3 {
		t.Errorf("secondary called %d times, want 3", secondary.calls)
	}
	// Once tripped it stays tripped for the run, even if the primary would recover.
	primary.err, primary.text = nil, "local"
	text, _, _, _ := f.Transcribe(ctx, "chunk.mp3")
	if text != "groq" || primary.calls != 2 {
		t.Errorf("after trip: text=%q primary.calls=%d (want groq, 2)", text, primary.calls)
	}
}

// ---------------------------------------------------------------------------
// Engine-aware tuning (timeout + chunk size)
// ---------------------------------------------------------------------------

func TestResolveSourceTimeout(t *testing.T) {
	// Default by engine: local gets the larger budget.
	if got := resolveSourceTimeout(engineGroq); got != remoteSourceTimeout {
		t.Errorf("groq: got %v, want %v", got, remoteSourceTimeout)
	}
	if got := resolveSourceTimeout(""); got != remoteSourceTimeout {
		t.Errorf("default(\"\"): got %v, want %v", got, remoteSourceTimeout)
	}
	if got := resolveSourceTimeout(engineLocal); got != localSourceTimeout {
		t.Errorf("local: got %v, want %v", got, localSourceTimeout)
	}
	if localSourceTimeout <= remoteSourceTimeout {
		t.Errorf("local timeout should exceed remote: local=%v remote=%v", localSourceTimeout, remoteSourceTimeout)
	}
	// Env override wins for any engine; invalid override is ignored.
	t.Setenv("SOURCE_TIMEOUT_MINUTES", "5")
	if got := resolveSourceTimeout(engineLocal); got != 5*time.Minute {
		t.Errorf("override: got %v, want 5m", got)
	}
	t.Setenv("SOURCE_TIMEOUT_MINUTES", "nope")
	if got := resolveSourceTimeout(engineGroq); got != remoteSourceTimeout {
		t.Errorf("bad override should be ignored: got %v", got)
	}
}

func TestChunkSecondsFor(t *testing.T) {
	if got := chunkSecondsFor(engineGroq); got != remoteChunkSeconds {
		t.Errorf("groq: got %d, want %d", got, remoteChunkSeconds)
	}
	if got := chunkSecondsFor(engineLocal); got != localChunkSeconds {
		t.Errorf("local: got %d, want %d", got, localChunkSeconds)
	}
	// Local chunks must be larger (no upload limit) so whisper.cpp reloads the
	// model fewer times.
	if localChunkSeconds <= remoteChunkSeconds {
		t.Errorf("local chunks should be larger: local=%d remote=%d", localChunkSeconds, remoteChunkSeconds)
	}
}

// TestApplyOverrides: CLI flags override the .env-sourced engine and batch size
// for a single run (empty engine / non-positive limit leave the config untouched),
// so the scheduled run keeps its .env defaults while a manual run can pick the
// engine and how many videos to process.
func TestApplyOverrides(t *testing.T) {
	base := Config{Engine: "groq", BatchSize: 25}

	if got := applyOverrides(base, "", 0); got != base {
		t.Errorf("no override should leave cfg unchanged, got %+v", got)
	}
	if got := applyOverrides(base, "local", 0); got.Engine != "local" || got.BatchSize != 25 {
		t.Errorf("engine-only override: got %+v", got)
	}
	if got := applyOverrides(base, "", 20); got.BatchSize != 20 || got.Engine != "groq" {
		t.Errorf("limit-only override: got %+v", got)
	}
	if got := applyOverrides(base, "local", 5); got.Engine != "local" || got.BatchSize != 5 {
		t.Errorf("both overrides: got %+v", got)
	}
	if got := applyOverrides(base, "", -3); got.BatchSize != 25 {
		t.Errorf("non-positive limit should be ignored: got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Groq transient-error retry (429 / 5xx)
// ---------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":     0,
		"  ":   0,
		"3":    3 * time.Second,
		"0.05": 50 * time.Millisecond,
		"-1":   0, // negative → ignored
		"soon": 0, // unparseable → ignored
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// writeTempAudio creates a small fake audio file for the Groq HTTP tests.
func writeTempAudio(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chunk_000.mp3")
	if err := os.WriteFile(p, []byte("fake audio bytes"), 0o600); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	return p
}

// TestGroqRetriesOn429ThenSucceeds: a 429 with Retry-After is retried, and the
// following 200 yields the transcript — a transient rate limit must not fail the
// video.
func TestGroqRetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0.02")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"olá","language":"pt","segments":[{"start":0,"end":1,"text":"olá"}]}`))
	}))
	defer srv.Close()

	g := &groqTranscriber{apiKey: "k", endpoint: srv.URL}
	text, lang, segs, err := g.Transcribe(context.Background(), writeTempAudio(t))
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if text != "olá" || lang != "pt" || len(segs) != 1 {
		t.Errorf("unexpected result: text=%q lang=%q segs=%+v", text, lang, segs)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 calls (429 then 200), got %d", got)
	}
}

// TestGroqGivesUpAfterMaxRetries: persistent 429 is retried up to the cap and
// then returns an error (so the video is recorded failed).
func TestGroqGivesUpAfterMaxRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0.01")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()

	g := &groqTranscriber{apiKey: "k", endpoint: srv.URL}
	if _, _, _, err := g.Transcribe(context.Background(), writeTempAudio(t)); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got, want := atomic.LoadInt32(&calls), int32(maxASRRetries+1); got != want {
		t.Errorf("expected %d calls, got %d", want, got)
	}
}

// TestGroqDoesNotRetryOn4xx: a non-429 client error (e.g. 400) is permanent and
// must not be retried.
func TestGroqDoesNotRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	g := &groqTranscriber{apiKey: "k", endpoint: srv.URL}
	if _, _, _, err := g.Transcribe(context.Background(), writeTempAudio(t)); err == nil {
		t.Fatal("expected error for 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("400 must not be retried; expected 1 call, got %d", got)
	}
}

// TestGroqClassifiesTransientVsPermanent: after exhausting retries, a persistent
// 429 is surfaced as a transient error (so SaveTranscript won't count it toward
// the retry cap), while a permanent 4xx is not.
func TestGroqClassifiesTransientVsPermanent(t *testing.T) {
	persistent429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0.01")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer persistent429.Close()

	g := &groqTranscriber{apiKey: "k", endpoint: persistent429.URL}
	_, _, _, err := g.Transcribe(context.Background(), writeTempAudio(t))
	if err == nil || !isTransient(err) {
		t.Fatalf("persistent 429 should classify transient, got err=%v transient=%v", err, isTransient(err))
	}

	badReq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer badReq.Close()

	g = &groqTranscriber{apiKey: "k", endpoint: badReq.URL}
	_, _, _, err = g.Transcribe(context.Background(), writeTempAudio(t))
	if err == nil || isTransient(err) {
		t.Fatalf("400 should classify permanent, got err=%v transient=%v", err, isTransient(err))
	}
}

// TestTranscribeSourceFlagsTransientASRFailure: transcribeSource propagates the
// transient signal onto the failed Transcript so the DB layer can keep the retry
// counter unchanged; a permanent ASR error leaves the flag false.
func TestTranscribeSourceFlagsTransientASRFailure(t *testing.T) {
	url := videoURL("vid")
	acq := newMockAcquirer()
	acq.chunks[url] = []AudioChunk{{Path: "/tmp/x/chunk_000.mp3"}}
	src := Source{Type: "youtube", Ref: url}

	transientTr := newMockTranscriber()
	transientTr.err = &transientError{errors.New("groq API error (status 429): rate limit")}
	got, _ := transcribeSource(context.Background(), acq, transientTr, groqModelName, src)
	if got.Status != statusFailed || !got.TransientFailure {
		t.Errorf("transient ASR error: status=%q transient=%v, want failed/true", got.Status, got.TransientFailure)
	}

	permTr := newMockTranscriber()
	permTr.err = errors.New("groq API error (status 400): bad request")
	got, _ = transcribeSource(context.Background(), acq, permTr, groqModelName, src)
	if got.Status != statusFailed || got.TransientFailure {
		t.Errorf("permanent ASR error: status=%q transient=%v, want failed/false", got.Status, got.TransientFailure)
	}
}

// TestGroqTransientOnContextCancelDuringBackoff: when a 429's backoff wait is cut
// short by context cancellation, the error must still classify as transient — the
// root cause is the rate limit, so it must not count toward the retry cap.
func TestGroqTransientOnContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10") // a backoff far longer than the ctx deadline
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	g := &groqTranscriber{apiKey: "k", endpoint: srv.URL}
	_, _, _, err := g.Transcribe(ctx, writeTempAudio(t))
	if err == nil || !isTransient(err) {
		t.Fatalf("a 429 backoff cut short by ctx cancellation must stay transient, got err=%v transient=%v", err, isTransient(err))
	}
}

// geminiResponse builds the API envelope that wraps the JSON transcript as text
// inside candidates/content/parts — the shape geminiTranscriber unwraps.
func geminiResponse(t *testing.T, language string, segs []Segment) []byte {
	t.Helper()
	type seg struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	}
	inner := struct {
		Language string `json:"language"`
		Segments []seg  `json:"segments"`
	}{Language: language}
	for _, s := range segs {
		inner.Segments = append(inner.Segments, seg{Start: s.Start, End: s.End, Text: s.Text})
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	envelope := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"parts": []any{map[string]any{"text": string(innerJSON)}},
			},
		}},
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

// TestGeminiRetriesOn429ThenSucceeds: the Gemini engine retries a transient 429
// with backoff (on par with Groq) and the following 200 yields the transcript.
func TestGeminiRetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0.02")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(geminiResponse(t, "pt", []Segment{{Start: 0, End: 1, Text: "olá"}}))
	}))
	defer srv.Close()

	g := &geminiTranscriber{apiKey: "k", endpoint: srv.URL}
	text, lang, segs, err := g.Transcribe(context.Background(), writeTempAudio(t))
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if text != "olá" || lang != "pt" || len(segs) != 1 {
		t.Errorf("unexpected result: text=%q lang=%q segs=%+v", text, lang, segs)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 calls (429 then 200), got %d", got)
	}
}

// TestGeminiClassifiesTransientVsPermanent: a persistent 429 is surfaced as a
// transient error after exhausting retries, while a 4xx is permanent.
func TestGeminiClassifiesTransientVsPermanent(t *testing.T) {
	persistent429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0.01")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer persistent429.Close()

	g := &geminiTranscriber{apiKey: "k", endpoint: persistent429.URL}
	_, _, _, err := g.Transcribe(context.Background(), writeTempAudio(t))
	if err == nil || !isTransient(err) {
		t.Fatalf("persistent 429 should classify transient, got err=%v transient=%v", err, isTransient(err))
	}

	badReq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer badReq.Close()

	g = &geminiTranscriber{apiKey: "k", endpoint: badReq.URL}
	_, _, _, err = g.Transcribe(context.Background(), writeTempAudio(t))
	if err == nil || isTransient(err) {
		t.Fatalf("400 should classify permanent, got err=%v transient=%v", err, isTransient(err))
	}
}

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// MockDatabase mirrors the SQL contract: idempotent on youtube_video_id, and
// PendingVideos excludes anything already transcribed with status 'done'.
type MockDatabase struct {
	pending     []VideoRef
	transcripts map[string]Transcript // keyed by dedup key
	segments    map[string][]Segment
	attempts    map[string]int // failed-attempt counter per dedup key
	savedSeq    map[string]int // last save order per key — mirrors updated_at
	saveCounter int            // monotonic save sequence
	saveErr     error
	pendingErr  error
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		transcripts: make(map[string]Transcript),
		segments:    make(map[string][]Segment),
		attempts:    make(map[string]int),
		savedSeq:    make(map[string]int),
	}
}

// dedupKey mirrors the uniqueness contract: youtube id when present, else the
// source ref (distinct per local/url source).
func dedupKey(t Transcript) string {
	if t.YoutubeVideoID != "" {
		return "yt:" + t.YoutubeVideoID
	}
	return "ref:" + t.SourceRef
}

func (m *MockDatabase) PendingVideos(ctx context.Context, limit int) ([]VideoRef, error) {
	if m.pendingErr != nil {
		return nil, m.pendingErr
	}
	// Mirror the SQL contract: exclude 'done', exclude failed videos past the
	// retry cap, and order never-attempted videos before previously-failed ones
	// so failures can't starve the backlog.
	var fresh, retry []VideoRef
	for _, ref := range m.pending {
		key := "yt:" + ref.YoutubeVideoID
		existing, ok := m.transcripts[key]
		switch {
		case ok && (existing.Status == statusDone || existing.Status == statusEmpty):
			continue // terminal: transcribed (with or without speech)
		case ok && existing.Status == statusFailed:
			if m.attempts[key] >= maxFailedAttempts {
				continue // exhausted retries — drop from the backlog
			}
			retry = append(retry, ref)
		default:
			fresh = append(fresh, ref)
		}
	}
	// Mirror the SQL ordering among failed rows: fewest attempts first, then the
	// least-recently-saved (updated_at ASC, round-robin) so a large backlog at the
	// same attempt_count can't starve itself.
	sort.SliceStable(retry, func(i, j int) bool {
		ki, kj := "yt:"+retry[i].YoutubeVideoID, "yt:"+retry[j].YoutubeVideoID
		if m.attempts[ki] != m.attempts[kj] {
			return m.attempts[ki] < m.attempts[kj]
		}
		return m.savedSeq[ki] < m.savedSeq[kj]
	})
	out := append(fresh, retry...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MockDatabase) SaveTranscript(ctx context.Context, t Transcript, segs []Segment) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	key := dedupKey(t)
	m.transcripts[key] = t // upsert: replaces, mirroring ON CONFLICT DO UPDATE
	m.segments[key] = segs // replace segments
	m.saveCounter++
	m.savedSeq[key] = m.saveCounter // mirror updated_at = CURRENT_TIMESTAMP
	// Mirror attempt_count: a permanent failure increments, a 'done'/'empty' save
	// resets to 0, and a transient failure (rate limit / 5xx) leaves it unchanged
	// so a daily-quota starvation never counts toward the retry cap.
	switch {
	case t.Status == statusFailed && t.TransientFailure:
		// leave unchanged
	case t.Status == statusFailed:
		m.attempts[key]++
	default:
		m.attempts[key] = 0
	}
	return nil
}

// MockAcquirer returns preconfigured chunks per source ref, or an error.
type MockAcquirer struct {
	chunks map[string][]AudioChunk
	err    error
}

func newMockAcquirer() *MockAcquirer {
	return &MockAcquirer{chunks: make(map[string][]AudioChunk)}
}

func (a *MockAcquirer) Acquire(ctx context.Context, src Source) ([]AudioChunk, error) {
	if a.err != nil {
		return nil, a.err
	}
	return a.chunks[src.Ref], nil
}

// MockTranscriber returns a preconfigured result per chunk path, or an error.
type mockResult struct {
	text     string
	language string
	segs     []Segment
}

type MockTranscriber struct {
	results map[string]mockResult
	err     error
}

func newMockTranscriber() *MockTranscriber {
	return &MockTranscriber{results: make(map[string]mockResult)}
}

func (tr *MockTranscriber) Transcribe(ctx context.Context, audioPath string) (string, string, []Segment, error) {
	if tr.err != nil {
		return "", "", nil, tr.err
	}
	r := tr.results[audioPath]
	return r.text, r.language, r.segs, nil
}

// TestPendingVideosPrioritizesFreshOverFailed: with limited capacity, a
// never-attempted video is returned before a previously-failed one, so a
// permanently-failing video cannot starve the backlog.
func TestPendingVideosPrioritizesFreshOverFailed(t *testing.T) {
	m := newMockDatabase()
	m.pending = []VideoRef{
		{YoutubeVideoID: "failed1", URL: videoURL("failed1")},
		{YoutubeVideoID: "fresh1", URL: videoURL("fresh1")},
	}
	m.transcripts["yt:failed1"] = Transcript{YoutubeVideoID: "failed1", Status: "failed"}

	got, err := m.PendingVideos(context.Background(), 1)
	if err != nil {
		t.Fatalf("PendingVideos failed: %v", err)
	}
	if len(got) != 1 || got[0].YoutubeVideoID != "fresh1" {
		t.Errorf("PendingVideos = %+v, want only [fresh1]", got)
	}
}

// TestPendingVideosExcludesExhaustedFailures: a video that has failed
// maxFailedAttempts times drops out of the backlog, so permanently-broken
// videos (deleted/private) stop being retried every run.
func TestPendingVideosExcludesExhaustedFailures(t *testing.T) {
	m := newMockDatabase()
	m.pending = []VideoRef{{YoutubeVideoID: "perma", URL: videoURL("perma")}}
	m.transcripts["yt:perma"] = Transcript{YoutubeVideoID: "perma", Status: "failed"}

	// One short of the cap: still retried.
	m.attempts["yt:perma"] = maxFailedAttempts - 1
	if got, _ := m.PendingVideos(context.Background(), 25); len(got) != 1 {
		t.Fatalf("below cap: want video retried, got %+v", got)
	}

	// At the cap: excluded.
	m.attempts["yt:perma"] = maxFailedAttempts
	if got, _ := m.PendingVideos(context.Background(), 25); len(got) != 0 {
		t.Errorf("at cap: want video excluded, got %+v", got)
	}
}

// TestPendingVideosFewerAttemptsFirst: among failed rows, the one with fewer
// attempts is retried before one closer to the cap.
func TestPendingVideosFewerAttemptsFirst(t *testing.T) {
	m := newMockDatabase()
	m.pending = []VideoRef{
		{YoutubeVideoID: "near", URL: videoURL("near")},
		{YoutubeVideoID: "far", URL: videoURL("far")},
	}
	m.transcripts["yt:near"] = Transcript{YoutubeVideoID: "near", Status: statusFailed}
	m.transcripts["yt:far"] = Transcript{YoutubeVideoID: "far", Status: statusFailed}
	m.attempts["yt:near"] = maxFailedAttempts - 1 // almost retired
	m.attempts["yt:far"] = 1                      // plenty of headroom

	got, err := m.PendingVideos(context.Background(), 25)
	if err != nil {
		t.Fatalf("PendingVideos: %v", err)
	}
	if len(got) != 2 || got[0].YoutubeVideoID != "far" {
		t.Errorf("want [far, near] (fewest attempts first), got %+v", got)
	}
}

// TestPendingVideosRoundRobinAmongFailed: a backlog of equally-failed rows (same
// attempt_count, e.g. quota-starved transient failures) is drained round-robin —
// the least-recently-tried first — so re-trying one row sends it to the back
// instead of the same rows being picked every run while others wait forever.
func TestPendingVideosRoundRobinAmongFailed(t *testing.T) {
	ctx := context.Background()
	m := newMockDatabase()
	m.pending = []VideoRef{
		{YoutubeVideoID: "a", URL: videoURL("a")},
		{YoutubeVideoID: "b", URL: videoURL("b")},
	}
	// Both transient-failed at attempt_count=0; 'a' was tried (saved) first.
	if err := m.SaveTranscript(ctx, Transcript{YoutubeVideoID: "a", SourceRef: videoURL("a"), Status: statusFailed, TransientFailure: true}, nil); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := m.SaveTranscript(ctx, Transcript{YoutubeVideoID: "b", SourceRef: videoURL("b"), Status: statusFailed, TransientFailure: true}, nil); err != nil {
		t.Fatalf("save b: %v", err)
	}

	got, _ := m.PendingVideos(ctx, 25)
	if len(got) != 2 || got[0].YoutubeVideoID != "a" {
		t.Fatalf("want [a, b] (a tried longest ago leads), got %+v", got)
	}

	// Re-try 'a' (saved again now): it becomes the most-recent, so 'b' should lead.
	if err := m.SaveTranscript(ctx, Transcript{YoutubeVideoID: "a", SourceRef: videoURL("a"), Status: statusFailed, TransientFailure: true}, nil); err != nil {
		t.Fatalf("re-save a: %v", err)
	}
	got, _ = m.PendingVideos(ctx, 25)
	if len(got) != 2 || got[0].YoutubeVideoID != "b" {
		t.Errorf("after retrying a, want [b, a], got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Fluent harness
// ---------------------------------------------------------------------------

type ScribeHarness struct {
	t          *testing.T
	db         *MockDatabase
	acq        *MockAcquirer
	tr         *MockTranscriber
	engineName string
}

func NewScribeHarness(t *testing.T) *ScribeHarness {
	return &ScribeHarness{
		t:          t,
		db:         newMockDatabase(),
		acq:        newMockAcquirer(),
		tr:         newMockTranscriber(),
		engineName: groqModelName,
	}
}

// WithPendingVideos registers the videos discovered from the collector tables.
func (h *ScribeHarness) WithPendingVideos(refs ...VideoRef) *ScribeHarness {
	h.db.pending = append(h.db.pending, refs...)
	return h
}

// WithChunks attaches the audio chunks a source ref will produce.
func (h *ScribeHarness) WithChunks(ref string, chunks ...AudioChunk) *ScribeHarness {
	h.acq.chunks[ref] = append(h.acq.chunks[ref], chunks...)
	return h
}

// WithTranscription sets the ASR result for a given chunk path.
func (h *ScribeHarness) WithTranscription(path, text, language string, segs ...Segment) *ScribeHarness {
	h.tr.results[path] = mockResult{text: text, language: language, segs: segs}
	return h
}

func (h *ScribeHarness) WithAcquireError(err error) *ScribeHarness {
	h.acq.err = err
	return h
}

func (h *ScribeHarness) WithTranscribeError(err error) *ScribeHarness {
	h.tr.err = err
	return h
}

func (h *ScribeHarness) Execute(ctx context.Context, limit int) error {
	return runBatch(ctx, h.db, h.acq, h.tr, h.engineName, limit)
}

func (h *ScribeHarness) get(videoID string) (Transcript, bool) {
	t, ok := h.db.transcripts["yt:"+videoID]
	return t, ok
}

func (h *ScribeHarness) AssertTranscriptCount(expected int) {
	if len(h.db.transcripts) != expected {
		h.t.Errorf("transcript count = %d, want %d", len(h.db.transcripts), expected)
	}
}

func (h *ScribeHarness) AssertStatus(videoID, status string) {
	t, ok := h.get(videoID)
	if !ok {
		h.t.Errorf("transcript %q not found", videoID)
		return
	}
	if t.Status != status {
		h.t.Errorf("transcript %q status = %q, want %q", videoID, t.Status, status)
	}
}

func (h *ScribeHarness) AssertText(videoID, text string) {
	t, ok := h.get(videoID)
	if !ok {
		h.t.Errorf("transcript %q not found", videoID)
		return
	}
	if t.Text != text {
		h.t.Errorf("transcript %q text = %q, want %q", videoID, t.Text, text)
	}
}

func (h *ScribeHarness) AssertLanguage(videoID, lang string) {
	t, ok := h.get(videoID)
	if !ok {
		h.t.Errorf("transcript %q not found", videoID)
		return
	}
	if t.Language != lang {
		h.t.Errorf("transcript %q language = %q, want %q", videoID, t.Language, lang)
	}
}

func (h *ScribeHarness) AssertSegmentCount(videoID string, expected int) {
	if got := len(h.db.segments["yt:"+videoID]); got != expected {
		h.t.Errorf("transcript %q segment count = %d, want %d", videoID, got, expected)
	}
}

func (h *ScribeHarness) AssertSegmentStart(videoID string, seq int, start float64) {
	segs := h.db.segments["yt:"+videoID]
	if seq >= len(segs) {
		h.t.Errorf("transcript %q has no segment %d", videoID, seq)
		return
	}
	if segs[seq].Start != start {
		h.t.Errorf("transcript %q segment %d start = %v, want %v", videoID, seq, segs[seq].Start, start)
	}
}

func (h *ScribeHarness) AssertAttemptCount(videoID string, expected int) {
	if got := h.db.attempts["yt:"+videoID]; got != expected {
		h.t.Errorf("transcript %q attempt_count = %d, want %d", videoID, got, expected)
	}
}

// ---------------------------------------------------------------------------
// Harness tests
// ---------------------------------------------------------------------------

// TestHarnessSingleVideoSingleChunk: the happy path for one short video.
func TestHarnessSingleVideoSingleChunk(t *testing.T) {
	url := videoURL("vid1")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "vid1", URL: url}).
		WithChunks(url, AudioChunk{Path: "/tmp/x/chunk_000.mp3", Offset: 0}).
		WithTranscription("/tmp/x/chunk_000.mp3", "olá mundo", "pt",
			Segment{Start: 0, End: 2, Text: "olá"}, Segment{Start: 2, End: 4, Text: "mundo"})

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertTranscriptCount(1)
	h.AssertStatus("vid1", "done")
	h.AssertLanguage("vid1", "pt")
	h.AssertText("vid1", "olá mundo")
	h.AssertSegmentCount("vid1", 2)
}

// TestHarnessMultiChunkStitchAndReindex: two chunks → text stitched and the
// second chunk's segment timestamps shifted by the chunk offset.
func TestHarnessMultiChunkStitchAndReindex(t *testing.T) {
	url := videoURL("long1")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "long1", URL: url}).
		WithChunks(url,
			AudioChunk{Path: "/tmp/y/chunk_000.mp3", Offset: 0},
			AudioChunk{Path: "/tmp/y/chunk_001.mp3", Offset: 600}).
		WithTranscription("/tmp/y/chunk_000.mp3", "primeira parte", "pt",
			Segment{Start: 0, End: 30, Text: "primeira parte"}).
		WithTranscription("/tmp/y/chunk_001.mp3", "segunda parte", "pt",
			Segment{Start: 10, End: 40, Text: "segunda parte"})

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertText("long1", "primeira parte\nsegunda parte")
	h.AssertSegmentCount("long1", 2)
	h.AssertSegmentStart("long1", 1, 610) // 10 + 600 offset
}

// TestHarnessLanguageByMajority: three chunks detect pt, en, pt → the transcript
// language is the majority (pt), not whatever the first chunk happened to report.
func TestHarnessLanguageByMajority(t *testing.T) {
	url := videoURL("mixed")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "mixed", URL: url}).
		WithChunks(url,
			AudioChunk{Path: "/tmp/m/chunk_000.mp3", Offset: 0},
			AudioChunk{Path: "/tmp/m/chunk_001.mp3", Offset: 600},
			AudioChunk{Path: "/tmp/m/chunk_002.mp3", Offset: 1200}).
		WithTranscription("/tmp/m/chunk_000.mp3", "a", "en", Segment{Start: 0, End: 1, Text: "a"}).
		WithTranscription("/tmp/m/chunk_001.mp3", "b", "pt", Segment{Start: 0, End: 1, Text: "b"}).
		WithTranscription("/tmp/m/chunk_002.mp3", "c", "pt", Segment{Start: 0, End: 1, Text: "c"})

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertLanguage("mixed", "pt") // 2×pt beats 1×en, despite en being first
}

// TestHarnessEmptyTranscriptMarkedEmpty: a video with no speech (no text, no
// segments) is saved as 'empty' — distinguishable from a real transcript and
// terminal, so PendingVideos does not keep reprocessing it.
func TestHarnessEmptyTranscriptMarkedEmpty(t *testing.T) {
	url := videoURL("silent")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "silent", URL: url}).
		WithChunks(url, AudioChunk{Path: "/tmp/s/chunk_000.mp3", Offset: 0}).
		WithTranscription("/tmp/s/chunk_000.mp3", "", "pt") // no text, no segments

	ctx := context.Background()
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertStatus("silent", statusEmpty)
	h.AssertSegmentCount("silent", 0)

	// Terminal: an empty result is not reprocessed on the next run.
	pending, err := h.db.PendingVideos(ctx, 25)
	if err != nil {
		t.Fatalf("PendingVideos: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("empty video still pending: %+v", pending)
	}
}

// TestHarnessAcquireFailureContinues: a download failure is recorded as failed
// and the batch carries on to the next video.
func TestHarnessAcquireFailureContinues(t *testing.T) {
	good := videoURL("good")
	h := NewScribeHarness(t).
		WithPendingVideos(
			VideoRef{YoutubeVideoID: "bad", URL: videoURL("bad")},
			VideoRef{YoutubeVideoID: "good", URL: good}).
		WithChunks(good, AudioChunk{Path: "/tmp/z/chunk_000.mp3", Offset: 0}).
		WithTranscription("/tmp/z/chunk_000.mp3", "ok", "en", Segment{Start: 0, End: 1, Text: "ok"}).
		WithAcquireError(errors.New("yt-dlp: Sign in to confirm you're not a bot"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	// Both rows persisted; one failed, one done.
	h.AssertTranscriptCount(2)
	h.AssertStatus("bad", "failed")
	h.AssertSegmentCount("bad", 0)
}

// TestHarnessFailedTranscriptHasNoSegments verifies a transcribe error is
// captured with the message and no segments.
func TestHarnessFailedTranscriptHasNoSegments(t *testing.T) {
	url := videoURL("vid1")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "vid1", URL: url}).
		WithChunks(url, AudioChunk{Path: "/tmp/x/chunk_000.mp3", Offset: 0}).
		WithTranscribeError(errors.New("groq 429"))

	if err := h.Execute(context.Background(), 25); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertStatus("vid1", "failed")
	h.AssertSegmentCount("vid1", 0)
	if tr, _ := h.get("vid1"); tr.Error == "" {
		t.Error("expected failed transcript to carry an error message")
	}
}

// TestHarnessIdempotentReRun: re-running does not re-transcribe an already-done
// video (PendingVideos excludes it), so the count stays stable.
func TestHarnessIdempotentReRun(t *testing.T) {
	url := videoURL("vid1")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "vid1", URL: url}).
		WithChunks(url, AudioChunk{Path: "/tmp/x/chunk_000.mp3", Offset: 0}).
		WithTranscription("/tmp/x/chunk_000.mp3", "olá", "pt", Segment{Start: 0, End: 1, Text: "olá"})

	ctx := context.Background()
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	h.AssertTranscriptCount(1)
	h.AssertSegmentCount("vid1", 1)

	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	h.AssertTranscriptCount(1) // still 1 — already done, excluded from pending
}

// TestHarnessExhaustedFailureLeavesBacklog: a video that fails every run is
// retried up to the cap and then drops out of the pending set entirely.
func TestHarnessExhaustedFailureLeavesBacklog(t *testing.T) {
	url := videoURL("perma")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "perma", URL: url}).
		WithAcquireError(errors.New("yt-dlp: Video unavailable"))

	ctx := context.Background()
	for i := 0; i < maxFailedAttempts; i++ {
		if err := h.Execute(ctx, 25); err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}
	h.AssertStatus("perma", "failed")

	// Retries exhausted → no longer pending.
	pending, err := h.db.PendingVideos(ctx, 25)
	if err != nil {
		t.Fatalf("PendingVideos: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("exhausted video still pending: %+v", pending)
	}
}

// TestHarnessBatchSizeLimit: only `limit` videos are processed per run.
func TestHarnessBatchSizeLimit(t *testing.T) {
	h := NewScribeHarness(t)
	for _, id := range []string{"a", "b", "c"} {
		u := videoURL(id)
		h.WithPendingVideos(VideoRef{YoutubeVideoID: id, URL: u}).
			WithChunks(u, AudioChunk{Path: "/tmp/" + id + "/chunk_000.mp3", Offset: 0}).
			WithTranscription("/tmp/"+id+"/chunk_000.mp3", id, "en", Segment{Start: 0, End: 1, Text: id})
	}

	if err := h.Execute(context.Background(), 2); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertTranscriptCount(2) // limited to 2 of 3
}

// TestTransientFailureDoesNotCountTowardCap: a video failing with a transient ASR
// error (rate limit / 5xx) is recorded failed but its attempt_count is never
// bumped, so it stays in the backlog and is retried indefinitely — exhausting a
// daily quota must not retire an otherwise-fine video.
func TestTransientFailureDoesNotCountTowardCap(t *testing.T) {
	url := videoURL("ratelimited")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "ratelimited", URL: url}).
		WithChunks(url, AudioChunk{Path: "/tmp/r/chunk_000.mp3", Offset: 0}).
		WithTranscribeError(&transientError{errors.New("groq API error (status 429): rate limit")})

	ctx := context.Background()
	// Fail well past the cap; a transient failure must never count toward it.
	for i := 0; i < maxFailedAttempts+2; i++ {
		if err := h.Execute(ctx, 25); err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}
	h.AssertStatus("ratelimited", statusFailed)
	h.AssertAttemptCount("ratelimited", 0)

	// Still pending despite many failures — not retired.
	pending, err := h.db.PendingVideos(ctx, 25)
	if err != nil {
		t.Fatalf("PendingVideos: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("rate-limited video should remain pending, got %+v", pending)
	}
}

// TestPermanentFailureCountsTowardCap: a permanent ASR error bumps attempt_count
// each run and the video is retired once the cap is reached.
func TestPermanentFailureCountsTowardCap(t *testing.T) {
	url := videoURL("badreq")
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "badreq", URL: url}).
		WithChunks(url, AudioChunk{Path: "/tmp/b/chunk_000.mp3", Offset: 0}).
		WithTranscribeError(errors.New("groq API error (status 400): bad request"))

	ctx := context.Background()
	for i := 0; i < maxFailedAttempts; i++ {
		if err := h.Execute(ctx, 25); err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}
	h.AssertStatus("badreq", statusFailed)
	h.AssertAttemptCount("badreq", maxFailedAttempts)

	pending, err := h.db.PendingVideos(ctx, 25)
	if err != nil {
		t.Fatalf("PendingVideos: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("permanently-failing video should be retired, got %+v", pending)
	}
}

// TestDoneSaveResetsAttemptCount: a previously-failed video (attempt_count > 0)
// that finally transcribes resets the counter to 0.
func TestDoneSaveResetsAttemptCount(t *testing.T) {
	url := videoURL("flap")
	chunk := "/tmp/f/chunk_000.mp3"
	h := NewScribeHarness(t).
		WithPendingVideos(VideoRef{YoutubeVideoID: "flap", URL: url}).
		WithChunks(url, AudioChunk{Path: chunk, Offset: 0}).
		WithTranscribeError(errors.New("groq API error (status 400): bad request"))

	ctx := context.Background()
	for i := 0; i < 2; i++ { // two permanent failures: counter climbs
		if err := h.Execute(ctx, 25); err != nil {
			t.Fatalf("failing run %d: %v", i, err)
		}
	}
	h.AssertAttemptCount("flap", 2)

	// Now it transcribes cleanly: the counter resets.
	h.tr.err = nil
	h.WithTranscription(chunk, "olá", "pt", Segment{Start: 0, End: 1, Text: "olá"})
	if err := h.Execute(ctx, 25); err != nil {
		t.Fatalf("success run: %v", err)
	}
	h.AssertStatus("flap", statusDone)
	h.AssertAttemptCount("flap", 0)
}

// ---------------------------------------------------------------------------
// Integration test (real SQL) — opt-in via SCRIBE_TEST_DATABASE_URL.
//
// Unit tests above use MockDatabase, which cannot reproduce SQL three-valued
// (NULL) logic. This test runs the actual PendingVideos query against Postgres
// to guard the NULL edge cases a mock will always miss:
//
//   - a video with NO transcript row MUST be returned (the bug case: a naive
//     NOT (status='failed' AND attempt_count>=cap) evaluates to NULL when
//     t.status IS NULL, silently dropping every never-attempted video);
//   - a video with status='done' MUST be excluded;
//   - a video with status='empty' MUST be excluded;
//   - a video with status='failed' and attempt_count < cap MUST be returned;
//   - a video with status='failed' and attempt_count >= cap MUST be excluded.
//
// Skipped unless SCRIBE_TEST_DATABASE_URL points at a throwaway Postgres.
// ---------------------------------------------------------------------------

func TestPendingVideosIntegration(t *testing.T) {
	dsn := os.Getenv("SCRIBE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SCRIBE_TEST_DATABASE_URL to a throwaway Postgres to run the integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	const schema = "scribe_it_test"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %.60q: %v", sql, err)
		}
	}

	exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	exec("SET search_path TO " + schema)

	exec(`CREATE TABLE channel_videos (
		youtube_video_id TEXT PRIMARY KEY,
		url              TEXT NOT NULL
	)`)
	exec(`CREATE TABLE playlist_videos (
		youtube_video_id TEXT NOT NULL,
		url              TEXT NOT NULL
	)`)
	exec(`CREATE TABLE transcripts (
		id               SERIAL PRIMARY KEY,
		source_type      TEXT NOT NULL DEFAULT 'youtube',
		youtube_video_id TEXT UNIQUE,
		source_ref       TEXT NOT NULL DEFAULT '',
		engine           TEXT NOT NULL DEFAULT 'groq/whisper-large-v3',
		status           TEXT NOT NULL DEFAULT 'done',
		error            TEXT,
		attempt_count    INT  NOT NULL DEFAULT 0,
		created_at       TIMESTAMPTZ DEFAULT now(),
		updated_at       TIMESTAMPTZ DEFAULT now()
	)`)

	// Helper: insert a video into channel_videos and optionally its transcript.
	insertVideo := func(id string, status *string, attemptCount int) {
		exec("INSERT INTO channel_videos (youtube_video_id, url) VALUES ($1, $2)",
			id, "https://youtu.be/"+id)
		if status != nil {
			exec(`INSERT INTO transcripts (youtube_video_id, status, attempt_count)
				  VALUES ($1, $2, $3)`, id, *status, attemptCount)
		}
	}
	sp := func(s string) *string { return &s }

	insertVideo("never_attempted", nil, 0)                        // no row — MUST be returned
	insertVideo("done_video", sp("done"), 0)                      // MUST be excluded
	insertVideo("empty_video", sp("empty"), 0)                    // MUST be excluded
	insertVideo("failed_under_cap", sp("failed"), maxFailedAttempts-1) // MUST be returned
	insertVideo("failed_at_cap", sp("failed"), maxFailedAttempts)      // MUST be excluded

	db := &pgxDatabase{pool: pool}
	refs, err := db.PendingVideos(ctx, 100)
	if err != nil {
		t.Fatalf("PendingVideos: %v", err)
	}

	got := make(map[string]bool, len(refs))
	for _, r := range refs {
		got[r.YoutubeVideoID] = true
	}

	mustInclude := []string{"never_attempted", "failed_under_cap"}
	mustExclude := []string{"done_video", "empty_video", "failed_at_cap"}

	for _, id := range mustInclude {
		if !got[id] {
			t.Errorf("PendingVideos missing %q (should be pending)", id)
		}
	}
	for _, id := range mustExclude {
		if got[id] {
			t.Errorf("PendingVideos includes %q (should be excluded)", id)
		}
	}
}
