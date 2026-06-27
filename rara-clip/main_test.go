package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
// profiles is the list returned by FetchTargetProfiles.
type mockDatabase struct {
	posts      map[string]LinkedInPost
	failOn     string
	stamped    []string
	stampErr   error
	profiles   []string
	profileErr error
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

func (m *mockDatabase) FetchTargetProfiles(_ context.Context) ([]string, error) {
	return m.profiles, m.profileErr
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
// partitionURLs — routes profile URLs to the right pipeline.
// ---------------------------------------------------------------------------

func TestPartitionURLs(t *testing.T) {
	persons, companies := partitionURLs([]string{
		"https://www.linkedin.com/in/satyanadella/",
		"https://www.linkedin.com/company/langchain/posts/",
		"https://www.linkedin.com/showcase/google-antigravity/posts/",
		"https://www.linkedin.com/in/fabiobragaoliveira/",
	})
	if len(persons) != 2 {
		t.Errorf("persons = %v, want 2", persons)
	}
	if len(companies) != 2 {
		t.Errorf("companies = %v, want 2", companies)
	}
}

// ---------------------------------------------------------------------------
// decodeBrightDataPosts — pure normalizer over the CLI's varying JSON keys.
// ---------------------------------------------------------------------------

func TestDecodeBrightDataPostsProfileFormat(t *testing.T) {
	raw := []byte(`[
		{"name":"Renato","posts":[
			{"title":"hello","attribution":"first para","link":"https://lnkd.in/a"},
			{"title":"only title","link":"https://lnkd.in/b"},
			{"attribution":"only attr","link":"https://lnkd.in/c"}
		]},
		{"name":"Ana","posts":[
			{"title":"world","attribution":"second","link":"https://lnkd.in/d"}
		]}
	]`)
	posts, err := decodeBrightDataPosts(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 4 {
		t.Fatalf("decoded %d posts, want 4", len(posts))
	}
	if posts[0].URL != urlA || posts[0].Author != authorRenato || posts[0].Text != "hello\n\nfirst para" {
		t.Errorf("row 0 (title+attribution combined) = %+v", posts[0])
	}
	if posts[1].URL != urlB || posts[1].Text != "only title" {
		t.Errorf("row 1 (title only) = %+v", posts[1])
	}
	if posts[2].URL != "https://lnkd.in/c" || posts[2].Text != "only attr" {
		t.Errorf("row 2 (attribution only) = %+v", posts[2])
	}
	if posts[3].Author != "Ana" {
		t.Errorf("row 3 author = %+v", posts[3])
	}
}

// Posts with created_at older than postLookbackDays are dropped; recent ones and undated ones pass.
func TestDecodeBrightDataPostsFiltersOldPosts(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	raw := []byte(`[{"name":"Renato","posts":[
		{"title":"recent","link":"https://lnkd.in/a","created_at":"2026-06-25T00:00:00.000Z"},
		{"title":"too old","link":"https://lnkd.in/b","created_at":"2026-06-01T00:00:00.000Z"},
		{"title":"no date","link":"https://lnkd.in/c"}
	]}]`)
	posts, err := decodeBrightDataPostsAt(raw, now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("want 2 (recent + undated), got %d: %v", len(posts), posts)
	}
	if posts[0].URL != urlA {
		t.Errorf("expected recent post first, got %+v", posts[0])
	}
	if posts[1].URL != "https://lnkd.in/c" {
		t.Errorf("expected undated post second, got %+v", posts[1])
	}
}

func TestDecodeBrightDataPostsDropsEmpty(t *testing.T) {
	raw := []byte(`[{"name":"Renato","posts":[
		{"title":"keep","link":"https://lnkd.in/a"},
		{"title":"","attribution":"","link":""}
	]}]`)
	posts, err := decodeBrightDataPosts(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 1 {
		t.Errorf("a post with no link and no text should be dropped: %+v", posts)
	}
}

func TestDecodeBrightDataPostsInvalidJSON(t *testing.T) {
	if _, err := decodeBrightDataPosts([]byte(`not json`)); err == nil {
		t.Error("invalid JSON should error")
	}
}

// ---------------------------------------------------------------------------
// decodeCompanyProfiles — company/showcase pipeline (updates[] format).
// ---------------------------------------------------------------------------

func TestDecodeCompanyProfiles(t *testing.T) {
	raw := []byte(`[{"name":"LangChain","updates":[
		{"post_url":"https://lnkd.in/a","text":"hello world","title":"ignored when text set"},
		{"post_url":"https://lnkd.in/b","text":"","title":"title only"},
		{"post_url":"","text":"","title":""}
	]}]`)
	posts, err := decodeCompanyProfiles(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("want 2 posts (empty dropped), got %d: %v", len(posts), posts)
	}
	if posts[0].URL != urlA || posts[0].Author != "LangChain" || posts[0].Text != "hello world" {
		t.Errorf("post 0 = %+v", posts[0])
	}
	if posts[1].Text != "title only" {
		t.Errorf("post 1 (title fallback) = %+v", posts[1])
	}
}

// Company updates have a "date" field (not "created_at"); old ones must be filtered out.
func TestDecodeCompanyProfilesFiltersOldPosts(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	raw := []byte(`[{"name":"LangChain","updates":[
		{"post_url":"https://lnkd.in/a","text":"recent","date":"2026-06-26T19:57:47.848Z"},
		{"post_url":"https://lnkd.in/b","text":"too old","date":"2026-06-01T00:00:00.000Z"},
		{"post_url":"https://lnkd.in/c","text":"no date"}
	]}]`)
	posts, err := decodeCompanyProfilesAt(raw, now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("want 2 (recent + undated), got %d: %v", len(posts), posts)
	}
	if posts[0].URL != urlA {
		t.Errorf("expected recent post first, got %+v", posts[0])
	}
	if posts[1].URL != "https://lnkd.in/c" {
		t.Errorf("expected undated post second, got %+v", posts[1])
	}
}

func TestNormalizeLinkedInCompanyURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://www.linkedin.com/company/langchain/posts/", "https://www.linkedin.com/company/langchain/"},
		{"https://www.linkedin.com/showcase/google-antigravity/posts/?feedView=all", "https://www.linkedin.com/showcase/google-antigravity/"},
		{"https://www.linkedin.com/showcase/nvidiabrasil/", "https://www.linkedin.com/showcase/nvidiabrasil/"},
		{"https://www.linkedin.com/company/apple/posts/", "https://www.linkedin.com/company/apple/"},
	}
	for _, c := range cases {
		if got := normalizeLinkedInCompanyURL(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
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
// linkedin_person_profile pipeline args. URLs come from the caller (DB-sourced), not from env.
func TestNewBrightDataLinkedInSourceDefaults(t *testing.T) {
	t.Setenv(envBdataBin, "")
	t.Setenv(envBrightDataArgs, "")

	s := newBrightDataLinkedInSource([]string{urlA, urlB})
	if s.bin != "bdata" {
		t.Errorf("default bin = %q, want bdata", s.bin)
	}
	if got := strings.Join(s.args, " "); got != "pipelines linkedin_person_profile --json" {
		t.Errorf("default args = %q, want the linkedin_person_profile pipeline", got)
	}
	if len(s.urls) != 2 || s.urls[0] != urlA || s.urls[1] != urlB {
		t.Errorf("urls = %v, want the two provided entries", s.urls)
	}
}

// A populated env overrides the binary path and pipeline args (not the URLs).
func TestNewBrightDataLinkedInSourceOverrides(t *testing.T) {
	t.Setenv(envBdataBin, "/opt/bin/bdata")
	t.Setenv(envBrightDataArgs, "collect --raw")

	s := newBrightDataLinkedInSource([]string{urlA})
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

// FetchPosts refuses to shell out when no URLs are provided (nothing to collect).
func TestFetchPostsNoURLs(t *testing.T) {
	if _, err := newBrightDataLinkedInSource(nil).FetchPosts(context.Background()); err == nil {
		t.Error("FetchPosts with no URLs should error rather than run the CLI")
	}
}

// collectLinkedIn skips with (0, nil) when no profiles are configured — nothing to collect.
func TestCollectLinkedInSkipsWhenNoProfiles(t *testing.T) {
	db := newMockDatabase() // profiles = nil
	n, err := collectLinkedIn(context.Background(), db, "clip-vpc")
	if err != nil || n != 0 {
		t.Errorf("want (0, nil) for empty profiles, got (%d, %v)", n, err)
	}
}

// collectLinkedIn propagates a FetchTargetProfiles error to the caller.
func TestCollectLinkedInPropagatesProfileFetchError(t *testing.T) {
	sentinel := errors.New("db down")
	db := newMockDatabase()
	db.profileErr = sentinel
	_, err := collectLinkedIn(context.Background(), db, "clip-vpc")
	if !errors.Is(err, sentinel) {
		t.Errorf("want profile fetch error in chain, got %v", err)
	}
}

// collectLinkedIn with profiles runs the full collector path (covered by run tests via fakeCollector).
func TestCollectLinkedInWithProfilesRunsCollector(t *testing.T) {
	db := newMockDatabase()
	db.profiles = []string{urlA}
	// fakeCollector is wired via newBrightDataLinkedInSource — we can't inject it here, so we
	// only verify that collectLinkedIn returns an error when FetchPosts fails (no bdata CLI).
	// The full collector path is covered by the run() tests.
	_, err := collectLinkedIn(context.Background(), db, "clip-vpc")
	// bdata is not installed in tests; FetchPosts returns an error — that's expected.
	if err == nil {
		t.Error("expected error from missing bdata CLI")
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
