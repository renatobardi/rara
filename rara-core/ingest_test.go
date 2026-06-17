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
	enableYouTubeFlow(t, db)
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
	enableYouTubeFlow(t, db)
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

// TestIngestFreezesFlowVersion (#5 cleanup): a flow version bump must NOT re-stamp an
// already-discovered item. In-flight items finish on the flow shape they were discovered
// with; only NEW items pick up the new version.
func TestIngestFreezesFlowVersion(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	enableYouTubeFlow(t, db)
	src := fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	if _, err := IngestYouTube(ctx, db, src); err != nil {
		t.Fatal(err)
	}
	if got := db.items[itemKey(laneYouTube, "vid1")].FlowVersion; got != 1 {
		t.Fatalf("first discovery flow_version = %d, want 1", got)
	}

	// Operator edits the flow: bump version to 2.
	f := db.flows[youtubeFlowName]
	f.Version = 2
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}

	// Re-discovery of the SAME video must keep flow_version frozen at 1.
	if _, err := IngestYouTube(ctx, db, src); err != nil {
		t.Fatal(err)
	}
	if got := db.items[itemKey(laneYouTube, "vid1")].FlowVersion; got != 1 {
		t.Errorf("re-ingest re-stamped flow_version to %d, want it frozen at 1", got)
	}

	// A brand-new video discovered now picks up version 2.
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid2"}}}); err != nil {
		t.Fatal(err)
	}
	if got := db.items[itemKey(laneYouTube, "vid2")].FlowVersion; got != 2 {
		t.Errorf("new item flow_version = %d, want 2", got)
	}
}

func TestIngestSkipsEmptyAndDedups(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	enableYouTubeFlow(t, db)
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
	enableYouTubeFlow(t, db)
	sentinel := errors.New("boom")
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("ingest should surface the source error, got %v", err)
	}
}

// TestIngestYouTubeSkipsDisabledFlow: a disabled youtube flow must not be ingested.
// Returns 0 items and nil error — the lane is intentionally off, not broken.
func TestIngestYouTubeSkipsDisabledFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Disable the flow.
	f := db.flows[youtubeFlowName]
	f.Enabled = false
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	n, err := IngestYouTube(ctx, db, fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}})
	if err != nil {
		t.Fatalf("disabled flow should not error, got %v", err)
	}
	if n != 0 {
		t.Fatalf("disabled flow: ingested %d items, want 0", n)
	}
	if len(db.items) != 0 {
		t.Fatalf("disabled flow: %d items created, want 0", len(db.items))
	}
}

// TestIngestPodcastSkipsDisabledFlow: same contract for the podcast lane.
func TestIngestPodcastSkipsDisabledFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedPodcastLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := db.flows[podcastFlowName]
	f.Enabled = false
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	n, err := IngestPodcast(ctx, db, fakePodcastSource{episodes: []PodcastEpisode{{GUID: "ep1"}}})
	if err != nil {
		t.Fatalf("disabled podcast flow should not error, got %v", err)
	}
	if n != 0 || len(db.items) != 0 {
		t.Fatalf("disabled podcast flow: n=%d items=%d, want both 0", n, len(db.items))
	}
}

// TestIngestEmailSkipsDisabledFlow: same contract for the email lane.
func TestIngestEmailSkipsDisabledFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedEmailLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := db.flows[emailFlowName]
	f.Enabled = false
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	n, err := IngestEmail(ctx, db, fakeEmailSource{emails: []EmailItem{{MessageID: "msg1"}}})
	if err != nil {
		t.Fatalf("disabled email flow should not error, got %v", err)
	}
	if n != 0 || len(db.items) != 0 {
		t.Fatalf("disabled email flow: n=%d items=%d, want both 0", n, len(db.items))
	}
}
