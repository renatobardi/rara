package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Repeated test fixtures, named once so no single literal recurs across the cases below.
const (
	urlA         = "https://lnkd.in/a"
	urlB         = "https://lnkd.in/b"
	urlX         = "https://lnkd.in/x"
	authorRenato = "Renato"
)

// ---------------------------------------------------------------------------
// Test doubles — the two seams, mocked, so the collector loop runs with zero I/O.
// ---------------------------------------------------------------------------

// fakeCollector is the Bright Data fetch seam, mocked: it yields a fixed batch (or an error).
type fakeCollector struct {
	posts []LinkedInPost
	err   error
}

func (f *fakeCollector) FetchPosts(_ context.Context) ([]LinkedInPost, error) {
	return f.posts, f.err
}

// mockDatabase is the Neon seam, mocked: linkedin_posts as a map keyed on the URL, so the upsert
// is idempotent exactly like the real ON CONFLICT (url). failOn forces a per-post upsert error.
// stamped records which provider names were passed to StampProviderCollected. stampErr, if set,
// is returned by StampProviderCollected to simulate a provider-not-found or DB error.
type mockDatabase struct {
	posts    map[string]LinkedInPost
	failOn   string
	stamped  []string
	stampErr error
}

func newMockDatabase() *mockDatabase { return &mockDatabase{posts: map[string]LinkedInPost{}} }

func (m *mockDatabase) UpsertLinkedInPost(_ context.Context, p LinkedInPost) error {
	if p.URL == m.failOn {
		return errors.New("upsert failed")
	}
	m.posts[p.URL] = p // idempotent on URL: a re-collect refreshes in place
	return nil
}

func (m *mockDatabase) StampProviderCollected(_ context.Context, name string) error {
	m.stamped = append(m.stamped, name)
	return m.stampErr
}

// ---------------------------------------------------------------------------
// run — pure orchestration over the collector + database seams.
// ---------------------------------------------------------------------------

// The happy path: every complete post the source yields is catalogued into linkedin_posts.
func TestRunCatalogsPosts(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	collector := &fakeCollector{posts: []LinkedInPost{
		{URL: urlA, Author: authorRenato, Text: "on platform engineering"},
		{URL: urlB, Author: "Ana", Text: "on distributed systems"},
	}}

	n, err := run(ctx, db, collector, "clip-vpc")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 || len(db.posts) != 2 {
		t.Fatalf("catalogued=%d posts=%d, want 2/2", n, len(db.posts))
	}
	if got := db.posts[urlA]; got.Author != authorRenato || got.Text != "on platform engineering" {
		t.Errorf("stored post = %+v", got)
	}
}

// A partial row (no URL or no real text) is skipped, not fatal — Bright Data sometimes yields
// incomplete posts; one must not abort the whole crawl. A pure-markup body counts as empty.
func TestRunSkipsPartialRows(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	collector := &fakeCollector{posts: []LinkedInPost{
		{URL: "https://lnkd.in/ok", Text: "real content here"},
		{URL: "", Text: "no url"},                            // partial: no URL
		{URL: "https://lnkd.in/empty", Text: "  "},           // partial: whitespace text
		{URL: "https://lnkd.in/markup", Text: "<div></div>"}, // partial: markup-only
	}}

	n, err := run(ctx, db, collector, "clip-vpc")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 1 || len(db.posts) != 1 {
		t.Errorf("catalogued=%d posts=%d, want only the one complete post", n, len(db.posts))
	}
}

// run is idempotent on the URL: re-collecting the same post converges to one row and refreshes
// the body in place (the real upsert's ON CONFLICT (url)).
func TestRunIdempotentOnURL(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: urlX, Text: "first"}}}, "clip-vpc"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: urlX, Text: "edited"}}}, "clip-vpc"); err != nil {
		t.Fatal(err)
	}
	if len(db.posts) != 1 {
		t.Errorf("re-collect must converge: %d rows", len(db.posts))
	}
	if got := db.posts[urlX].Text; got != "edited" {
		t.Errorf("re-collect should refresh the post: %q", got)
	}
}

// The URL is trimmed before it becomes the idempotency key, so a padded re-collect still
// collapses onto the same row rather than duplicating.
func TestRunTrimsURLKey(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: urlX, Text: "a"}}}, "clip-vpc"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: "  " + urlX + "  ", Text: "b"}}}, "clip-vpc"); err != nil {
		t.Fatal(err)
	}
	if len(db.posts) != 1 {
		t.Errorf("a padded URL must collapse onto the same row: %d rows", len(db.posts))
	}
}

// On a successful run the provider stamp is written once with the provider name "clip", signalling
// to rara-core that this lane has collected. A fetch error must not stamp (the stamp is skipped
// because run returns early before reaching the stamp call).
func TestRunStampsProviderOnSuccess(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	collector := &fakeCollector{posts: []LinkedInPost{
		{URL: urlA, Author: authorRenato, Text: "on platform engineering"},
	}}

	if _, err := run(ctx, db, collector, "clip-vpc"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(db.stamped) != 1 || db.stamped[0] != "clip-vpc" {
		t.Errorf("stamped = %v, want [clip-vpc]", db.stamped)
	}
}

// A fetch error aborts the run (it is a real source fault, not a per-post quirk).
func TestRunPropagatesFetchError(t *testing.T) {
	sentinel := errors.New("bright data unavailable")
	if _, err := run(context.Background(), newMockDatabase(), &fakeCollector{err: sentinel}, "clip-vpc"); !errors.Is(err, sentinel) {
		t.Fatalf("fetch error should propagate, got %v", err)
	}
}

// A per-post upsert error is logged and skipped — one bad row must not stall the crawl; the
// remaining posts are still catalogued.
func TestRunContinuesOnUpsertError(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	db.failOn = "https://lnkd.in/bad"
	collector := &fakeCollector{posts: []LinkedInPost{
		{URL: "https://lnkd.in/bad", Text: "boom"},
		{URL: "https://lnkd.in/good", Text: "ok"},
	}}
	n, err := run(ctx, db, collector, "clip-vpc")
	if err != nil {
		t.Fatalf("a per-post upsert error must not abort the run: %v", err)
	}
	if n != 1 || len(db.posts) != 1 {
		t.Errorf("catalogued=%d posts=%d, want the one good post", n, len(db.posts))
	}
}

// ---------------------------------------------------------------------------
// decodeBrightDataPosts — pure normalizer over the CLI's varying JSON keys.
// ---------------------------------------------------------------------------

func TestDecodeBrightDataPostsFlexibleKeys(t *testing.T) {
	raw := []byte(`[
		{"url":"https://lnkd.in/a","author":"Renato","post_text":"hello"},
		{"post_url":"https://lnkd.in/b","account":"Ana","text":"world"},
		{"url":"https://lnkd.in/c","user_id":"bob","body":"body text"},
		{"url":"https://lnkd.in/d","headline":"just a headline"}
	]`)
	posts, err := decodeBrightDataPosts(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 4 {
		t.Fatalf("decoded %d posts, want 4", len(posts))
	}
	if posts[0].URL != urlA || posts[0].Author != authorRenato || posts[0].Text != "hello" {
		t.Errorf("row 0 = %+v", posts[0])
	}
	if posts[1].URL != urlB || posts[1].Author != "Ana" || posts[1].Text != "world" {
		t.Errorf("row 1 (post_url/account/text aliases) = %+v", posts[1])
	}
	if posts[2].Author != "bob" || posts[2].Text != "body text" {
		t.Errorf("row 2 (user_id/body aliases) = %+v", posts[2])
	}
	if posts[3].Text != "just a headline" {
		t.Errorf("row 3 (headline fallback) = %+v", posts[3])
	}
}

func TestDecodeBrightDataPostsDropsEmpty(t *testing.T) {
	raw := []byte(`[{"url":"https://lnkd.in/a","post_text":"keep"},{"author":"only author"}]`)
	posts, err := decodeBrightDataPosts(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 1 {
		t.Errorf("a row with neither url nor text should be dropped: %+v", posts)
	}
}

func TestDecodeBrightDataPostsInvalidJSON(t *testing.T) {
	if _, err := decodeBrightDataPosts([]byte(`not json`)); err == nil {
		t.Error("invalid JSON should error")
	}
}

// ---------------------------------------------------------------------------
// postHasContent — the storage gate (shared with no one; rara-clip's own copy).
// ---------------------------------------------------------------------------

func TestPostHasContent(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"real text", true},
		{"  ", false},
		{"", false},
		{"<div></div>", false},
		{"&nbsp;", false},
		{"<p>hi</p>", true},
	}
	for _, c := range cases {
		if got := postHasContent(c.raw); got != c.want {
			t.Errorf("postHasContent(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// newBrightDataLinkedInSource — env wiring (defaults, overrides, URL splitting).
// ---------------------------------------------------------------------------

// With the env unset, the constructor falls back to the `bdata` binary and the default
// linkedin-posts pipeline args, and parses the URL list (comma/newline separated, trimmed).
func TestNewBrightDataLinkedInSourceDefaults(t *testing.T) {
	t.Setenv(envBdataBin, "")
	t.Setenv(envBrightDataArgs, "")
	t.Setenv(envBrightDataURLs, urlA+" , \n  "+urlB+"  ")

	s := newBrightDataLinkedInSource()
	if s.bin != "bdata" {
		t.Errorf("default bin = %q, want bdata", s.bin)
	}
	if got := strings.Join(s.args, " "); got != "pipelines linkedin-posts --json" {
		t.Errorf("default args = %q, want the linkedin-posts pipeline", got)
	}
	if len(s.urls) != 2 || s.urls[0] != urlA || s.urls[1] != urlB {
		t.Errorf("urls = %v, want the two trimmed entries", s.urls)
	}
}

// A populated env overrides every default (the binary path and the pipeline args).
func TestNewBrightDataLinkedInSourceOverrides(t *testing.T) {
	t.Setenv(envBdataBin, "/opt/bin/bdata")
	t.Setenv(envBrightDataArgs, "collect --raw")
	t.Setenv(envBrightDataURLs, "https://lnkd.in/only")

	s := newBrightDataLinkedInSource()
	if s.bin != "/opt/bin/bdata" {
		t.Errorf("override bin = %q", s.bin)
	}
	if got := strings.Join(s.args, " "); got != "collect --raw" {
		t.Errorf("override args = %q", got)
	}
	if len(s.urls) != 1 {
		t.Errorf("urls = %v, want one", s.urls)
	}
}

// FetchPosts refuses to shell out when no input URLs are configured (nothing to collect).
func TestFetchPostsNoURLs(t *testing.T) {
	t.Setenv(envBrightDataURLs, "")
	if _, err := newBrightDataLinkedInSource().FetchPosts(context.Background()); err == nil {
		t.Error("FetchPosts with no URLs should error rather than run the CLI")
	}
}

// A StampProviderCollected error must not propagate — the stamp is best-effort. run must still
// return nil so the crawl is considered successful (all posts were catalogued).
func TestRunStampErrorIsNotFatal(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	db.stampErr = errors.New("provider \"clip\" not found in providers table")
	collector := &fakeCollector{posts: []LinkedInPost{
		{URL: urlA, Author: authorRenato, Text: "on platform engineering"},
	}}

	_, err := run(ctx, db, collector, "clip-vpc")
	if err != nil {
		t.Fatalf("stamp error must not be fatal: run returned %v", err)
	}
}

// splitList drops empty/whitespace entries across both comma and newline separators.
func TestSplitList(t *testing.T) {
	got := splitList("a,, b \n\n c ,")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitList = %v, want [a b c]", got)
	}
	if len(splitList("  \n , ")) != 0 {
		t.Error("splitList of only separators/whitespace should be empty")
	}
}
