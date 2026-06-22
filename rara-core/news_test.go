package main

import (
	"context"
	"slices"
	"testing"
)

// fakeNewsSource is a fixed list of collected feed articles — the read side of news ingest, mocked.
type fakeNewsSource struct {
	articles []NewsItem
	err      error
}

func (f fakeNewsSource) News(_ context.Context) ([]NewsItem, error) {
	return f.articles, f.err
}

// TestSeedNewsLane: the glean-cloud provider on `extrair` (accepts news) and the news flow that
// swaps transcrever for extrair — the same shape as the email lane (the source is already text).
func TestSeedNewsLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p, ok := db.providers[provExtrairNews]
	if !ok {
		t.Fatalf("provider %q not seeded", provExtrairNews)
	}
	if p.Capability != capExtrair || p.Runtime != runtimeCloudRun {
		t.Errorf("glean-cloud = {%s,%s}, want {extrair,cloudrun}", p.Capability, p.Runtime)
	}
	if got := string(p.Constraints); got != `{"accepts":["news"]}` {
		t.Errorf("glean-cloud constraints = %q, want accepts=[news]", got)
	}
	f, ok := db.flows[newsFlowName]
	if !ok || f.SourceType != laneNews {
		t.Fatalf("news flow = %+v, want news source_type", f)
	}
	steps, _ := db.ListFlowSteps(ctx, f.ID)
	gotSeq := make([]string, len(steps))
	for i, s := range steps {
		gotSeq[i] = s.Capability
	}
	if want := []string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}; !slices.Equal(gotSeq, want) {
		t.Errorf("news flow steps = %v, want %v (extrair in place of transcrever)", gotSeq, want)
	}
}

// TestSeedNewsLaneDisabledByDefault: news is an opt-in lane — it ships DISABLED so lighting it is a
// deliberate operator action (Fontes & Flows toggle / UPDATE flows). The other lanes ship enabled.
func TestSeedNewsLaneDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if db.flows[newsFlowName].Enabled {
		t.Error("news lane should ship disabled (opt-in), got enabled")
	}
}

// TestSeedNewsLanePreservesOperatorEnable: once an operator enables the lane, a later re-seed
// (e.g. a core redeploy running `core-job seed`) must NOT silently turn it back off.
func TestSeedNewsLanePreservesOperatorEnable(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Operator lights the lane.
	f := db.flows[newsFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	// Re-seed.
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if !db.flows[newsFlowName].Enabled {
		t.Error("re-seed turned an operator-enabled news lane back off")
	}
}

// TestIngestFeed: feed articles become items (lane=news, source_ref=url, PUBLIC), idempotent on
// (lane, source_ref), skipping rows with no url.
func TestIngestFeed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Enable the lane so ingest runs (news ships disabled).
	f := db.flows[newsFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	src := fakeNewsSource{articles: []NewsItem{
		{URL: "https://a.example/1", Title: "One"},
		{URL: ""}, // malformed -> skipped
		{URL: "https://a.example/1"},
		{URL: "https://a.example/2", Title: "Two"},
	}}
	n, err := IngestFeed(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 3 {
		t.Errorf("processed %d, want 3 (empty url skipped)", n)
	}
	if len(db.items) != 2 {
		t.Errorf("dedup failed: %d items, want 2", len(db.items))
	}
	it := db.items[itemKey(laneNews, "https://a.example/1")]
	if it.Sensitivity != sensitivityPublic {
		t.Errorf("news item sensitivity = %q, want public", it.Sensitivity)
	}
}

// TestAutoIngestNews: a Reconciler with a news source and an enabled news lane discovers articles
// on its ingest pass — the news lane joins youtube/podcast/email in the auto-ingest loop (A5).
func TestAutoIngestNews(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := db.flows[newsFlowName]
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(db)
	r.news = fakeNewsSource{articles: []NewsItem{{URL: "https://a.example/1"}}}
	r.ingestEveryN = 1

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.items[itemKey(laneNews, "https://a.example/1")]; !ok {
		t.Error("auto-ingest did not discover the news article on the first pass")
	}
}

// TestIngestFeedSkipsDisabledLane: news ships disabled, so ingest is a no-op until the lane is lit.
func TestIngestFeedSkipsDisabledLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedNewsLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakeNewsSource{articles: []NewsItem{{URL: "https://a.example/1", Title: "One"}}}
	n, err := IngestFeed(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 0 || len(db.items) != 0 {
		t.Errorf("disabled lane: processed %d / %d items, want 0/0", n, len(db.items))
	}
}
