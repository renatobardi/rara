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

	// Unknown engine → error.
	if _, _, err := NewTranscriber(Config{Engine: "whisperx", GroqAPIKey: "k"}); err == nil {
		t.Error("expected error for unknown engine")
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
		case ok && existing.Status == "done":
			continue
		case ok && existing.Status == "failed":
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
	// Mirror attempt_count: increment on failure, reset on success.
	if t.Status == "failed" {
		m.attempts[key]++
	} else {
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
