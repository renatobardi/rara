package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	addon "rara-addon"
)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestValidateFetchURL(t *testing.T) {
	for _, ok := range []string{"https://cdn.example.com/ep.mp3", "http://cdn.example.com/ep.mp3"} {
		if err := validateFetchURL(ok); err != nil {
			t.Errorf("validateFetchURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://x/y.mp3", "gopher://x", "data:audio/mp3;base64,AAAA"} {
		if err := validateFetchURL(bad); err == nil {
			t.Errorf("validateFetchURL(%q) = nil, want a rejection", bad)
		}
	}
}

func TestDownloadDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("AUDIOBYTES"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "audio.bin")
	got, err := downloadDirect(context.Background(), srv.URL, dest)
	if err != nil || got != dest {
		t.Fatalf("downloadDirect = %q, %v", got, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "AUDIOBYTES" {
		t.Errorf("downloaded content = %q, want AUDIOBYTES", b)
	}

	// A non-200 response is an error (no file passed on to ffmpeg).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bad.Close()
	if _, err := downloadDirect(context.Background(), bad.URL, dest); err == nil {
		t.Error("expected an error on a non-200 response")
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

// mockStore is the in-memory ScribeStore for handler tests: saved transcripts keyed by dedupKey,
// a guid->enclosure map for the asr-direct-audio path, and a synthetic returned id. Error fields
// force each seam to fail.
type mockStore struct {
	transcripts map[string]Transcript // keyed by dedupKey
	segments    map[string][]Segment
	enclosures  map[string]string // guid -> enclosure url
	saveErr     error
	enclErr     error
	saved       int // monotonic save count -> the returned id
}

func newMockStore() *mockStore {
	return &mockStore{
		transcripts: make(map[string]Transcript),
		segments:    make(map[string][]Segment),
		enclosures:  make(map[string]string),
	}
}

// dedupKey mirrors the uniqueness contract: youtube id when present, else the
// (source_type, source_ref) pair (distinct per podcast/url source).
func dedupKey(t Transcript) string {
	if t.YoutubeVideoID != "" {
		return "yt:" + t.YoutubeVideoID
	}
	return t.SourceType + ":" + t.SourceRef
}

func (m *mockStore) SaveTranscript(ctx context.Context, t Transcript, segs []Segment) (int, error) {
	if m.saveErr != nil {
		return 0, m.saveErr
	}
	key := dedupKey(t)
	m.transcripts[key] = t // upsert: replaces, mirroring the SQL idempotency
	m.segments[key] = segs
	m.saved++
	return 1000 + m.saved, nil // first save -> 1001 (deterministic OutputRef)
}

func (m *mockStore) EnclosureURL(ctx context.Context, guid string) (string, bool, error) {
	if m.enclErr != nil {
		return "", false, m.enclErr
	}
	u, ok := m.enclosures[guid]
	return u, ok, nil
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

// ---------------------------------------------------------------------------
// transcribeSource — the engine/db-agnostic transcription core (driven directly)
// ---------------------------------------------------------------------------

func ytSource(id string) Source { return Source{Type: "youtube", Ref: videoURL(id)} }

// TestTranscribeSourceSingleChunk: the happy path for one short source — text, language, segments,
// and the youtube_video_id parsed from the watch URL (an 11-char id).
func TestTranscribeSourceSingleChunk(t *testing.T) {
	const vid = "dQw4w9WgXcQ"
	src := ytSource(vid)
	acq, tr := oneChunk(src.Ref, "/tmp/x/chunk_000.mp3", "olá mundo", "pt",
		Segment{Start: 0, End: 2, Text: "olá"}, Segment{Start: 2, End: 4, Text: "mundo"})

	got, segs := transcribeSource(context.Background(), acq, tr, groqModelName, src)
	if got.Status != statusDone || got.Language != "pt" || got.Text != "olá mundo" || len(segs) != 2 {
		t.Errorf("transcribeSource = %+v, segs=%d", got, len(segs))
	}
	if got.YoutubeVideoID != vid {
		t.Errorf("youtube id = %q, want %q", got.YoutubeVideoID, vid)
	}
}

// TestTranscribeSourceMultiChunkStitchAndReindex: two chunks → text stitched and the second
// chunk's segment timestamps shifted by the chunk offset.
func TestTranscribeSourceMultiChunkStitchAndReindex(t *testing.T) {
	src := ytSource("long1")
	acq := newMockAcquirer()
	acq.chunks[src.Ref] = []AudioChunk{{Path: "/tmp/y/chunk_000.mp3", Offset: 0}, {Path: "/tmp/y/chunk_001.mp3", Offset: 600}}
	tr := newMockTranscriber()
	tr.results["/tmp/y/chunk_000.mp3"] = mockResult{text: "primeira parte", language: "pt", segs: []Segment{{Start: 0, End: 30, Text: "primeira parte"}}}
	tr.results["/tmp/y/chunk_001.mp3"] = mockResult{text: "segunda parte", language: "pt", segs: []Segment{{Start: 10, End: 40, Text: "segunda parte"}}}

	got, segs := transcribeSource(context.Background(), acq, tr, groqModelName, src)
	if got.Text != "primeira parte\nsegunda parte" {
		t.Errorf("stitched text = %q", got.Text)
	}
	if len(segs) != 2 || segs[1].Start != 610 { // 10 + 600 offset
		t.Errorf("reindex: segs=%+v", segs)
	}
}

// TestTranscribeSourceLanguageByMajority: pt, en, pt → majority pt (not the first chunk's en).
func TestTranscribeSourceLanguageByMajority(t *testing.T) {
	src := ytSource("mixed")
	acq := newMockAcquirer()
	acq.chunks[src.Ref] = []AudioChunk{{Path: "/m/0.mp3", Offset: 0}, {Path: "/m/1.mp3", Offset: 600}, {Path: "/m/2.mp3", Offset: 1200}}
	tr := newMockTranscriber()
	tr.results["/m/0.mp3"] = mockResult{text: "a", language: "en", segs: []Segment{{Start: 0, End: 1, Text: "a"}}}
	tr.results["/m/1.mp3"] = mockResult{text: "b", language: "pt", segs: []Segment{{Start: 0, End: 1, Text: "b"}}}
	tr.results["/m/2.mp3"] = mockResult{text: "c", language: "pt", segs: []Segment{{Start: 0, End: 1, Text: "c"}}}

	got, _ := transcribeSource(context.Background(), acq, tr, groqModelName, src)
	if got.Language != "pt" {
		t.Errorf("language = %q, want pt (majority)", got.Language)
	}
}

// TestTranscribeSourceEmptyMarkedEmpty: no text + no segments → status 'empty'.
func TestTranscribeSourceEmptyMarkedEmpty(t *testing.T) {
	src := ytSource("silent")
	acq, tr := oneChunk(src.Ref, "/s/0.mp3", "", "pt")

	got, segs := transcribeSource(context.Background(), acq, tr, groqModelName, src)
	if got.Status != statusEmpty || len(segs) != 0 {
		t.Errorf("empty: status=%q segs=%d", got.Status, len(segs))
	}
}

// TestTranscribeSourceAcquireFailure: a download failure is captured as a failed Transcript with
// no segments (the handler then persists it and requeues).
func TestTranscribeSourceAcquireFailure(t *testing.T) {
	acq := newMockAcquirer()
	acq.err = errors.New("yt-dlp: Sign in to confirm you're not a bot")
	got, segs := transcribeSource(context.Background(), acq, newMockTranscriber(), groqModelName, ytSource("bad"))
	if got.Status != statusFailed || got.Error == "" || segs != nil {
		t.Errorf("acquire failure: %+v segs=%v", got, segs)
	}
}

// TestTranscribeSourcePermanentASRFailure: a permanent ASR error fails the source (no segments)
// and is NOT flagged transient.
func TestTranscribeSourcePermanentASRFailure(t *testing.T) {
	src := ytSource("vid1")
	acq, tr := oneChunk(src.Ref, "/x/0.mp3", "ignored", "en")
	tr.err = errors.New("groq API error (status 400): bad request")
	got, _ := transcribeSource(context.Background(), acq, tr, groqModelName, src)
	if got.Status != statusFailed || got.TransientFailure {
		t.Errorf("permanent ASR failure: status=%q transient=%v", got.Status, got.TransientFailure)
	}
}

// ---------------------------------------------------------------------------
// transcribeHandler — the bridge-total claim-worker domain (mock store + fakes)
// ---------------------------------------------------------------------------

func runHandler(store *mockStore, acq *MockAcquirer, tr *MockTranscriber, provider, sourceRef string) (addon.Result, error) {
	h := transcribeHandler(store, acq, tr, groqModelName, provider)
	return h(context.Background(), addon.Item{SourceRef: sourceRef}, addon.Step{Seq: 1})
}

// oneChunk wires a mock acquirer + transcriber for a source that produces a single chunk with the
// given ASR result (factored out so the per-test setup isn't repeated boilerplate).
func oneChunk(srcRef, chunkPath, text, lang string, segs ...Segment) (*MockAcquirer, *MockTranscriber) {
	acq := newMockAcquirer()
	acq.chunks[srcRef] = []AudioChunk{{Path: chunkPath, Offset: 0}}
	tr := newMockTranscriber()
	tr.results[chunkPath] = mockResult{text: text, language: lang, segs: segs}
	return acq, tr
}

// TestHandlerYouTubeHappyPath: asr-youtube builds the watch URL from the video id, transcribes, and
// saves a transcript keyed by youtube_video_id; OutputRef is the new row id.
func TestHandlerYouTubeHappyPath(t *testing.T) {
	store := newMockStore()
	acq, tr := oneChunk(videoURL("vid1"), "/x/0.mp3", "olá", "pt", Segment{Start: 0, End: 1, Text: "olá"})

	res, err := runHandler(store, acq, tr, provASRYouTube, "vid1")
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.OutputRef != "1001" || res.Filtered {
		t.Errorf("result = %+v, want OutputRef 1001, Filtered false", res)
	}
	got, ok := store.transcripts["yt:vid1"]
	if !ok || got.Status != statusDone || got.YoutubeVideoID != "vid1" || got.SourceType != "youtube" || got.SourceRef != videoURL("vid1") {
		t.Errorf("saved transcript = %+v (ok=%v)", got, ok)
	}
}

// TestHandlerDirectAudioReKeysToPodcast: asr-direct-audio resolves the enclosure URL from
// podcast_episodes, transcribes it, but keys the transcript on the spine's GUID + lane=podcast (NOT
// the enclosure URL), so the downstream gate/distill lookups chain on the same GUID.
func TestHandlerDirectAudioReKeysToPodcast(t *testing.T) {
	store := newMockStore()
	store.enclosures["guid-42"] = "https://cdn.example.com/ep42.mp3"
	acq, tr := oneChunk("https://cdn.example.com/ep42.mp3", "/p/0.mp3", "hello", "en", Segment{Start: 0, End: 1, Text: "hello"})

	res, err := runHandler(store, acq, tr, provASRDirectAudio, "guid-42")
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.OutputRef == "" {
		t.Error("expected an OutputRef")
	}
	got, ok := store.transcripts["podcast:guid-42"]
	if !ok || got.SourceType != lanePodcast || got.SourceRef != "guid-42" || got.YoutubeVideoID != "" {
		t.Errorf("re-keyed transcript = %+v (ok=%v), want (podcast, guid-42, no yt id)", got, ok)
	}
}

// TestHandlerEmptyIsFiltered: a no-speech transcript is benign no-content — the item is curated out.
func TestHandlerEmptyIsFiltered(t *testing.T) {
	store := newMockStore()
	acq, tr := oneChunk(videoURL("silent"), "/s/0.mp3", "", "pt")

	res, err := runHandler(store, acq, tr, provASRYouTube, "silent")
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.Filtered {
		t.Error("empty transcript should be Filtered")
	}
}

// TestHandlerFailedIsRetryableAndPersisted: a download/ASR failure persists a failed row (for
// observability) and surfaces as addon.ErrRetryable so the SDK requeues.
func TestHandlerFailedIsRetryableAndPersisted(t *testing.T) {
	store := newMockStore()
	acq := newMockAcquirer()
	acq.err = errors.New("yt-dlp: Video unavailable")

	_, err := runHandler(store, acq, newMockTranscriber(), provASRYouTube, "vid1")
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("err = %v, want wrapping addon.ErrRetryable", err)
	}
	if got, ok := store.transcripts["yt:vid1"]; !ok || got.Status != statusFailed {
		t.Errorf("expected a persisted failed row, got %+v (ok=%v)", got, ok)
	}
}

// TestHandlerEnclosureNotReadyIsRetryable: a missing podcast_episodes row is transient (the
// collector may lag) → ErrRetryable, and nothing is transcribed or saved.
func TestHandlerEnclosureNotReadyIsRetryable(t *testing.T) {
	store := newMockStore() // no enclosure registered
	_, err := runHandler(store, newMockAcquirer(), newMockTranscriber(), provASRDirectAudio, "guid-missing")
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("err = %v, want wrapping addon.ErrRetryable", err)
	}
	if len(store.transcripts) != 0 {
		t.Errorf("nothing should be saved, got %d", len(store.transcripts))
	}
}

// TestHandlerRejectsNonHttpEnclosure: a feed-supplied enclosure with a non-http(s) scheme is
// rejected before any download (the SSRF/local-file guard) — a terminal error, nothing saved.
func TestHandlerRejectsNonHttpEnclosure(t *testing.T) {
	store := newMockStore()
	store.enclosures["guid-evil"] = "file:///etc/passwd"
	_, err := runHandler(store, newMockAcquirer(), newMockTranscriber(), provASRDirectAudio, "guid-evil")
	if err == nil || errors.Is(err, addon.ErrRetryable) {
		t.Errorf("err = %v, want a terminal (non-retryable) rejection", err)
	}
	if len(store.transcripts) != 0 {
		t.Errorf("nothing should be saved, got %d", len(store.transcripts))
	}
}

// TestHandlerUnknownProvider: an unrecognized provider is a config error (terminal, not retryable).
func TestHandlerUnknownProvider(t *testing.T) {
	_, err := runHandler(newMockStore(), newMockAcquirer(), newMockTranscriber(), "asr-bogus", "x")
	if err == nil || errors.Is(err, addon.ErrRetryable) {
		t.Errorf("err = %v, want a terminal (non-retryable) error", err)
	}
}

// TestHandlerSaveErrorPropagates: a store write failure propagates.
func TestHandlerSaveErrorPropagates(t *testing.T) {
	store := newMockStore()
	store.saveErr = errors.New("neon write timeout")
	acq, tr := oneChunk(videoURL("vid1"), "/x/0.mp3", "ok", "en", Segment{Start: 0, End: 1, Text: "ok"})

	_, err := runHandler(store, acq, tr, provASRYouTube, "vid1")
	if err == nil || !strings.Contains(err.Error(), "neon write timeout") {
		t.Errorf("err = %v, want the save error propagated", err)
	}
}

// ---------------------------------------------------------------------------
// Integration test (real SQL) — opt-in via SCRIBE_TEST_DATABASE_URL.
//
// Exercises the app's domain DB (appDB) against Postgres: SaveTranscript returns the row id and is
// idempotent — YouTube via the youtube_video_id UNIQUE key; podcast/non-youtube via the explicit
// pre-delete on (source_type, source_ref), since a NULL youtube_video_id does not dedup — and
// EnclosureURL resolves the podcast enclosure. Skipped unless SCRIBE_TEST_DATABASE_URL points at a
// throwaway Postgres.
// ---------------------------------------------------------------------------

func TestSaveTranscriptIntegration(t *testing.T) {
	dsn := os.Getenv("SCRIBE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SCRIBE_TEST_DATABASE_URL to a throwaway Postgres to run the integration test")
	}
	ctx := context.Background()

	const schema = "scribe_it_test"
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer boot.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := boot.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %.60q: %v", sql, err)
		}
	}
	exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, _ = boot.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	exec("SET search_path TO " + schema)

	exec(`CREATE TABLE transcripts (
		id SERIAL PRIMARY KEY, source_type TEXT NOT NULL, youtube_video_id TEXT UNIQUE,
		source_ref TEXT NOT NULL, language TEXT, engine TEXT NOT NULL, transcript TEXT,
		duration_seconds INT, status TEXT NOT NULL, error TEXT, attempt_count INT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ DEFAULT now())`)
	exec(`CREATE TABLE transcript_segments (
		id SERIAL PRIMARY KEY, transcript_id INT NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
		seq INT NOT NULL, start_seconds NUMERIC(10,3) NOT NULL, end_seconds NUMERIC(10,3) NOT NULL, text TEXT NOT NULL)`)
	exec(`CREATE TABLE podcast_episodes (guid TEXT PRIMARY KEY, enclosure_url TEXT NOT NULL)`)
	exec(`INSERT INTO podcast_episodes (guid, enclosure_url) VALUES ('g1', 'https://cdn/ep.mp3')`)

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

	// EnclosureURL: found and not-found.
	if u, ok, err := db.EnclosureURL(ctx, "g1"); err != nil || !ok || u != "https://cdn/ep.mp3" {
		t.Errorf("EnclosureURL(g1) = %q,%v,%v", u, ok, err)
	}
	if _, ok, _ := db.EnclosureURL(ctx, "nope"); ok {
		t.Error("EnclosureURL(nope) should not be found")
	}

	assertCount := func(label, where string, want int) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM transcripts WHERE "+where).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		if n != want {
			t.Errorf("%s: count = %d, want %d", label, n, want)
		}
	}

	// YouTube: idempotent on youtube_video_id — two saves keep one row, id stable.
	yt := Transcript{SourceType: "youtube", YoutubeVideoID: "vid1", SourceRef: videoURL("vid1"), Engine: "groq/whisper-large-v3", Text: "a", Status: statusDone}
	id1, err := db.SaveTranscript(ctx, yt, []Segment{{Start: 0, End: 1, Text: "a"}})
	if err != nil {
		t.Fatalf("save yt 1: %v", err)
	}
	yt.Text = "b"
	id2, err := db.SaveTranscript(ctx, yt, []Segment{{Start: 0, End: 2, Text: "b"}})
	if err != nil {
		t.Fatalf("save yt 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("youtube re-save changed id: %d -> %d (should upsert in place)", id1, id2)
	}
	assertCount("youtube", "youtube_video_id = 'vid1'", 1)

	// Podcast (non-youtube): two saves for the same (source_type, source_ref) keep ONE row thanks to
	// the pre-delete (a NULL youtube_video_id would otherwise not dedup).
	pod := Transcript{SourceType: lanePodcast, SourceRef: "g1", Engine: "groq/whisper-large-v3", Text: "x", Status: statusDone}
	if _, err := db.SaveTranscript(ctx, pod, []Segment{{Start: 0, End: 1, Text: "x"}}); err != nil {
		t.Fatalf("save pod 1: %v", err)
	}
	if _, err := db.SaveTranscript(ctx, pod, []Segment{{Start: 0, End: 1, Text: "x"}}); err != nil {
		t.Fatalf("save pod 2: %v", err)
	}
	assertCount("podcast", "source_type = 'podcast' AND source_ref = 'g1'", 1)
}
