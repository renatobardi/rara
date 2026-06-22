package main

import (
	"context"
	"strings"
	"testing"

	addon "rara-addon"
)

// TestExtrairNewsIsValidProvider: glean-cloud joins winnow-cloud/scrub-cloud as a recognized provider.
func TestExtrairNewsIsValidProvider(t *testing.T) {
	if !isValidProvider(provExtrairNews) {
		t.Errorf("%q should be a valid GLEAN_PROVIDER", provExtrairNews)
	}
}

// TestGleanHandlerCleansAndWritesNews: a news article (already text from the feed) is HTML-stripped
// and written as a to-text row tagged with the news engine, pinned to lane=news.
func TestGleanHandlerCleansAndWritesNews(t *testing.T) {
	store := &mockStore{
		raw:   "<p>The headline body.</p><p>Second paragraph.</p>",
		ready: true,
	}
	item := addon.Item{ID: 9, Lane: laneNews, SourceRef: "https://a.example/1"}

	res, err := gleanHandler(store)(context.Background(), item, addon.Step{Seq: 3})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(store.writes) != 1 {
		t.Fatalf("expected 1 to-text write, got %d", len(store.writes))
	}
	w := store.writes[0]
	if w.sourceType != laneNews || w.sourceRef != "https://a.example/1" || w.engine != newsEngine {
		t.Errorf("write = %+v, want source_type=news ref=url engine=%s", w, newsEngine)
	}
	if strings.Contains(w.text, "<p>") || !strings.Contains(w.text, "The headline body.") {
		t.Errorf("news text not cleaned: %q", w.text)
	}
	if res.Filtered {
		t.Error("a non-empty extraction must not curate the item out")
	}
}

// TestGleanHandlerFiltersEmptyNews: an article with no captured text (body+excerpt empty) writes an
// empty to-text row and curates the item out (Filtered) — it does NOT fail the step ("não derruba").
func TestGleanHandlerFiltersEmptyNews(t *testing.T) {
	store := &mockStore{raw: "", ready: true}
	item := addon.Item{ID: 9, Lane: laneNews, SourceRef: "https://a.example/empty"}

	res, err := gleanHandler(store)(context.Background(), item, addon.Step{Seq: 3})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.Filtered {
		t.Error("an article with no text must curate the item out (Filtered), not fail")
	}
}
