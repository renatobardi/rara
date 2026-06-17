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

// TestSeedLinkedInLaneDisabledByDefault: linkedin is an opt-in lane — it ships DISABLED so
// lighting it is a deliberate operator action (Fontes & Flows toggle / UPDATE flows).
func TestSeedLinkedInLaneDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if db.flows[linkedinFlowName].Enabled {
		t.Error("linkedin lane should ship disabled (opt-in), got enabled")
	}
}

// ---------------------------------------------------------------------------
// IngestLinkedIn — Bright Data path: linkedin_posts → spine.
// ---------------------------------------------------------------------------

// fakeLinkedInSource is a fixed list of collected posts — the read side of bulk ingest, mocked.
type fakeLinkedInSource struct {
	posts []LinkedInPost
	err   error
}

func (f fakeLinkedInSource) LinkedInPosts(_ context.Context) ([]LinkedInPost, error) {
	return f.posts, f.err
}

// TestIngestLinkedIn: posts from the source become spine items (lane=linkedin, source_ref=url,
// PUBLIC), idempotent on (lane, source_ref), rows with empty url are skipped.
func TestIngestLinkedIn(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// LinkedIn ships disabled — enable it for this test.
	f := db.flows[linkedinFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	src := fakeLinkedInSource{posts: []LinkedInPost{
		{URL: "https://linkedin.com/posts/1", Author: "Alice"},
		{URL: ""},                                              // malformed → skipped
		{URL: "https://linkedin.com/posts/1", Author: "Alice"}, // duplicate → one item
		{URL: "https://linkedin.com/posts/2", Author: "Bob"},
	}}
	n, err := IngestLinkedIn(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 3 {
		t.Errorf("processed %d, want 3 (empty url skipped)", n)
	}
	if len(db.items) != 2 {
		t.Errorf("dedup failed: %d items, want 2", len(db.items))
	}
	it := db.items[itemKey(laneLinkedIn, "https://linkedin.com/posts/1")]
	if it.Sensitivity != sensitivityPublic {
		t.Errorf("linkedin item sensitivity = %q, want public", it.Sensitivity)
	}
}

// TestIngestLinkedInSkipsDisabledLane: linkedin ships disabled, so IngestLinkedIn is a no-op
// until the operator lights the lane.
func TestIngestLinkedInSkipsDisabledLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakeLinkedInSource{posts: []LinkedInPost{{URL: "https://linkedin.com/posts/1"}}}
	n, err := IngestLinkedIn(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 0 || len(db.items) != 0 {
		t.Errorf("disabled lane: processed %d / %d items, want 0/0", n, len(db.items))
	}
}

// TestIngestLinkedInManualAndBrightDataConverge: a post submitted via the manual-inbox
// (SubmitLinkedInPost) and the same URL arriving via the Bright Data bulk source
// (IngestLinkedIn) converge to exactly ONE spine item — DiscoverItem is idempotent.
func TestIngestLinkedInManualAndBrightDataConverge(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := db.flows[linkedinFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	store := newFakeLinkedInStore()
	const url = "https://linkedin.com/posts/shared"

	// Manual-inbox path.
	if _, err := SubmitLinkedInPost(ctx, db, store, LinkedInPost{
		URL: url, Author: "Alice", Text: "Shipping today.",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Bright Data bulk path — same URL.
	if _, err := IngestLinkedIn(ctx, db, fakeLinkedInSource{posts: []LinkedInPost{{URL: url}}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if len(db.items) != 1 {
		t.Errorf("manual + bulk ingest produced %d items, want 1", len(db.items))
	}
}

// TestAutoIngestLinkedIn: a Reconciler with a linkedin source and an enabled linkedin lane
// discovers posts on its ingest pass — the linkedin lane joins the auto-ingest loop.
func TestAutoIngestLinkedIn(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := db.flows[linkedinFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(db, nil)
	r.li = fakeLinkedInSource{posts: []LinkedInPost{{URL: "https://linkedin.com/posts/1"}}}
	r.ingestEveryN = 1

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.items[itemKey(laneLinkedIn, "https://linkedin.com/posts/1")]; !ok {
		t.Error("auto-ingest did not discover the linkedin post on the first pass")
	}
}

// TestSeedLinkedInLanePreservesOperatorEnable: once an operator enables the lane, a later
// re-seed (e.g. a core redeploy running `core-job seed`) must NOT silently turn it back off.
func TestSeedLinkedInLanePreservesOperatorEnable(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Operator lights the lane.
	f := db.flows[linkedinFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	// Re-seed.
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if !db.flows[linkedinFlowName].Enabled {
		t.Error("re-seed turned an operator-enabled linkedin lane back off")
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

// The LinkedIn lane seeds BOTH collectors as config-as-data: the manual inbox (the surface's
// fallback) and the automated brightdata-linkedin crawl (now its own app, rara-clip). Both
// coletar, both accept only linkedin, so neither competes with another lane. The provider rows
// describe the lane's collectors; the automated collector's CODE lives in rara-clip.
func TestSeedLinkedInLaneSeedsBrightDataCollector(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
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
