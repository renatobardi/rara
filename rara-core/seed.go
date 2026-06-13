// seed.go — Phase 1 deliverable #1: seed the YouTube lane configuration.
//
// Config is data, not code (the 2.0 principle): a lane is a flow row + its flow_steps,
// a worker is a provider row, a logical task is a capability row. This file writes those
// rows for the single YouTube lane, idempotently (every call is a full-record upsert, so
// re-seeding is safe and converges). It is the only place that knows the *shape* of the
// YouTube pipeline; the reconciler reads it back generically.
//
// One lane, not two: harvest (channels) and shelf (playlists) both feed the SAME spine,
// which dedups globally on youtube_video_id (see ingest.go). The per-item pipeline is
// identical regardless of which collector discovered the video, so there is one `youtube`
// flow whose `coletar` capability simply has two providers (harvest, shelf). The
// channel-vs-playlist distinction lives in the collector, not the flow.
package main

import "context"

// youtubeFlowName is the canonical name of the single YouTube lane flow. The ingest
// stamps items with this flow's id + version.
const youtubeFlowName = "youtube"

const laneYouTube = "youtube"

// Provider names for the YouTube lane (mirror the architecture's naming).
const (
	provHarvest    = "harvest"     // coletar — channels collector (Data API key)
	provShelf      = "shelf"       // coletar — playlists collector (OAuth)
	provASRYouTube = "asr-youtube" // transcrever — scribe on the Mac (residential IP)
	provDistill    = "distill"     // destilar — distill on Cloud Run
)

// passThroughGate marks a flow_step's gate as a no-op (always keep) for this phase.
// Real curation (rules -> profile -> llm-judge) is Phase 3; until then the reconciler
// reads this option and records a keep without calling any gate worker.
const optGateMode = `{"gate":"passthrough"}`

// SeedYouTubeLane writes the YouTube lane's capabilities, providers, flow and flow_steps,
// plus a default global routing policy. Idempotent: safe to call on every boot.
//
// The capability catalog is also seeded by migration 001 (ON CONFLICT DO NOTHING); we
// re-upsert here so the seed is self-contained and testable against an empty store (the
// FK from providers/flow_steps to capabilities must resolve).
func SeedYouTubeLane(ctx context.Context, db Database) error {
	// 1) Capabilities — the fixed logical tasks the lane touches.
	caps := []Capability{
		{Name: capColetar, Description: "Discover work items from a source (collector)"},
		{Name: capTranscrever, Description: "Audio -> text (ASR)"},
		{Name: capGateBarato, Description: "Cheap curation gate on metadata, before paying for to-text"},
		{Name: capGateRico, Description: "Rich curation gate on full text, before paying for distillation"},
		{Name: capDestilar, Description: "Curate text into a RAG-ready knowledge document"},
	}
	for _, c := range caps {
		if err := db.UpsertCapability(ctx, c); err != nil {
			return err
		}
	}

	// 2) Providers — concrete implementations. cost/quality/latency are placeholders;
	//    the router that consumes them is Phase 2. What matters in Phase 1 is the
	//    runtime/activation pair, which decides where work runs and how it wakes.
	providers := []Provider{
		// coletar: two collectors feeding one spine. Cloud Run jobs, woken on demand.
		{Name: provHarvest, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true},
		{Name: provShelf, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true},
		// transcrever: scribe is resident on the Mac. YouTube audio download is blocked
		// from datacenter IPs, hence the residential constraint (the router enforces it
		// in Phase 2; recorded now so the constraint travels with the provider).
		{Name: provASRYouTube, Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident,
			Constraints: []byte(`{"requires":"residential"}`), Enabled: true},
		// destilar: distill is a scale-to-zero Cloud Run job, woken on demand.
		{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true},
	}
	for _, p := range providers {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}

	// 3) Flow + steps — the declarative YouTube pipeline. The canonical lane template is
	//    coletar -> gate_barato -> transcrever -> gate_rico -> destilar. Gates are
	//    pass-through this phase (see optGateMode).
	flowID, err := db.UpsertFlow(ctx, Flow{Name: youtubeFlowName, SourceType: laneYouTube, Enabled: true, Version: 1})
	if err != nil {
		return err
	}
	steps := []FlowStep{
		{FlowID: flowID, Seq: 1, Capability: capColetar, Enabled: true},
		{FlowID: flowID, Seq: 2, Capability: capGateBarato, Options: []byte(optGateMode), Enabled: true},
		{FlowID: flowID, Seq: 3, Capability: capTranscrever, Enabled: true},
		{FlowID: flowID, Seq: 4, Capability: capGateRico, Options: []byte(optGateMode), Enabled: true},
		{FlowID: flowID, Seq: 5, Capability: capDestilar, Enabled: true},
	}
	for _, s := range steps {
		if err := db.UpsertFlowStep(ctx, s); err != nil {
			return err
		}
	}

	// 4) Default global routing policy. Phase 1 selects the only enabled provider per
	//    capability; the cost<->quality weighting below is seeded for Phase 2's router.
	return db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: "global", CostWeight: 0.5, QualityWeight: 0.5})
}
