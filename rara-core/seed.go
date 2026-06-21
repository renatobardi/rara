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
	"log"
	"os"
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

// LLM model names baked into provider Env — kept as constants so they match deploy-distill.yml and
// deploy-sift.yml without silent divergence.
const (
	modelDistill = "groq-llama"
	modelGate    = "groq-fast"
)

// Provider names (mirror the architecture's naming).
const (
	provHarvest        = "harvest"          // coletar — channels collector (Data API key); job rara-harvest
	provShelf          = "shelf"            // coletar — playlists collector (OAuth); job rara-shelf
	provDial           = "dial"             // coletar — podcast RSS collector; job rara-dial (F5 wake+pull)
	provFeed           = "feed"             // coletar — news RSS/HN/HTML collector; job rara-feed
	provCourier        = "courier"          // coletar — Gmail email collector; job rara-courier
	provASRYouTube     = "asr-youtube"      // transcrever — scribe on the Mac (residential IP)
	provASRDirectAudio = "asr-direct-audio" // transcrever — direct-audio ASR on Cloud Run (podcast)
	provDistill        = "distill"          // destilar — distill on Cloud Run (third-party LLM)
	provGateBarato     = "gate-barato"      // gate_barato — metadata cascade worker (third-party LLM)
	provGateRico       = "gate-rico"        // gate_rico — full-text cascade worker (third-party LLM)
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
// (tagged {"sensitivity":"third_party"}) and a self-host VPC provider.
//
// VPC-first routing is enforced by routing_policies.fallback (see seedSharedConfig). Private
// content (email) is additionally excluded from third-party cloud providers by the sensitivity
// constraint, so it always stays on the VPC variant. Both cloud and VPC variants are on_demand:
// the cloud ones are woken via Cloud Run Jobs `run`; the VPC ones are woken by rara-runner
// (POST /run on the tailnet). on_demand is health-exempt at selection time.
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
	runnerURL := os.Getenv("RUNNER_LOCAL_URL")
	vpcEnabled := runnerURL != "" // VPC on_demand providers need a runner URL; disable if unset
	if !vpcEnabled {
		log.Print("seed: RUNNER_LOCAL_URL not set — VPC on_demand providers seeded as disabled")
	}
	providers := []Provider{
		// destilar: LLM step. Both variants on_demand: cloud via Cloud Run Jobs; VPC via rara-runner.
		{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Constraints: thirdParty, Enabled: true, Worker: provDistill,
			Env: []byte(`{"DISTILL_PROVIDER":"distill","LITELLM_MODEL":"` + modelDistill + `"}`)},
		{Name: provDistillLocal, Capability: capDestilar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: provDistill, // same worker as cloud sibling
			RunnerURL: runnerURL,
			Env:       []byte(`{"DISTILL_PROVIDER":"distill-local","CURATE_ENGINE":"litellm","LITELLM_MODEL":"` + modelDistill + `"}`)},
		// gate_barato / gate_rico: cascade gates (rules -> profile -> LLM-judge).
		{Name: provGateBarato, Capability: capGateBarato, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Constraints: thirdParty, Enabled: true, Worker: provGateBarato,
			Env: []byte(`{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"gate-barato","LITELLM_MODEL":"` + modelGate + `"}`)},
		{Name: provGateBaratoLocal, Capability: capGateBarato, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: provGateBarato, // same worker as cloud sibling
			RunnerURL: runnerURL,
			Env:       []byte(`{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"gate-barato-local"}`)},
		{Name: provGateRico, Capability: capGateRico, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Constraints: thirdParty, Enabled: true, Worker: provGateRico,
			Env: []byte(`{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"gate-rico","LITELLM_MODEL":"` + modelGate + `"}`)},
		{Name: provGateRicoLocal, Capability: capGateRico, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: provGateRico, // same worker as cloud sibling
			RunnerURL: runnerURL,
			Env:       []byte(`{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"gate-rico-local"}`)},
	}
	for _, p := range providers {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// seedSharedConfig upserts the global routing policy, per-capability VPC-first policies, and
// the v1 interest_profile. Each LLM capability gets its own policy that pins the VPC provider
// first in the fallback order (local → cloud), so items route to VPC and fall through to cloud
// only when VPC is unhealthy or excluded. The profile is seeded ONCE (idempotent).
func seedSharedConfig(ctx context.Context, db Database) error {
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal}); err != nil {
		return err
	}
	// ponytail: string slice of known-safe ASCII names never errors in json.Marshal
	fallbackJSON := func(names ...string) json.RawMessage { b, _ := json.Marshal(names); return b }
	vpcFirst := []struct {
		scope    string
		fallback json.RawMessage
	}{
		{capGateBarato, fallbackJSON(provGateBaratoLocal, provGateBarato)},
		{capGateRico, fallbackJSON(provGateRicoLocal, provGateRico)},
		{capDestilar, fallbackJSON(provDistillLocal, provDistill)},
	}
	for _, p := range vpcFirst {
		if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: p.scope, Fallback: p.fallback}); err != nil {
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
		// Woken by the dispatcher (rara-runner) on the cadence below; reads target_channels /
		// discovers playlists via API on each wake (sources already in Neon for harvest).
		{Name: provHarvest, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: provHarvest,
			CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)}, // daily cadence; 30min retry throttle
		{Name: provShelf, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: provShelf,
			CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)}, // daily cadence; 30min retry throttle
		// transcrever: scribe (local Whisper) is resident on the Mac. YouTube blocks audio
		// download from datacenter IPs, hence the residential constraint; `accepts` pins it to
		// youtube items so it never competes for a podcast (which a datacenter ASR handles).
		// Routing rule: this is a HARD residential constraint with NO datacenter fallback —
		// fail-closed (item waits) is correct; falling back to Cloud Run/VPC would just hit the
		// same block. Contrast with clip/rara-clip (Bright Data proxies do the unblock,
		// so the host IP doesn't matter → no residential constraint on that provider).
		{Name: provASRYouTube, Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident,
			Worker:      provASRYouTube,
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
	// coletar: rara-dial — woken by the dispatcher on cadence; reads enabled podcast_feeds
	// from the DB on each wake (wake+pull pattern, F5). Provider name "dial" + job prefix
	// "rara-" = Cloud Run job "rara-dial".
	if err := db.UpsertProvider(ctx, Provider{
		Name: provDial, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Enabled: true, Worker: provDial,
		CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800), // daily cadence; 30min retry throttle
	}); err != nil {
		return err
	}
	// transcrever: direct-audio ASR — any runtime (no residential), accepts only podcast.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provASRDirectAudio, Capability: capTranscrever, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Worker:      provASRDirectAudio,
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
	// coletar: rara-courier — woken by the dispatcher on cadence; Gmail OAuth credentials
	// stay in env (Secret Manager) — no "sources" to pull from Neon for this one. Provider
	// name "courier" + job prefix "rara-" = Cloud Run job "rara-courier".
	if err := db.UpsertProvider(ctx, Provider{
		Name: provCourier, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Enabled: true, Worker: provCourier,
		CollectCadenceSeconds: intPtr(21600), RetryIntervalSeconds: intPtr(1800), // 6h cadence; 30min retry throttle
	}); err != nil {
		return err
	}
	// extrair: deterministic HTML/quote/signature cleaning — any runtime, accepts only email.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provExtrairEmail, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Worker:      provExtrairEmail,
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
	// coletar: rara-feed — woken by the dispatcher on cadence; reads enabled feed_sources
	// from the DB on each wake (already reads Neon, pull already done). Provider name "feed"
	// + job prefix "rara-" = Cloud Run job "rara-feed".
	if err := db.UpsertProvider(ctx, Provider{
		Name: provFeed, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Enabled: true, Worker: provFeed,
		CollectCadenceSeconds: intPtr(21600), RetryIntervalSeconds: intPtr(1800), // 6h cadence; 30min retry throttle
	}); err != nil {
		return err
	}
	// extrair: deterministic HTML/boilerplate cleaning — any runtime, accepts only news.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provExtrairNews, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Worker:      provExtrairNews,
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
