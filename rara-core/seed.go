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

import (
	"context"
	"encoding/json"
)

// Flow names — the canonical name of each lane's flow. ingest stamps items with the flow id +
// version; the source_type column mirrors the lane.
const (
	youtubeFlowName = "youtube"
	podcastFlowName = "podcast"
	emailFlowName   = "email"
	newsFlowName    = "news"
)

// Lane names — the source type carried on items.lane (and matched by a provider's `accepts`).
const (
	laneYouTube = "youtube"
	lanePodcast = "podcast"
	laneEmail   = "email"
	laneNews    = "news"
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
	provDial           = "rara-dial"        // coletar — podcast RSS collector (Cloud Run, F5 wake+pull)
	provExtrairEmail   = "extrair-email"    // extrair — email HTML/quote/signature cleaner (Cloud Run)
	provExtrairNews    = "extrair-news"     // extrair — feed-article HTML/boilerplate cleaner (Cloud Run)
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
	// env = the per-run NON-secret config each worker IMAGE reads from its environment (confirmed
	// against the worker main.go + the Cloud Run deploy YAML). Identity keys: sift reads
	// SIFT_GATE + SIFT_PROVIDER, distill reads DISTILL_PROVIDER (so it claims only its own steps).
	// The cloud variants also pin LITELLM_MODEL (the exact value baked in deploy-sift.yml /
	// deploy-distill.yml today: groq-fast for gates, groq-llama for distill). The self-host
	// (-local, VPC) variants carry identity only — their model/endpoint comes from the host's own
	// LiteLLM/ollama config, not a constant we can seed here. NO secrets (DATABASE_URL, API keys,
	// LITELLM_BASE_URL is a deploy-resolved endpoint) — the host/agent resolves those (§7).
	providers := []Provider{
		// destilar: the priciest step (model tokens), high quality. Cloud is on_demand (woken
		// via Cloud Run Jobs `run`); the self-host variant is resident on the always-on VPC, so
		// the reconciler never tries to "wake" it (and the router's health gate applies, with
		// bootstrap grace until its first heartbeat — the asr-youtube model).
		{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 2.00, Quality: 0.92, LatencyMs: 30000, Constraints: thirdParty, Enabled: true,
			Env: []byte(`{"DISTILL_PROVIDER":"distill","LITELLM_MODEL":"groq-llama"}`)},
		{Name: provDistillLocal, Capability: capDestilar, Runtime: runtimeVPC, Activation: activationResident,
			Cost: 3.00, Quality: 0.84, LatencyMs: 60000, Enabled: true,
			Env: []byte(`{"DISTILL_PROVIDER":"distill-local"}`)},
		// gate_barato / gate_rico: the cascade gates (rules -> profile -> LLM-judge). Cheap on
		// average (only the borderline middle pays the LLM call).
		{Name: provGateBarato, Capability: capGateBarato, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 0.50, Quality: 0.88, LatencyMs: 5000, Constraints: thirdParty, Enabled: true,
			Env: []byte(`{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"gate-barato","LITELLM_MODEL":"groq-fast"}`)},
		{Name: provGateBaratoLocal, Capability: capGateBarato, Runtime: runtimeVPC, Activation: activationResident,
			Cost: 0.90, Quality: 0.80, LatencyMs: 9000, Enabled: true,
			Env: []byte(`{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"gate-barato-local"}`)},
		{Name: provGateRico, Capability: capGateRico, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Cost: 0.60, Quality: 0.90, LatencyMs: 8000, Constraints: thirdParty, Enabled: true,
			Env: []byte(`{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"gate-rico","LITELLM_MODEL":"groq-fast"}`)},
		{Name: provGateRicoLocal, Capability: capGateRico, Runtime: runtimeVPC, Activation: activationResident,
			Cost: 1.00, Quality: 0.82, LatencyMs: 14000, Enabled: true,
			Env: []byte(`{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"gate-rico-local"}`)},
	}
	for _, p := range providers {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// seedSharedConfig upserts the global routing policy, per-capability VPC-first policies, and
// the v1 interest_profile. The global policy is a balanced cost<->quality weighting with no
// explicit fallback. Each LLM capability gets its own policy that pins the resident VPC provider
// first (local → cloud fallback), so public items are routed to the VPC worker when healthy and
// fall through to the cloud on_demand provider when the resident is down or unassigned.
// The profile is seeded ONCE (idempotent): a revision is a NEW version (Phase 6's learning
// loop), never an overwrite.
func seedSharedConfig(ctx context.Context, db Database) error {
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 0.5, QualityWeight: 0.5}); err != nil {
		return err
	}
	// VPC-first per-capability policies: resident local → cloud on_demand fallback.
	// The fallback list pins the ordering so the router prefers the VPC provider regardless
	// of cost/quality score (the *-local variants are seeded worse on both axes intentionally,
	// as that controls private-vs-public routing — the fallback list overrides score for public).
	vpcFirst := []struct {
		scope    string
		fallback json.RawMessage
	}{
		{capGateBarato, json.RawMessage(`["gate-barato-local","gate-barato"]`)},
		{capGateRico, json.RawMessage(`["gate-rico-local","gate-rico"]`)},
		{capDestilar, json.RawMessage(`["distill-local","distill"]`)},
	}
	for _, p := range vpcFirst {
		if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: p.scope, CostWeight: 0.5, QualityWeight: 0.5, Fallback: p.fallback}); err != nil {
			return err
		}
	}

	if _, found, err := db.GetLatestInterestProfile(ctx); err != nil {
		return err
	} else if !found {
		// v1 is the bootstrap profile: seeded ACTIVE so the gate has a live document from day
		// one. Every later version (the Phase 6 reviser, or a manual surface add) is `proposed`
		// and needs explicit approval to take effect.
		return db.InsertInterestProfile(ctx, InterestProfile{
			Version:    1,
			Topics:     []byte(`["software architecture","platform engineering","devops","ai","llm","data engineering","distributed systems","kubernetes"]`),
			Authors:    []byte(`[]`),
			AntiTopics: []byte(`[]`),
			Weights:    []byte(`{"keep_threshold":0.6}`),
			Status:     profileActive,
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
	for i, capName := range capabilities {
		if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: flowID, Seq: i + 1, Capability: capName, Enabled: true}); err != nil {
			return err
		}
	}
	return nil
}

// seedOptInLaneFlow is like seedLaneFlow but preserves an existing flow's Enabled flag so that
// re-seeding never silently disables a lane the operator has turned on. Defaults to disabled on
// first seed (operator must opt in explicitly).
func seedOptInLaneFlow(ctx context.Context, db Database, name, sourceType string, capabilities []string) error {
	enabled := false
	if f, found, err := db.GetFlow(ctx, name); err != nil {
		return err
	} else if found {
		enabled = f.Enabled
	}
	flowID, err := db.UpsertFlow(ctx, Flow{Name: name, SourceType: sourceType, Enabled: enabled, Version: 1})
	if err != nil {
		return err
	}
	for i, capName := range capabilities {
		if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: flowID, Seq: i + 1, Capability: capName, Enabled: true}); err != nil {
			return err
		}
	}
	return nil
}

// SeedYouTubeLane writes the YouTube lane: shared capabilities/providers/config plus the
// YouTube-specific collectors (harvest, shelf) and the residential-constrained scribe
// (asr-youtube), and the youtube flow. Idempotent: safe to call on every boot.
//
// The flow seeds DISABLED on first run and preserves the operator's enable on re-seed — a
// later `core-job seed` must never silently turn the lane back off. Enable deliberately via
// Fontes & Flows or: UPDATE flows SET enabled=true WHERE name='youtube'.
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
	// Preserves operator's enable across re-seeds; defaults to disabled on first seed.
	if err := seedOptInLaneFlow(ctx, db, youtubeFlowName, laneYouTube,
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
	// coletar: rara-dial — woken by the Runner dispatch (F3) like any worker; reads enabled
	// podcast_feeds from the DB on each wake (wake+pull pattern, F5).
	if err := db.UpsertProvider(ctx, Provider{
		Name: provDial, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Cost: 0.05, Quality: 0.99, LatencyMs: 30000, Enabled: true,
	}); err != nil {
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
	// Preserves operator's enable across re-seeds; defaults to disabled on first seed.
	if err := seedOptInLaneFlow(ctx, db, emailFlowName, laneEmail,
		[]string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}

// SeedNewsLane writes the News lane: shared capabilities/providers/config plus the news extractor
// (extrair-news) and the news flow. Like email, the source is already text (feed articles from
// rara-feed: HN/RSS/html), so the lane swaps `transcrever` for `extrair` (clean HTML/boilerplate
// instead of transcribing audio). News is PUBLIC, so IngestFeed stamps items sensitivity=public.
//
// Unlike the always-on lanes, news ships DISABLED: lighting it is a deliberate operator action
// (Fontes & Flows toggle, or UPDATE flows SET enabled=true WHERE name='news'). So this seeder does
// not reuse seedLaneFlow (which would force enabled=true): it seeds the flow disabled on first run
// and PRESERVES an operator's enable on re-seed — a later `core-job seed` must never silently turn
// the lane back off. Idempotent.
func SeedNewsLane(ctx context.Context, db Database) error {
	if err := seedCapabilities(ctx, db); err != nil {
		return err
	}
	if err := seedSharedProviders(ctx, db); err != nil {
		return err
	}
	// extrair: deterministic HTML/boilerplate cleaning — any runtime, accepts only news.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provExtrairNews, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Cost: 0.20, Quality: 0.85, LatencyMs: 2000,
		Constraints: []byte(`{"accepts":["news"]}`), Enabled: true,
	}); err != nil {
		return err
	}
	// Preserves operator's enable across re-seeds; defaults to disabled on first seed.
	if err := seedOptInLaneFlow(ctx, db, newsFlowName, laneNews,
		[]string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}
