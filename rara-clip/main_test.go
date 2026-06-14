package main

import (
	"context"
	"errors"
	"testing"
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
type mockDatabase struct {
	posts  map[string]LinkedInPost
	failOn string
}

func newMockDatabase() *mockDatabase { return &mockDatabase{posts: map[string]LinkedInPost{}} }

func (m *mockDatabase) UpsertLinkedInPost(_ context.Context, p LinkedInPost) error {
	if p.URL == m.failOn {
		return errors.New("upsert failed")
	}
	m.posts[p.URL] = p // idempotent on URL: a re-collect refreshes in place
	return nil
}

// ---------------------------------------------------------------------------
// run — pure orchestration over the collector + database seams.
// ---------------------------------------------------------------------------

// The happy path: every complete post the source yields is catalogued into linkedin_posts.
func TestRunCatalogsPosts(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	collector := &fakeCollector{posts: []LinkedInPost{
		{URL: "https://lnkd.in/a", Author: "Renato", Text: "on platform engineering"},
		{URL: "https://lnkd.in/b", Author: "Ana", Text: "on distributed systems"},
	}}

	n, err := run(ctx, db, collector)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 || len(db.posts) != 2 {
		t.Fatalf("catalogued=%d posts=%d, want 2/2", n, len(db.posts))
	}
	if got := db.posts["https://lnkd.in/a"]; got.Author != "Renato" || got.Text != "on platform engineering" {
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

	n, err := run(ctx, db, collector)
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
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: "https://lnkd.in/x", Text: "first"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: "https://lnkd.in/x", Text: "edited"}}}); err != nil {
		t.Fatal(err)
	}
	if len(db.posts) != 1 {
		t.Errorf("re-collect must converge: %d rows", len(db.posts))
	}
	if got := db.posts["https://lnkd.in/x"].Text; got != "edited" {
		t.Errorf("re-collect should refresh the post: %q", got)
	}
}

// The URL is trimmed before it becomes the idempotency key, so a padded re-collect still
// collapses onto the same row rather than duplicating.
func TestRunTrimsURLKey(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: "https://lnkd.in/x", Text: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, db, &fakeCollector{posts: []LinkedInPost{{URL: "  https://lnkd.in/x  ", Text: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if len(db.posts) != 1 {
		t.Errorf("a padded URL must collapse onto the same row: %d rows", len(db.posts))
	}
}

// A fetch error aborts the run (it is a real source fault, not a per-post quirk).
func TestRunPropagatesFetchError(t *testing.T) {
	sentinel := errors.New("bright data unavailable")
	if _, err := run(context.Background(), newMockDatabase(), &fakeCollector{err: sentinel}); !errors.Is(err, sentinel) {
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
	n, err := run(ctx, db, collector)
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
	if posts[0].URL != "https://lnkd.in/a" || posts[0].Author != "Renato" || posts[0].Text != "hello" {
		t.Errorf("row 0 = %+v", posts[0])
	}
	if posts[1].URL != "https://lnkd.in/b" || posts[1].Author != "Ana" || posts[1].Text != "world" {
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
