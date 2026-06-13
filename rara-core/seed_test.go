package main

import (
	"context"
	"testing"
)

// TestSeedYouTubeLane asserts the lane config the reconciler later reads back: the five
// capabilities, the six providers (incl. the two gate workers) with the right
// runtime/activation, one `youtube` flow at version 1, its five ordered steps, a default
// policy, and the seeded interest_profile v1.
func TestSeedYouTubeLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Capabilities the lane touches (extrair is not part of the YouTube lane).
	for _, c := range []string{capColetar, capTranscrever, capGateBarato, capGateRico, capDestilar} {
		if _, ok := db.capabilities[c]; !ok {
			t.Errorf("capability %q not seeded", c)
		}
	}

	// Providers: runtime/activation are what Phase 1 acts on.
	wantProviders := map[string]struct {
		cap, runtime, activation string
	}{
		provHarvest:    {capColetar, runtimeCloudRun, activationOnDemand},
		provShelf:      {capColetar, runtimeCloudRun, activationOnDemand},
		provASRYouTube: {capTranscrever, runtimeLocal, activationResident},
		provDistill:    {capDestilar, runtimeCloudRun, activationOnDemand},
		provGateBarato: {capGateBarato, runtimeCloudRun, activationOnDemand},
		provGateRico:   {capGateRico, runtimeCloudRun, activationOnDemand},
	}
	for name, want := range wantProviders {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("provider %q not seeded", name)
			continue
		}
		if p.Capability != want.cap || p.Runtime != want.runtime || p.Activation != want.activation {
			t.Errorf("provider %q = {%s,%s,%s}, want {%s,%s,%s}",
				name, p.Capability, p.Runtime, p.Activation, want.cap, want.runtime, want.activation)
		}
		if !p.Enabled {
			t.Errorf("provider %q should be enabled", name)
		}
	}
	// asr-youtube carries the residential requirement AND accepts only youtube (so it never
	// competes for a podcast item). The router enforces both.
	if got := string(db.providers[provASRYouTube].Constraints); got != `{"requires":"residential","accepts":["youtube"]}` {
		t.Errorf("asr-youtube constraints = %q, want residential + accepts youtube", got)
	}

	// Flow: single youtube lane at version 1.
	f, ok := db.flows[youtubeFlowName]
	if !ok {
		t.Fatalf("flow %q not seeded", youtubeFlowName)
	}
	if f.SourceType != laneYouTube || f.Version != 1 || !f.Enabled {
		t.Errorf("flow = %+v, want youtube/v1/enabled", f)
	}

	// Steps: coletar -> gate_barato -> transcrever -> gate_rico -> destilar, in order.
	steps, _ := db.ListFlowSteps(ctx, f.ID)
	wantSeq := []string{capColetar, capGateBarato, capTranscrever, capGateRico, capDestilar}
	if len(steps) != len(wantSeq) {
		t.Fatalf("got %d flow steps, want %d", len(steps), len(wantSeq))
	}
	for i, s := range steps {
		if s.Seq != i+1 || s.Capability != wantSeq[i] {
			t.Errorf("step %d = (seq %d, %s), want (seq %d, %s)", i, s.Seq, s.Capability, i+1, wantSeq[i])
		}
	}
	// Default routing policy seeded for Phase 2's router.
	if _, ok := db.policies["global"]; !ok {
		t.Error("global routing policy not seeded")
	}

	// interest_profile v1 seeded with a keep_threshold and starter topics.
	p, found, _ := db.GetLatestInterestProfile(ctx)
	if !found || p.Version != 1 {
		t.Fatalf("interest_profile v1 not seeded (found=%v, v%d)", found, p.Version)
	}
	if got := string(p.Weights); got != `{"keep_threshold":0.6}` {
		t.Errorf("profile weights = %q, want a keep_threshold", got)
	}
	if len(parseStringArray(p.Topics)) == 0 {
		t.Error("profile v1 should seed starter topics")
	}
}

// TestSeedIdempotent asserts re-seeding converges (no duplicate rows, stable flow id).
func TestSeedIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	id1 := db.flows[youtubeFlowName].ID
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := db.flows[youtubeFlowName].ID; got != id1 {
		t.Errorf("flow id changed on re-seed: %d -> %d", id1, got)
	}
	// 6 shared work providers (gate-barato/-local, gate-rico/-local, distill/-local) + 3
	// YouTube-specific (harvest, shelf, asr-youtube).
	if len(db.providers) != 9 {
		t.Errorf("expected 9 providers after re-seed, got %d", len(db.providers))
	}
	if len(db.flowSteps) != 5 {
		t.Errorf("expected 5 flow steps after re-seed, got %d", len(db.flowSteps))
	}
	// interest_profile is seeded exactly once — re-seeding must not create a v2 or error.
	if len(db.profiles) != 1 {
		t.Errorf("expected interest_profile seeded once, got %d versions", len(db.profiles))
	}
}
