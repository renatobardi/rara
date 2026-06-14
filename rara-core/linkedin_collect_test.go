package main

import (
	"context"
	"errors"
	"testing"
)

// fakeLinkedInCollector is the automated-source seam, mocked: it yields a fixed batch (or an
// error), so CollectLinkedIn is verified with zero network I/O.
type fakeLinkedInCollector struct {
	posts []LinkedInPost
	err   error
}

func (f *fakeLinkedInCollector) FetchPosts(_ context.Context) ([]LinkedInPost, error) {
	return f.posts, f.err
}

// ---------------------------------------------------------------------------
// CollectLinkedIn — pure orchestration over Database + store + collector seams.
// ---------------------------------------------------------------------------

// The Bright Data collector must write the SAME table + spine items the manual inbox does: it
// reuses SubmitLinkedInPost, so a collected post is indistinguishable downstream.
func TestCollectLinkedInWritesSameContract(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	collector := &fakeLinkedInCollector{posts: []LinkedInPost{
		{URL: "https://lnkd.in/a", Author: "Renato", Text: "on platform engineering"},
		{URL: "https://lnkd.in/b", Author: "Ana", Text: "on distributed systems"},
	}}

	n, err := CollectLinkedIn(ctx, db, store, collector)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if n != 2 {
		t.Fatalf("collected = %d, want 2", n)
	}
	if len(store.posts) != 2 || len(db.items) != 2 {
		t.Fatalf("posts=%d items=%d, want 2/2", len(store.posts), len(db.items))
	}
	for _, it := range db.items {
		if it.Lane != laneLinkedIn || it.Sensitivity != sensitivityPublic || it.Status != itemDiscovered {
			t.Errorf("collected item = %+v, want linkedin/public/discovered (same contract as manual)", it)
		}
	}
}

// A partial row (no URL or no real text) is skipped, not fatal — Bright Data sometimes yields
// incomplete posts; one must not abort the whole crawl.
func TestCollectLinkedInSkipsPartialRows(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	collector := &fakeLinkedInCollector{posts: []LinkedInPost{
		{URL: "https://lnkd.in/ok", Text: "real content here"},
		{URL: "", Text: "no url"},                 // partial: no URL
		{URL: "https://lnkd.in/empty", Text: " "}, // partial: empty text
	}}

	n, err := CollectLinkedIn(ctx, db, store, collector)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if n != 1 || len(db.items) != 1 {
		t.Errorf("collected=%d items=%d, want only the one complete post", n, len(db.items))
	}
}

// CollectLinkedIn is idempotent on the URL (it delegates to SubmitLinkedInPost): re-collecting
// the same batch converges to one item per URL.
func TestCollectLinkedInIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	batch := []LinkedInPost{{URL: "https://lnkd.in/x", Text: "first"}}
	if _, err := CollectLinkedIn(ctx, db, store, &fakeLinkedInCollector{posts: batch}); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectLinkedIn(ctx, db, store, &fakeLinkedInCollector{posts: []LinkedInPost{{URL: "https://lnkd.in/x", Text: "edited"}}}); err != nil {
		t.Fatal(err)
	}
	if len(db.items) != 1 {
		t.Errorf("re-collect must converge: %d items", len(db.items))
	}
	if got := store.posts["https://lnkd.in/x"].Text; got != "edited" {
		t.Errorf("re-collect should refresh the post: %q", got)
	}
}

// A fetch error aborts the collect (it is a real source fault, not a per-post quirk).
func TestCollectLinkedInPropagatesFetchError(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("bright data unavailable")
	if _, err := CollectLinkedIn(ctx, db, newFakeLinkedInStore(), &fakeLinkedInCollector{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("fetch error should propagate, got %v", err)
	}
}

// A genuine submit failure (the flow is not seeded) propagates rather than being swallowed.
func TestCollectLinkedInPropagatesSubmitError(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase() // lane NOT seeded -> SubmitLinkedInPost errors
	collector := &fakeLinkedInCollector{posts: []LinkedInPost{{URL: "https://lnkd.in/x", Text: "body"}}}
	if _, err := CollectLinkedIn(ctx, db, newFakeLinkedInStore(), collector); err == nil {
		t.Error("a real submit failure (unseeded flow) should propagate")
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

// ---------------------------------------------------------------------------
// Seed: the Bright Data collector is seeded alongside the manual inbox (the fallback).
// ---------------------------------------------------------------------------

func TestSeedLinkedInLaneSeedsBrightDataCollector(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Both collectors exist, both coletar, both accept only linkedin (so neither competes with
	// another lane). The manual inbox stays as a fallback.
	for _, name := range []string{provManualInbox, provBrightDataLinked} {
		p, ok := db.providers[name]
		if !ok {
			t.Fatalf("collector %q not seeded", name)
		}
		if p.Capability != capColetar {
			t.Errorf("%q capability = %q, want coletar", name, p.Capability)
		}
		if got := string(p.Constraints); got != `{"accepts":["linkedin"]}` {
			t.Errorf("%q constraints = %q, want accepts=[linkedin]", name, got)
		}
	}
}
