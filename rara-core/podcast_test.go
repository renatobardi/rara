package main

import (
	"context"
	"testing"
)

// fakePodcastSource is a fixed list of collected episodes — the read side of podcast ingest,
// mocked (zero I/O).
type fakePodcastSource struct {
	episodes []PodcastEpisode
	err      error
}

func (f fakePodcastSource) PodcastEpisodes(_ context.Context) ([]PodcastEpisode, error) {
	return f.episodes, f.err
}

// TestSeedPodcastLane asserts the podcast lane config: the echo-cloud provider on
// transcrever (cloudrun, on_demand, accepts=podcast, NO residential) and the podcast flow
// (same template as youtube). The shared gates/distill are reused.
func TestSeedPodcastLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedPodcastLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// rara-dial: the podcast coletar provider — registered so the F3 Runner dispatch can wake it.
	if dial, ok := db.providers[provDial]; !ok {
		t.Fatalf("provider %q not seeded — rara-dial must be coletar so the Runner dispatch can wake it", provDial)
	} else if dial.Capability != capColetar || dial.Runtime != runtimeCloudRun || dial.Activation != activationOnDemand || !dial.Enabled {
		t.Errorf("rara-dial = {cap:%s rt:%s act:%s enabled:%v}, want {coletar,cloudrun,on_demand,true}",
			dial.Capability, dial.Runtime, dial.Activation, dial.Enabled)
	}

	// echo-cloud provider: transcrever, cloudrun, on_demand, accepts podcast, no residential.
	p, ok := db.providers[provASRDirectAudio]
	if !ok {
		t.Fatalf("provider %q not seeded", provASRDirectAudio)
	}
	if p.Capability != capTranscrever || p.Runtime != runtimeCloudRun || p.Activation != activationOnDemand {
		t.Errorf("echo-cloud = {%s,%s,%s}, want {transcrever,cloudrun,on_demand}", p.Capability, p.Runtime, p.Activation)
	}
	if got := string(p.Constraints); got != `{"accepts":["podcast"]}` {
		t.Errorf("echo-cloud constraints = %q, want accepts=[podcast] (no residential)", got)
	}

	// The shared work providers are present (reused, not lane-specific).
	for _, name := range []string{provGateBarato, provGateRico, provDistill} {
		if _, ok := db.providers[name]; !ok {
			t.Errorf("shared provider %q not seeded by the podcast lane", name)
		}
	}

	// Flow: podcast/v1 with the canonical 5 steps.
	f, ok := db.flows[podcastFlowName]
	if !ok || f.SourceType != lanePodcast || f.Version != 1 || !f.Enabled {
		t.Fatalf("podcast flow = %+v, want podcast/v1/enabled", f)
	}
	steps, _ := db.ListFlowSteps(ctx, f.ID)
	wantSeq := []string{capColetar, capGateBarato, capTranscrever, capGateRico, capDestilar}
	if len(steps) != len(wantSeq) {
		t.Fatalf("got %d podcast flow steps, want %d", len(steps), len(wantSeq))
	}
	for i, s := range steps {
		if s.Seq != i+1 || s.Capability != wantSeq[i] {
			t.Errorf("step %d = (seq %d, %s), want (seq %d, %s)", i, s.Seq, s.Capability, i+1, wantSeq[i])
		}
	}
}

// TestIngestPodcast: episodes become items (lane=podcast, source_ref=guid, public), idempotent.
func TestIngestPodcast(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedPodcastLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakePodcastSource{episodes: []PodcastEpisode{
		{GUID: "ep1", Title: "One"},
		{GUID: ""},    // malformed -> skipped
		{GUID: "ep1"}, // same guid -> one item
		{GUID: "ep2", Title: "Two"},
	}}
	n, err := IngestPodcast(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 3 { // ep1 twice + ep2, empty skipped
		t.Errorf("processed %d, want 3 (empty skipped)", n)
	}
	if len(db.items) != 2 {
		t.Errorf("dedup failed: %d items, want 2", len(db.items))
	}
	it, ok := db.items[itemKey(lanePodcast, "ep1")]
	if !ok {
		t.Fatal("item for ep1 not created")
	}
	if it.Lane != lanePodcast || it.SourceRef != "ep1" || it.Status != itemDiscovered {
		t.Errorf("item = %+v, want lane=podcast source_ref=ep1 discovered", it)
	}
	if it.Sensitivity != sensitivityPublic {
		t.Errorf("podcast item sensitivity = %q, want public", it.Sensitivity)
	}
}

func TestIngestPodcastRequiresSeededFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, err := IngestPodcast(ctx, db, fakePodcastSource{episodes: []PodcastEpisode{{GUID: "x"}}}); err == nil {
		t.Fatal("ingest without a seeded podcast flow should error")
	}
}

// TestReconcilePodcastRoutesDirectAudio is the slice (a) payoff: with BOTH lanes seeded,
// transcrever has two providers, and the reconciler routes each item to the provider whose
// `accepts` matches its lane — a podcast item to echo-cloud, never to the residential
// caption-mac. YouTube routing is unchanged (verified in the same test).
func TestReconcilePodcastRoutesDirectAudio(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	enableYouTubeFlow(t, db)
	if err := SeedPodcastLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Both ASR providers known alive (resident caption-mac; on_demand echo-cloud is
	// health-exempt but a heartbeat is harmless).
	markProviderAlive(t, db, provASRYouTube)
	markProviderAlive(t, db, provASRDirectAudio)

	// Discover one podcast episode and one youtube video.
	if _, err := IngestPodcast(ctx, db, fakePodcastSource{episodes: []PodcastEpisode{{GUID: "ep1"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestYouTube(ctx, db, fakeSpineSource{videos: []YouTubeVideo{{VideoID: "vid1"}}}); err != nil {
		t.Fatal(err)
	}
	podID := db.items[itemKey(lanePodcast, "ep1")].ID
	ytID := db.items[itemKey(laneYouTube, "vid1")].ID

	r := NewReconciler(db)
	// Pass 1 assigns gate_barato to both; keep both; pass 2 assigns transcrever to each.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	runGate(t, db, podID, 2, gateBarato, decisionKeep)
	runGate(t, db, ytID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if s := db.itemSteps[itemStepKey{podID, 3}]; s.Status != stepPending || s.AssignedProvider != provEchoLocal {
		t.Errorf("podcast transcrever = %+v, want pending+%s (VPC-first routing)", s, provEchoLocal)
	}
	if s := db.itemSteps[itemStepKey{ytID, 3}]; s.Status != stepPending || s.AssignedProvider != provASRYouTube {
		t.Errorf("youtube transcrever = %+v, want pending+%s (unchanged)", s, provASRYouTube)
	}
}
