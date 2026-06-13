// seed.go — lane configuration seeders (Phase 1 YouTube; Phase 4 podcast + email).
//
// Config is data, not code (the 2.0 principle): a lane is a flow row + its flow_steps, a
// worker is a provider row, a logical task is a capability row. This file writes those rows,
// idempotently (every call is a full-record upsert, so re-seeding is safe and converges). It
// is the only place that knows the *shape* of each lane's pipeline; the reconciler reads it
// back generically.
//
// Structure: capabilities, the shared work providers (gates + distill) and the shared config
// (global policy + interest_profile) are seeded by small helpers that every lane seeder calls,
// so each Seed*Lane is self-contained (safe to run alone against an empty store) and the
// shared rows never diverge between lanes. Each lane seeder then adds its own to-text provider
// and its flow.
package main

import "context"

// Flow names — the canonical name of each lane's flow. ingest stamps items with the flow id +
// version; the source_type column mirrors the lane.
const (
	youtubeFlowName = "youtube"
	podcastFlowName = "podcast"
	emailFlowName   = "email"
)

// Lane names — the source type carried on items.lane (and matched by a provider's `accepts`).
const (
	laneYouTube = "youtube"
	lanePodcast = "podcast"
	laneEmail   = "email"
)

// Provider names (mirror the architecture's naming).
const (
	provHarvest        = "harvest"          // coletar — channels collector (Data API key)
	provShelf          = "shelf"            // coletar — playlists collector (OAuth)
	provASRYouTube     = "asr-youtube"      // transcrever — scribe on the Mac (residential IP)
	provASRDirectAudio = "asr-direct-audio" // transcrever — direct-audio ASR on Cloud Run (podcast)
	provDistill        = "distill"          // destilar — distill on Cloud Run (third-party LLM)
	provGateBarato     = "gate-barato"      // gate_barato — metadata cascade worker (third-party LLM)
	provGateRico       = "gate-rico"        // gate_rico — full-text cascade worker (third-party LLM)
	provExtrairEmail   = "extrair-email"    // extrair — email HTML/quote/signature cleaner (Cloud Run)
	// Self-host variants (VPC) of the LLM steps — the ONLY route for private content. Strictly
	// pricier and lower quality than their cloud siblings, so the cloud (third-party) provider
	// wins for public items, while a private item (third-party excluded) falls to these.
	provDistillLocal    = "distill-local"
	provGateBaratoLocal = "gate-barato-local"
	provGateRicoLocal   = "gate-rico-local"
)

// seedCapabilities upserts the fixed logical-task catalog. Also seeded by migration 001
// (ON CONFLICT DO NOTHING); re-upserted here so a lane seeder is self-contained and testable
// against an empty store (the FK from providers/flow_steps to capabilities must resolve).
func seedCapabilities(ctx context.Context, db Database) error {
	caps := []Capability{
		{Name: capColetar, Description: "Discover work items from a source (collector)"},
		{Name: capTranscrever, Description: "Audio -> text (ASR)"},
		{Name: capExtrair, Description: "Already-text source -> normalized text"},
		{Name: capGateBarato, Description: "Cheap curation gate on metadata, before paying for to-text"},
		{Name: capGateRico, Description: "Rich curation gate on full text, before paying for distillation"},
		{Name: capDestilar, Description: "Curate text into a RAG-ready knowledge document"},
	}
	for _, c := range caps {
		if err := db.UpsertCapability(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// seedSharedProviders upserts the work providers every lane reuses: the two curation gates and
// distill, each in TWO variants — a default cloud provider that calls a third-party model
// (tagged {"sensitivity":"third_party"}) and a self-host VPC provider for private content.
// cost (relative weight), quality (0..1) and latency_ms feed the router's cost<->quality score.
//
// Sensitivity routing (the slice-c payoff): the cloud variant is excluded for a `private` item
// (email), so private content reaches ONLY the self-host variant. The self-host variants are
// deliberately pricier and lower quality than their cloud siblings, so for PUBLIC items the
// cloud provider dominates the score and is chosen — the self-host route exists purely so
// private content has somewhere to go. Cloud providers are on_demand (woken via Cloud Run Jobs
// `run`); the self-host ones run on the always-on VPC. (coletar is auto-satisfied by the
// reconciler — the item already exists — so no coletar provider is needed for routing.)
func seedSharedProviders(ctx context.Context, db Database) error {
	thirdParty := []byte(`{"sensitivity":"third_party"}`)
	providers := []Provider{
		// destilar: the priciest step (model tokens), high quality.
		{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 2.00, Quality: 0.92, LatencyMs: 30000, Constraints: thirdParty, Enabled: true},
		{Name: provDistillLocal, Capability: capDestilar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Cost: 3.00, Quality: 0.84, LatencyMs: 60000, Enabled: true},
		// gate_barato / gate_rico: the cascade gates (rules -> profile -> LLM-judge). Cheap on
		// average (only the borderline middle pays the LLM call).
		{Name: provGateBarato, Capability: capGateBarato, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 0.50, Quality: 0.88, LatencyMs: 5000, Constraints: thirdParty, Enabled: true},
		{Name: provGateBaratoLocal, Capability: capGateBarato, Runtime: runtimeVPC, Activation: activationOnDemand,
			Cost: 0.90, Quality: 0.80, LatencyMs: 9000, Enabled: true},
		{Name: provGateRico, Capability: capGateRico, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 0.60, Quality: 0.90, LatencyMs: 8000, Constraints: thirdParty, Enabled: true},
		{Name: provGateRicoLocal, Capability: capGateRico, Runtime: runtimeVPC, Activation: activationOnDemand,
			Cost: 1.00, Quality: 0.82, LatencyMs: 14000, Enabled: true},
	}
	for _, p := range providers {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// seedSharedConfig upserts the global routing policy and the v1 interest_profile (the gate
// cascade's profile-layer document). The policy is a balanced cost<->quality weighting with no
// explicit fallback. The profile is seeded ONCE (idempotent): a revision is a NEW version
// (Phase 6's learning loop), never an overwrite.
func seedSharedConfig(ctx context.Context, db Database) error {
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 0.5, QualityWeight: 0.5}); err != nil {
		return err
	}
	if _, found, err := db.GetLatestInterestProfile(ctx); err != nil {
		return err
	} else if !found {
		return db.InsertInterestProfile(ctx, InterestProfile{
			Version:    1,
			Topics:     []byte(`["software architecture","platform engineering","devops","ai","llm","data engineering","distributed systems","kubernetes"]`),
			Authors:    []byte(`[]`),
			AntiTopics: []byte(`[]`),
			Weights:    []byte(`{"keep_threshold":0.6}`),
		})
	}
	return nil
}

// seedLaneFlow upserts a flow (version 1) and its ordered steps. Used by every lane seeder so
// the flow shape lives in one place. Returns the flow id (unused by callers today).
func seedLaneFlow(ctx context.Context, db Database, name, sourceType string, capabilities []string) error {
	flowID, err := db.UpsertFlow(ctx, Flow{Name: name, SourceType: sourceType, Enabled: true, Version: 1})
	if err != nil {
		return err
	}
	for i, cap := range capabilities {
		if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: flowID, Seq: i + 1, Capability: cap, Enabled: true}); err != nil {
			return err
		}
	}
	return nil
}

// SeedYouTubeLane writes the YouTube lane: shared capabilities/providers/config plus the
// YouTube-specific collectors (harvest, shelf) and the residential-constrained scribe
// (asr-youtube), and the youtube flow. Idempotent: safe to call on every boot.
func SeedYouTubeLane(ctx context.Context, db Database) error {
	if err := seedCapabilities(ctx, db); err != nil {
		return err
	}
	if err := seedSharedProviders(ctx, db); err != nil {
		return err
	}
	// YouTube-specific providers.
	providers := []Provider{
		// coletar: YouTube Data API (key) and OAuth playlists — cheap, fast metadata reads.
		// (coletar is auto-satisfied by the reconciler, never actually routed; values seeded
		// for completeness and future per-collector routing.)
		{Name: provHarvest, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 0.10, Quality: 0.95, LatencyMs: 500, Enabled: true},
		{Name: provShelf, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 0.10, Quality: 0.95, LatencyMs: 800, Enabled: true},
		// transcrever: scribe (local Whisper) is resident on the Mac. YouTube blocks audio
		// download from datacenter IPs, hence the residential constraint; `accepts` pins it to
		// youtube items so it never competes for a podcast (which a datacenter ASR handles).
		{Name: provASRYouTube, Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident,
			Cost: 1.00, Quality: 0.90, LatencyMs: 120000,
			Constraints: []byte(`{"requires":"residential","accepts":["youtube"]}`), Enabled: true},
	}
	for _, p := range providers {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	// Flow: coletar -> gate_barato -> transcrever -> gate_rico -> destilar.
	if err := seedLaneFlow(ctx, db, youtubeFlowName, laneYouTube,
		[]string{capColetar, capGateBarato, capTranscrever, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}

// SeedPodcastLane writes the Podcast lane: shared capabilities/providers/config plus the
// direct-audio ASR provider (asr-direct-audio) and the podcast flow. The lane template is
// identical to YouTube's; only the to-text provider differs — asr-direct-audio runs on Cloud
// Run with NO residential constraint (the enclosure is a direct CDN mp3, not a yt-dlp
// download) and `accepts` pins it to podcast items. It reuses the gates, distill and the
// transcripts table (source_type=podcast). Idempotent.
func SeedPodcastLane(ctx context.Context, db Database) error {
	if err := seedCapabilities(ctx, db); err != nil {
		return err
	}
	if err := seedSharedProviders(ctx, db); err != nil {
		return err
	}
	// transcrever: direct-audio ASR — any runtime (no residential), accepts only podcast.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provASRDirectAudio, Capability: capTranscrever, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Cost: 0.80, Quality: 0.88, LatencyMs: 90000,
		Constraints: []byte(`{"accepts":["podcast"]}`), Enabled: true,
	}); err != nil {
		return err
	}
	if err := seedLaneFlow(ctx, db, podcastFlowName, lanePodcast,
		[]string{capColetar, capGateBarato, capTranscrever, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}

// SeedEmailLane writes the Email lane: shared capabilities/providers/config plus the email
// extractor (extrair-email) and the email flow. The lane template swaps `transcrever` for
// `extrair` (the source is already text — clean HTML/signature/quoted-reply instead of
// transcribing audio); everything else mirrors the other lanes. Email content is PRIVATE, so
// IngestEmail stamps items sensitivity=private and the router (slice c) keeps them off
// third-party models — but the extractor itself does deterministic local cleaning (no LLM), so
// it carries no sensitivity tag. Idempotent.
func SeedEmailLane(ctx context.Context, db Database) error {
	if err := seedCapabilities(ctx, db); err != nil {
		return err
	}
	if err := seedSharedProviders(ctx, db); err != nil {
		return err
	}
	// extrair: deterministic HTML/quote/signature cleaning — any runtime, accepts only email.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provExtrairEmail, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Cost: 0.20, Quality: 0.85, LatencyMs: 2000,
		Constraints: []byte(`{"accepts":["email"]}`), Enabled: true,
	}); err != nil {
		return err
	}
	if err := seedLaneFlow(ctx, db, emailFlowName, laneEmail,
		[]string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}
