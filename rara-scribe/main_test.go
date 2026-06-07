package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
	saveErr     error
	pendingErr  error
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		transcripts: make(map[string]Transcript),
		segments:    make(map[string][]Segment),
		attempts:    make(map[string]int),
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
	// Mirror attempt_count: only a PERMANENT failure increments the cap; a
	// transient failure (429/5xx/empty download) leaves it unchanged; a success
	// (done/empty) resets it.
	switch {
	case t.Status == statusFailed && isPermanentError(t.Error):
		m.attempts[key]++
	case t.Status == statusFailed:
		// transient — unchanged
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

func TestIsPermanentError(t *testing.T) {
	permanent := []string{
		"yt-dlp failed: ERROR: [youtube] x: Video unavailable",
		"This video is no longer available because the YouTube account associated with this video has been terminated.",
		"ERROR: [youtube] x: Private video. Sign in if you've been granted access",
		"ERROR: Join this channel to get access to members-only content",
		"this video has been removed by the uploader",
	}
	for _, e := range permanent {
		if !isPermanentError(e) {
			t.Errorf("want permanent: %q", e)
		}
	}
	transient := []string{
		"groq API error (status 429): rate_limit_exceeded",
		"groq API error (status 502): <html>bad gateway",
		"groq API error (status 520): error code 520",
		"ffmpeg failed: exit status 234: Error opening output files: Invalid argument",
		"yt-dlp failed: signal: killed: [download]",
		"This live event will begin in 2 days",
		"write tcp ...: broken pipe",
		"some brand-new error we have never seen", // unknown → transient (never retire a good video)
		"",
	}
	for _, e := range transient {
		if isPermanentError(e) {
			t.Errorf("want transient: %q", e)
		}
	}
}

// TestAttemptCountOnlyCountsPermanent: a transient failure (429/5xx/empty
// download) must NOT increment the retry cap — only a permanent failure
// (unavailable/private/terminated) does; a success resets it. This keeps the
// nightly Groq run from retiring 429-throttled videos after a few nights.
func TestAttemptCountOnlyCountsPermanent(t *testing.T) {
	ctx := context.Background()
	m := newMockDatabase()
	base := Transcript{YoutubeVideoID: "v1", SourceType: "youtube", SourceRef: videoURL("v1")}
	key := dedupKey(base)

	// Three transient (429) failures → counter stays 0 (never retired).
	transient := base
	transient.Status, transient.Error = statusFailed, "groq API error (status 429): rate_limit_exceeded"
	for i := 0; i < 3; i++ {
		_ = m.SaveTranscript(ctx, transient, nil)
	}
	if m.attempts[key] != 0 {
		t.Errorf("transient failures incremented cap: got %d, want 0", m.attempts[key])
	}

	// A permanent failure → increments.
	perm := base
	perm.Status, perm.Error = statusFailed, "ERROR: [youtube] v1: Video unavailable"
	_ = m.SaveTranscript(ctx, perm, nil)
	if m.attempts[key] != 1 {
		t.Errorf("permanent failure not counted: got %d, want 1", m.attempts[key])
	}

	// A success resets the counter.
	done := base
	done.Status = statusDone
	_ = m.SaveTranscript(ctx, done, nil)
	if m.attempts[key] != 0 {
		t.Errorf("success did not reset: got %d, want 0", m.attempts[key])
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
