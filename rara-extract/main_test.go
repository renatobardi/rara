package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	addon "rara-addon"
)

// ---------------------------------------------------------------------------
// cleanEmailText — pure, zero I/O. Deterministic noise removal: HTML, the
// "-- " signature, and quoted-reply history.
// ---------------------------------------------------------------------------

func TestCleanEmailTextPlain(t *testing.T) {
	in := "Hi team,\n\nShipping the fix today.\n\nThanks"
	if got := cleanEmailText(in); got != in {
		t.Errorf("plain text should pass through unchanged:\n got %q\nwant %q", got, in)
	}
}

func TestCleanEmailTextStripsHTML(t *testing.T) {
	in := `<html><head><style>p{color:red}</style></head><body><p>Hello &amp; welcome</p><p>Second line</p><script>evil()</script></body></html>`
	got := cleanEmailText(in)
	if strings.Contains(got, "<") || strings.Contains(got, "color:red") || strings.Contains(got, "evil()") {
		t.Errorf("HTML/script/style not stripped: %q", got)
	}
	if !strings.Contains(got, "Hello & welcome") || !strings.Contains(got, "Second line") {
		t.Errorf("body text lost: %q", got)
	}
}

func TestCleanEmailTextDropsSignature(t *testing.T) {
	in := "The actual message.\n\n-- \nRenato Bardi\nSoftware Architect\nphone: 555-1234"
	got := cleanEmailText(in)
	if strings.Contains(got, "Software Architect") || strings.Contains(got, "555-1234") {
		t.Errorf("signature not removed: %q", got)
	}
	if !strings.Contains(got, "The actual message.") {
		t.Errorf("body lost: %q", got)
	}
}

func TestCleanEmailTextDropsQuotedReply(t *testing.T) {
	in := "My reply on top.\n\nOn Mon, 9 Jun 2025, Alice <a@x.com> wrote:\n> previous message\n> more quoted text"
	got := cleanEmailText(in)
	if strings.Contains(got, "previous message") || strings.Contains(got, "Alice") {
		t.Errorf("quoted reply not removed: %q", got)
	}
	if !strings.Contains(got, "My reply on top.") {
		t.Errorf("reply body lost: %q", got)
	}
}

func TestCleanEmailTextEmptyResult(t *testing.T) {
	// Pure signature + quote -> nothing meaningful left.
	in := "> only quoted\n> text here\n-- \nsig"
	if got := cleanEmailText(in); got != "" {
		t.Errorf("an all-quote/sig email should clean to empty, got %q", got)
	}
}

// TestCleanEmailTextDeterministic: the cleaner is a pure function — repeated calls on the same
// input yield the exact same output (no map iteration, time, or other nondeterminism leaks in).
func TestCleanEmailTextDeterministic(t *testing.T) {
	in := `<div><p>On topic &amp; tidy</p></div>` + "\n\nOn Tue, Bob wrote:\n> noise\n-- \nsig"
	first := cleanEmailText(in)
	for i := 0; i < 100; i++ {
		if got := cleanEmailText(in); got != first {
			t.Fatalf("clean is non-deterministic: call %d = %q, want %q", i, got, first)
		}
	}
}

// ---------------------------------------------------------------------------
// cleanPostText — pure, zero I/O. Lighter than email (no signature/quote): HTML
// strip if any, collapse blank runs, trim.
// ---------------------------------------------------------------------------

func TestCleanPostTextPlainPassThrough(t *testing.T) {
	in := "Shipping a control plane today.\n\nThe contract is the table."
	if got := cleanPostText(in); got != in {
		t.Errorf("plain post should pass through:\n got %q\nwant %q", got, in)
	}
}

func TestCleanPostTextStripsHTML(t *testing.T) {
	// A future Bright Data collector may yield HTML; the cleaner must reduce it to text.
	in := `<div><p>Hello &amp; welcome</p><p>Second line</p><script>evil()</script></div>`
	got := cleanPostText(in)
	if strings.Contains(got, "<") || strings.Contains(got, "evil()") {
		t.Errorf("HTML/script not stripped: %q", got)
	}
	if !strings.Contains(got, "Hello & welcome") || !strings.Contains(got, "Second line") {
		t.Errorf("body text lost: %q", got)
	}
}

func TestCleanPostTextCollapsesBlankRuns(t *testing.T) {
	in := "Line one.\n\n\n\n\nLine two.   \n"
	got := cleanPostText(in)
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank runs not collapsed: %q", got)
	}
	if got != "Line one.\n\nLine two." {
		t.Errorf("normalize = %q, want collapsed + trimmed", got)
	}
}

func TestCleanPostTextEmpty(t *testing.T) {
	if got := cleanPostText("   \n\t\n  "); got != "" {
		t.Errorf("whitespace-only post should clean to empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// The handler — the domain glue behind addon.Run: read the raw body, clean it,
// WRITE the to-text artifact, report its id. Input not ready -> retryable.
// ---------------------------------------------------------------------------

// mockStore is the GleanStore stubbed: a canned source body + a recorded to-text write log.
type mockStore struct {
	raw      string
	ready    bool
	readErr  error
	writeErr error

	writes []writtenText
	nextID int
}

// writtenText records one WriteText call so a test can assert what landed in transcripts.
type writtenText struct {
	sourceType, sourceRef, text, engine string
}

func (m *mockStore) ReadSource(context.Context, addon.Item) (string, bool, error) {
	return m.raw, m.ready, m.readErr
}

func (m *mockStore) WriteText(_ context.Context, sourceType, sourceRef, text, engine string) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	m.nextID++
	m.writes = append(m.writes, writtenText{sourceType, sourceRef, text, engine})
	return m.nextID, nil
}

// TestGleanHandlerCleansAndWritesEmail: an email item with a noisy body is cleaned and written to
// the transcripts store as source_type=email; the row id comes back as the step OutputRef, the item
// is NOT curated out, and the engine label marks it as email extractor output.
func TestGleanHandlerCleansAndWritesEmail(t *testing.T) {
	store := &mockStore{
		raw:   "The actual message.\n\n-- \nRenato\nArchitect",
		ready: true,
	}
	item := addon.Item{ID: 7, Lane: laneEmail, SourceRef: "msg-1"}

	res, err := gleanHandler(store)(context.Background(), item, addon.Step{Seq: 3})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(store.writes) != 1 {
		t.Fatalf("expected 1 to-text write, got %d", len(store.writes))
	}
	w := store.writes[0]
	if w.sourceType != laneEmail || w.sourceRef != "msg-1" || w.engine != emailEngine {
		t.Errorf("write = %+v, want source_type=email ref=msg-1 engine=%s", w, emailEngine)
	}
	if w.text != "The actual message." {
		t.Errorf("written text = %q, want signature stripped", w.text)
	}
	if res.OutputRef != "1" {
		t.Errorf("OutputRef = %q, want the transcript id %q", res.OutputRef, "1")
	}
	if res.Filtered {
		t.Error("a non-empty extraction must not curate the item out")
	}
}

// TestGleanHandlerCleansLinkedIn: a linkedin item uses the lighter post cleaner and is written as
// source_type=linkedin with the linkedin engine label.
func TestGleanHandlerCleansLinkedIn(t *testing.T) {
	store := &mockStore{raw: "<div><p>Posting &amp; shipping</p></div>", ready: true}
	item := addon.Item{ID: 9, Lane: laneLinkedIn, SourceRef: "https://x/p/1"}

	res, err := gleanHandler(store)(context.Background(), item, addon.Step{Seq: 3})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	w := store.writes[0]
	if w.sourceType != laneLinkedIn || w.engine != linkedinEngine {
		t.Errorf("write = %+v, want source_type=linkedin engine=%s", w, linkedinEngine)
	}
	if w.text != "Posting & shipping" {
		t.Errorf("written text = %q, want HTML stripped + unescaped", w.text)
	}
	if res.Filtered {
		t.Error("non-empty post must not be filtered")
	}
}

// TestGleanHandlerFiltersEmptyBody: a body that cleans to nothing (pure signature/quote) is benign
// no-content — the step is still done with its output_ref, but Filtered asks the SDK to curate the
// item out (status='empty' is recorded, not a failure).
func TestGleanHandlerFiltersEmptyBody(t *testing.T) {
	store := &mockStore{raw: "> only quoted\n-- \nsig", ready: true}
	item := addon.Item{ID: 1, Lane: laneEmail, SourceRef: "msg-empty"}

	res, err := gleanHandler(store)(context.Background(), item, addon.Step{Seq: 3})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(store.writes) != 1 || store.writes[0].text != "" {
		t.Fatalf("an all-noise email should still write an empty to-text row, got %+v", store.writes)
	}
	if !res.Filtered {
		t.Error("an empty extraction must curate the item out (Filtered)")
	}
	if res.OutputRef != "1" {
		t.Errorf("OutputRef = %q, want the (empty) transcript id", res.OutputRef)
	}
}

// TestGleanHandlerSourceNotReadyRetryable: the collector has not written the body yet (a race
// against ingest) -> the handler asks the SDK to requeue (ErrRetryable), not fail the item for good,
// and writes nothing.
func TestGleanHandlerSourceNotReadyRetryable(t *testing.T) {
	store := &mockStore{ready: false}
	item := addon.Item{ID: 1, Lane: laneEmail, SourceRef: "msg-missing"}

	_, err := gleanHandler(store)(context.Background(), item, addon.Step{Seq: 3})
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("source-not-ready should be retryable, got %v", err)
	}
	if len(store.writes) != 0 {
		t.Error("no to-text should be written when the source is not ready")
	}
}

// TestGleanHandlerReadErrorFails: a real read error is NOT a retryable miss — it surfaces as-is
// (terminal), with nothing written.
func TestGleanHandlerReadErrorFails(t *testing.T) {
	store := &mockStore{readErr: errors.New("db down")}
	_, err := gleanHandler(store)(context.Background(), addon.Item{ID: 1, Lane: laneEmail, SourceRef: "x"}, addon.Step{Seq: 3})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, addon.ErrRetryable) {
		t.Errorf("a read error must not be retryable, got %v", err)
	}
	if len(store.writes) != 0 {
		t.Error("nothing should be written on a read error")
	}
}

// TestGleanHandlerWriteErrorFails: a transcripts write error is terminal (surfaced as-is), not
// retryable.
func TestGleanHandlerWriteErrorFails(t *testing.T) {
	store := &mockStore{raw: "real body", ready: true, writeErr: errors.New("insert failed")}
	_, err := gleanHandler(store)(context.Background(), addon.Item{ID: 1, Lane: laneEmail, SourceRef: "x"}, addon.Step{Seq: 3})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, addon.ErrRetryable) {
		t.Errorf("a write error must not be retryable, got %v", err)
	}
}

// TestIsValidProvider guards the GLEAN_PROVIDER allow-list (one app, two text providers).
func TestIsValidProvider(t *testing.T) {
	// Both runtimes are valid: the VPC placement runs identical code (the cleaner is
	// lane-driven), so glean-vpc/winnow-vpc/scrub-vpc must be accepted just like the
	// cloud twins — otherwise the dispatched VPC worker log.Fatalf's on startup.
	for _, ok := range []string{
		provExtrairEmail, provExtrairLinkedIn, provExtrairNews,
		provExtrairEmailVPC, provExtrairLinkedInVPC, provExtrairNewsVPC,
	} {
		if !isValidProvider(ok) {
			t.Errorf("%q should be a valid provider", ok)
		}
	}
	for _, bad := range []string{"", "extrair", "transcrever", "assay-cloud", "distill-vpc"} {
		if isValidProvider(bad) {
			t.Errorf("%q should not be a valid provider", bad)
		}
	}
}
