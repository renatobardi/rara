package main

import (
	"context"
	"errors"
	"testing"
)

// fakeSpineSource is a fixed list of collected videos — the read side of ingest, mocked.
type fakeSpineSource struct {
	videos []YouTubeVideo
	err    error
}

func (f fakeSpineSource) YouTubeVideos(_ context.Context) ([]YouTubeVideo, error) {
	return f.videos, f.err
}

func TestIngestRequiresSeededFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	// No SeedYouTubeLane -> the youtube flow does not exist.
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{videos: []YouTubeVideo{{VideoID: "a"}}}); err == nil {
		t.Fatal("ingest without a seeded flow should error")
	}
}

func TestIngestCreatesItems(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakeSpineSource{videos: []YouTubeVideo{
		{VideoID: "vid1", Title: "One"},
		{VideoID: "vid2", Title: "Two"},
	}}
	n, err := IngestYouTube(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 2 {
		t.Fatalf("ingested %d, want 2", n)
	}
	flowID := db.flows[youtubeFlowName].ID
	for _, ref := range []string{"vid1", "vid2"} {
		it, ok := db.items[itemKey(laneYouTube, ref)]
		if !ok {
			t.Fatalf("item for %q not created", ref)
		}
		if it.Lane != laneYouTube || it.SourceRef != ref {
			t.Errorf("item = %+v, want lane=youtube source_ref=%s", it, ref)
		}
		if it.FlowID != flowID || it.FlowVersion != 1 {
			t.Errorf("item flow stamp = (%d,v%d), want (%d,v1)", it.FlowID, it.FlowVersion, flowID)
		}
		if it.Status != itemDiscovered {
			t.Errorf("new item status = %q, want discovered", it.Status)
		}
	}
}

// TestIngestIdempotentPreservesStatus is the key discovery contract: re-ingesting an
// already-known, in-flight video must NOT reset its runtime status back to discovered.
func TestIngestIdempotentPreservesStatus(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	if _, err := IngestYouTube(ctx, db, src); err != nil {
		t.Fatal(err)
	}
	// The reconciler advances the item to to_text.
	it := db.items[itemKey(laneYouTube, "vid1")]
	it.Status = itemToText
	if _, err := db.UpsertItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	// Re-discovery of the same video must preserve to_text.
	if _, err := IngestYouTube(ctx, db, src); err != nil {
		t.Fatal(err)
	}
	if got := db.items[itemKey(laneYouTube, "vid1")].Status; got != itemToText {
		t.Errorf("re-ingest regressed status to %q, want to_text preserved", got)
	}
	if len(db.items) != 1 {
		t.Errorf("re-ingest duplicated the item: %d rows", len(db.items))
	}
}

func TestIngestSkipsEmptyAndDedups(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakeSpineSource{videos: []YouTubeVideo{
		{VideoID: "vid1"},
		{VideoID: ""},     // malformed (private/deleted) -> skipped
		{VideoID: "vid1"}, // same video again (channel + playlist) -> one item
	}}
	n, err := IngestYouTube(ctx, db, src)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // vid1 processed twice (idempotent), empty skipped
		t.Errorf("processed %d, want 2 (empty skipped)", n)
	}
	if len(db.items) != 1 {
		t.Errorf("dedup failed: %d items, want 1", len(db.items))
	}
}

func TestIngestPropagatesSourceError(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("boom")
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("ingest should surface the source error, got %v", err)
	}
}
