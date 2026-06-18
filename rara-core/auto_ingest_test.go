package main

import (
	"context"
	"errors"
	"testing"
)

// TestAutoIngestRunsOnFirstPass: a Reconciler with sources and ingestEveryN=1 calls ingest
// on the first pass; newly collected items appear in the spine without a manual core-job ingest.
func TestAutoIngestRunsOnFirstPass(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	enableYouTubeFlow(t, db)
	r := NewReconciler(db)
	r.yt = fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	r.ingestEveryN = 1

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.items[itemKey(laneYouTube, "vid1")]; !ok {
		t.Error("auto-ingest did not discover vid1 on the first pass")
	}
}

// TestAutoIngestIdempotent: running two passes with the same source does not duplicate items.
func TestAutoIngestIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	enableYouTubeFlow(t, db)
	r := NewReconciler(db)
	r.yt = fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	r.ingestEveryN = 1

	for i := 0; i < 2; i++ {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
	if len(db.items) != 1 {
		t.Errorf("after 2 passes: %d items, want 1 (idempotent)", len(db.items))
	}
}

// TestAutoIngestSkipsLaneWithoutFlow: a lane whose flow has not been seeded is skipped
// silently — the pass completes without error and no item is created.
func TestAutoIngestSkipsLaneWithoutFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	// No seeds — no flows exist.
	r := NewReconciler(db)
	r.yt = fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	r.pod = fakePodcastSource{episodes: []PodcastEpisode{{GUID: "ep1"}}}
	r.ingestEveryN = 1

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("unexpected error when flows not seeded: %v", err)
	}
	if len(db.items) != 0 {
		t.Errorf("expected 0 items (lanes skipped), got %d", len(db.items))
	}
}

// TestAutoIngestLaneErrorDoesNotBlockOthers: if YouTube ingest fails (source errors), the
// Podcast lane still runs and its items are discovered.
func TestAutoIngestLaneErrorDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := SeedPodcastLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(db)
	r.yt = fakeSpineSource{err: errors.New("youtube source boom")}
	r.pod = fakePodcastSource{episodes: []PodcastEpisode{{GUID: "ep1"}}}
	r.ingestEveryN = 1

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.items[itemKey(lanePodcast, "ep1")]; !ok {
		t.Error("podcast item not discovered after youtube source error")
	}
}

// TestAutoIngestEveryNPasses: with ingestEveryN=3, ingest fires only on the 3rd (and 6th,…)
// pass, not on passes 1 or 2.
func TestAutoIngestEveryNPasses(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	enableYouTubeFlow(t, db)
	r := NewReconciler(db)
	r.yt = fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	r.ingestEveryN = 3

	// Passes 1 and 2: ingest must NOT run.
	for i := 0; i < 2; i++ {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		if _, ok := db.items[itemKey(laneYouTube, "vid1")]; ok {
			t.Errorf("ingest ran on pass %d (want only on pass 3)", i+1)
		}
	}
	// Pass 3: ingest MUST run.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.items[itemKey(laneYouTube, "vid1")]; !ok {
		t.Error("ingest did not run on pass 3")
	}
}

// TestAutoIngestSkipsDisabledLane: a seeded but disabled flow must be skipped silently by
// the reconciler — no item created, no error, no redundant log every 30s.
func TestAutoIngestSkipsDisabledLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Disable the youtube flow after seeding.
	f := db.flows[youtubeFlowName]
	f.Enabled = false
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(db)
	r.yt = fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}
	r.ingestEveryN = 1

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatalf("unexpected error with disabled lane: %v", err)
	}
	if len(db.items) != 0 {
		t.Errorf("disabled lane: %d items created, want 0", len(db.items))
	}
}
