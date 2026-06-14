package main

import (
	"context"
	"strings"
	"testing"
)

// fakeLinkedInStore records the posts SubmitLinkedInPost writes — the domain-write seam, mocked.
type fakeLinkedInStore struct {
	posts map[string]LinkedInPost // keyed on url (mirrors UNIQUE(url))
	err   error
}

func newFakeLinkedInStore() *fakeLinkedInStore {
	return &fakeLinkedInStore{posts: make(map[string]LinkedInPost)}
}

func (f *fakeLinkedInStore) UpsertLinkedInPost(_ context.Context, p LinkedInPost) error {
	if f.err != nil {
		return f.err
	}
	f.posts[p.URL] = p // ON CONFLICT (url) DO UPDATE
	return nil
}

// ---------------------------------------------------------------------------
// postHasContent — the collector's emptiness gate, pure + zero I/O. The full to-text cleaning
// now lives in the rara-glean app (with its own tests); the core keeps only this predicate.
// ---------------------------------------------------------------------------

func TestPostHasContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain text", "Shipping a control plane today.", true},
		{"text inside HTML", `<div><p>Hello &amp; welcome</p></div>`, true},
		{"whitespace only", "   \n\t\n  ", false},
		{"empty markup only", "<div></div>", false},
		{"entity-only whitespace", "<p>&nbsp;</p>", false}, // &nbsp; unescapes to a space -> empty
		{"truly empty", "", false},
	}
	for _, c := range cases {
		if got := postHasContent(c.in); got != c.want {
			t.Errorf("%s: postHasContent(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// SubmitLinkedInPost — pure orchestration over Database + the store seam.
// ---------------------------------------------------------------------------

func TestSubmitLinkedInPostDiscoversPublicItem(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	const url = "https://www.linkedin.com/posts/renato_activity-123"

	id, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: url, Author: "Renato", Text: "On platform engineering and control planes."})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The post landed in the domain store...
	if p, ok := store.posts[url]; !ok || p.Author != "Renato" {
		t.Fatalf("post not stored: %+v", store.posts)
	}
	// ...and the spine item is linkedin + public.
	it, ok := db.itemByID[id]
	if !ok {
		t.Fatalf("item %d not discovered", id)
	}
	if it.Lane != laneLinkedIn || it.SourceRef != url {
		t.Errorf("item = {%s,%s}, want {linkedin,%s}", it.Lane, it.SourceRef, url)
	}
	if it.Sensitivity != sensitivityPublic {
		t.Errorf("sensitivity = %q, want public", it.Sensitivity)
	}
	if it.Status != itemDiscovered {
		t.Errorf("status = %q, want discovered", it.Status)
	}
}

func TestSubmitLinkedInPostIdempotentOnURL(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	const url = "https://www.linkedin.com/posts/x"

	id1, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: url, Text: "first version of the post"})
	if err != nil {
		t.Fatal(err)
	}
	// Resubmitting the same URL collapses onto the same item and refreshes the post.
	id2, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: " " + url + " ", Text: "second version, edited"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("resubmission must reuse the item id: %d != %d", id1, id2)
	}
	if len(db.items) != 1 {
		t.Errorf("dedup on url failed: %d items", len(db.items))
	}
	if got := store.posts[url].Text; !strings.Contains(got, "second version") {
		t.Errorf("post not refreshed on resubmit: %q", got)
	}
}

func TestSubmitLinkedInPostValidates(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	// Empty URL.
	if _, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: "  ", Text: "body"}); err == nil {
		t.Error("empty url should error")
	}
	// Empty text (after cleaning).
	if _, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: "https://x", Text: "  \n "}); err == nil {
		t.Error("empty post text should error")
	}
	if len(store.posts) != 0 || len(db.items) != 0 {
		t.Errorf("a rejected submission must write nothing: posts=%d items=%d", len(store.posts), len(db.items))
	}
}

func TestSubmitLinkedInPostRequiresSeededFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase() // lane NOT seeded
	store := newFakeLinkedInStore()
	if _, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: "https://x", Text: "body"}); err == nil {
		t.Error("submit without a seeded linkedin flow should error")
	}
	if len(store.posts) != 0 {
		t.Errorf("no post should be written when the flow is missing: %d", len(store.posts))
	}
}

// ---------------------------------------------------------------------------
// Seed + reconcile.
// ---------------------------------------------------------------------------

// TestSeedLinkedInLane: the manual-inbox collector, the extrair-linkedin provider (accepts
// linkedin), and the linkedin flow that swaps transcrever for extrair.
func TestSeedLinkedInLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inbox, ok := db.providers[provManualInbox]
	if !ok || inbox.Capability != capColetar {
		t.Fatalf("manual-inbox = %+v, want a coletar provider", inbox)
	}
	ex, ok := db.providers[provExtrairLinked]
	if !ok {
		t.Fatalf("provider %q not seeded", provExtrairLinked)
	}
	if ex.Capability != capExtrair {
		t.Errorf("extrair-linkedin capability = %q, want extrair", ex.Capability)
	}
	if got := string(ex.Constraints); got != `{"accepts":["linkedin"]}` {
		t.Errorf("extrair-linkedin constraints = %q, want accepts=[linkedin]", got)
	}
	f, ok := db.flows[linkedinFlowName]
	if !ok || f.SourceType != laneLinkedIn {
		t.Fatalf("linkedin flow = %+v, want linkedin source_type", f)
	}
	steps, _ := db.ListFlowSteps(ctx, f.ID)
	wantSeq := []string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}
	if len(steps) != len(wantSeq) {
		t.Fatalf("got %d linkedin flow steps, want %d", len(steps), len(wantSeq))
	}
	for i, s := range steps {
		if s.Capability != wantSeq[i] {
			t.Errorf("step %d = %s, want %s (extrair in place of transcrever)", i+1, s.Capability, wantSeq[i])
		}
	}
}

// TestReconcileLinkedInRoutesToExtrairLinkedin: the linkedin flow routes the to-text step to
// the extrair-linkedin provider (not the email extractor), and once it completes the item
// reaches to_text; gate_rico for a PUBLIC post routes to the third-party (cloud) gate, not the
// self-host one (the sensitivity payoff, in reverse of email).
func TestReconcileLinkedInRoutesToExtrairLinkedin(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	itemID, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{URL: "https://lnkd.in/p1", Text: "on distributed systems"})
	if err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // coletar auto-done, assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign extrair (seq 3)
		t.Fatal(err)
	}
	s, ok := stepBySeq(db, itemID, 3)
	if !ok || s.Capability != capExtrair || s.AssignedProvider != provExtrairLinked {
		t.Fatalf("to-text step = %+v, want extrair+extrair-linkedin", s)
	}
	completeStep(t, db, itemID, 3, "transcript-linkedin-1")
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_rico
		t.Fatal(err)
	}
	if got := db.itemByID[itemID].Status; got != itemToText {
		t.Errorf("item status = %q, want to_text after extrair", got)
	}
	if g, ok := stepBySeq(db, itemID, 4); !ok || g.AssignedProvider != provGateRico {
		t.Errorf("gate_rico step = %+v, want pending+gate-rico (third-party, public item)", g)
	}
}
