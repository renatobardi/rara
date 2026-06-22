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

// Provider names — <worker>-<runtime> taxonomy (P1b).
// App column holds the pre-rename deploy target (Cloud Run job = jobPrefix+app, runner allowlist key = app).
const (
	provHarvest        = "harvest-cloud" // coletar — channels collector (Data API key)
	provShelf          = "shelf-cloud"   // coletar — playlists collector (OAuth)
	provDial           = "dial-cloud"    // coletar — podcast RSS collector
	provFeed           = "feed-cloud"    // coletar — news RSS/HN/HTML collector
	provCourier        = "courier-cloud" // coletar — Gmail email collector
	provASRYouTube     = "caption-mac"   // transcrever — scribe on the Mac (residential IP)
	provASRDirectAudio = "echo-cloud"    // transcrever — direct-audio ASR on Cloud Run (podcast)
	provDistill        = "distill-cloud" // destilar — distill on Cloud Run (third-party LLM)
	provGateBarato     = "sift-cloud"    // gate_barato — metadata cascade worker (third-party LLM)
	provGateRico       = "assay-cloud"   // gate_rico — full-text cascade worker (third-party LLM)
	provExtrairEmail   = "winnow-cloud"  // extrair — email HTML/quote/signature cleaner
	provExtrairNews    = "glean-cloud"   // extrair — feed-article HTML/boilerplate cleaner
	// VPC (self-host) variants of the LLM steps — the ONLY route for private content.
	provDistillLocal    = "distill-vpc"
	provGateBaratoLocal = "sift-vpc"
	provGateRicoLocal   = "assay-vpc"
	// VPC variants of per-lane workers (coletores, transcrever, extrair) — seeded disabled
	// until RUNNER_LOCAL_URL is set (same gate as the LLM steps above).
	provHarvestLocal  = "harvest-vpc"
	provShelfLocal    = "shelf-vpc"
	provDialLocal     = "dial-vpc"
	provFeedLocal     = "feed-vpc"
	provCourierLocal  = "courier-vpc"
	provEchoLocal     = "echo-vpc"
	provWinnowLocal   = "winnow-vpc"
	provGleanLocal    = "glean-vpc"
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
// vpcRunner returns the runner URL and whether VPC on_demand providers should be enabled.
// All VPC provider seeding gates on this pair; the log warning lives in seedSharedProviders.
func vpcRunner() (url string, enabled bool) {
	url = os.Getenv("RUNNER_LOCAL_URL")
	return url, url != ""
}

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
	modelDistill := os.Getenv("DISTILL_MODEL")
	if modelDistill == "" {
		log.Fatalf("seed: DISTILL_MODEL is required")
	}
	modelGate := os.Getenv("GATE_MODEL")
	if modelGate == "" {
		log.Fatalf("seed: GATE_MODEL is required")
	}
	runnerURL, vpcEnabled := vpcRunner()
	if !vpcEnabled {
		log.Print("seed: RUNNER_LOCAL_URL not set — VPC on_demand providers seeded as disabled")
	}
	// Safely encode model names from env into provider Env JSON — json.Marshal handles any
	// special characters (quotes, backslashes) that would otherwise break the raw concatenation.
	mDistill, _ := json.Marshal(modelDistill)
	mGate, _ := json.Marshal(modelGate)

	providers := []Provider{
		// destilar: LLM step. Both variants on_demand: cloud via Cloud Run Jobs; VPC via rara-runner.
		{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Constraints: thirdParty, Enabled: true, Worker: "distill", App: "distill",
			Description: "Destilador (LLM)",
			Env:         []byte(`{"DISTILL_PROVIDER":"distill-cloud","LITELLM_MODEL":` + string(mDistill) + `}`)},
		{Name: provDistillLocal, Capability: capDestilar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "distill", App: "distill",
			Description: "Destilador (LLM)",
			RunnerURL:   runnerURL,
			Env:         []byte(`{"DISTILL_PROVIDER":"distill-vpc"}`)}, // model/engine from host LiteLLM config
		// gate_barato / gate_rico: cascade gates (rules -> profile -> LLM-judge).
		// SIFT_GATE names the gate capability (unchanged); SIFT_PROVIDER is the placement identity.
		{Name: provGateBarato, Capability: capGateBarato, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Constraints: thirdParty, Enabled: true, Worker: "sift", App: "gate",
			Description: "Filtro — metadados (barato)",
			Env:         []byte(`{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"sift-cloud","LITELLM_MODEL":` + string(mGate) + `}`)},
		{Name: provGateBaratoLocal, Capability: capGateBarato, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "sift", App: "gate",
			Description: "Filtro — metadados (barato)",
			RunnerURL:   runnerURL,
			Env:         []byte(`{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"sift-vpc"}`)},
		{Name: provGateRico, Capability: capGateRico, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Constraints: thirdParty, Enabled: true, Worker: "assay", App: "gate",
			Description: "Filtro — texto completo (rico)",
			Env:         []byte(`{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"assay-cloud","LITELLM_MODEL":` + string(mGate) + `}`)},
		{Name: provGateRicoLocal, Capability: capGateRico, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "assay", App: "gate",
			Description: "Filtro — texto completo (rico)",
			RunnerURL:   runnerURL,
			Env:         []byte(`{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"assay-vpc"}`)},
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
		// Shared LLM steps (already established).
		{capGateBarato, fallbackJSON(provGateBaratoLocal, provGateBarato)},
		{capGateRico, fallbackJSON(provGateRicoLocal, provGateRico)},
		{capDestilar, fallbackJSON(provDistillLocal, provDistill)},
		// Per-lane workers — VPC before cloud, lane isolation preserved by accepts constraints.
		{capColetar, fallbackJSON(
			provHarvestLocal, provHarvest,
			provShelfLocal, provShelf,
			provDialLocal, provDial,
			provFeedLocal, provFeed,
			provCourierLocal, provCourier,
			provClipLocal, provBrightDataLinked,
		)},
		{capTranscrever, fallbackJSON(provEchoLocal, provASRDirectAudio, provASRYouTube)},
		{capExtrair, fallbackJSON(
			provWinnowLocal, provExtrairEmail,
			provGleanLocal, provExtrairNews,
			provScrubLocal, provExtrairLinked,
		)},
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
// YouTube-specific collectors (harvest, shelf) and the residential-constrained transcriber
// (caption-mac, app=transcribe), and the youtube flow. Idempotent: safe to call on every boot.
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
	runnerURL, vpcEnabled := vpcRunner()
	// YouTube-specific providers.
	providers := []Provider{
		// coletar: YouTube Data API (key) and OAuth playlists — cheap, fast metadata reads.
		{Name: provHarvest, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: "harvest", App: "harvest", Description: "Coletor de canais (YouTube)",
			CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)},
		{Name: provHarvestLocal, Capability: capColetar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "harvest", App: "harvest", Description: "Coletor de canais (YouTube)",
			RunnerURL: runnerURL, CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)},
		{Name: provShelf, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: "shelf", App: "shelf", Description: "Coletor de playlists (YouTube)",
			CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)},
		{Name: provShelfLocal, Capability: capColetar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "shelf", App: "shelf", Description: "Coletor de playlists (YouTube)",
			RunnerURL: runnerURL, CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)},
		// transcrever: scribe (local Whisper) resident on the Mac. YouTube blocks audio download
		// from datacenter IPs — HARD residential constraint with NO datacenter fallback.
		{Name: provASRYouTube, Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident,
			Worker: "caption", App: "transcribe", Description: "Transcritor — vídeo YouTube (Mac)",
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
// direct-audio transcriber (echo-cloud, app=transcribe) and the podcast flow. The lane template
// is identical to YouTube's; only the to-text provider differs — echo-cloud runs on Cloud Run
// with NO residential constraint (the enclosure is a direct CDN mp3, not a yt-dlp download)
// and `accepts` pins it to podcast items. It reuses the gates, distill and the transcripts
// table (source_type=podcast). Idempotent.
func SeedPodcastLane(ctx context.Context, db Database) error {
	if err := seedCapabilities(ctx, db); err != nil {
		return err
	}
	if err := seedSharedProviders(ctx, db); err != nil {
		return err
	}
	runnerURL, vpcEnabled := vpcRunner()
	// coletar: rara-dial — woken on cadence; reads enabled podcast_feeds from the DB (F5).
	for _, p := range []Provider{
		{Name: provDial, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: "dial", App: "dial", Description: "Coletor de podcasts (RSS)",
			CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)},
		{Name: provDialLocal, Capability: capColetar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "dial", App: "dial", Description: "Coletor de podcasts (RSS)",
			RunnerURL: runnerURL, CollectCadenceSeconds: intPtr(86400), RetryIntervalSeconds: intPtr(1800)},
	} {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	// transcrever: direct-audio ASR — no residential constraint, accepts only podcast.
	for _, p := range []Provider{
		{Name: provASRDirectAudio, Capability: capTranscrever, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Worker: "echo", App: "transcribe", Description: "Transcritor — áudio/podcast",
			Constraints: []byte(`{"accepts":["podcast"]}`), Enabled: true},
		{Name: provEchoLocal, Capability: capTranscrever, Runtime: runtimeVPC, Activation: activationOnDemand,
			Worker: "echo", App: "transcribe", Description: "Transcritor — áudio/podcast",
			Constraints: []byte(`{"accepts":["podcast"]}`), RunnerURL: runnerURL, Enabled: vpcEnabled},
	} {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	if err := seedLaneFlow(ctx, db, podcastFlowName, lanePodcast,
		[]string{capColetar, capGateBarato, capTranscrever, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}

// SeedEmailLane writes the Email lane: shared capabilities/providers/config plus the email
// extractor (winnow-cloud) and the email flow. The lane template swaps `transcrever` for
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
	runnerURL, vpcEnabled := vpcRunner()
	// coletar: rara-courier — woken on cadence; Gmail OAuth credentials from Secret Manager.
	for _, p := range []Provider{
		{Name: provCourier, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: "courier", App: "courier", Description: "Coletor de e-mail (Gmail)",
			CollectCadenceSeconds: intPtr(21600), RetryIntervalSeconds: intPtr(1800)},
		{Name: provCourierLocal, Capability: capColetar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "courier", App: "courier", Description: "Coletor de e-mail (Gmail)",
			RunnerURL: runnerURL, CollectCadenceSeconds: intPtr(21600), RetryIntervalSeconds: intPtr(1800)},
	} {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	// extrair: deterministic HTML/quote/signature cleaning — accepts only email.
	for _, p := range []Provider{
		{Name: provExtrairEmail, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Worker: "winnow", App: "extract", Description: "Normalizador — e-mail",
			Constraints: []byte(`{"accepts":["email"]}`), Enabled: true},
		{Name: provWinnowLocal, Capability: capExtrair, Runtime: runtimeVPC, Activation: activationOnDemand,
			Worker: "winnow", App: "extract", Description: "Normalizador — e-mail",
			Constraints: []byte(`{"accepts":["email"]}`), RunnerURL: runnerURL, Enabled: vpcEnabled},
	} {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	// Preserves operator's enable across re-seeds; defaults to disabled on first seed.
	if err := seedOptInLaneFlow(ctx, db, emailFlowName, laneEmail,
		[]string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}

// SeedNewsLane writes the News lane: shared capabilities/providers/config plus the news extractor
// (glean-cloud) and the news flow. Like email, the source is already text (feed articles from
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
	runnerURL, vpcEnabled := vpcRunner()
	// coletar: rara-feed — woken on cadence; reads enabled feed_sources from DB on each wake.
	for _, p := range []Provider{
		{Name: provFeed, Capability: capColetar, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Enabled: true, Worker: "feed", App: "feed", Description: "Coletor de feeds (RSS/HN/HTML)",
			CollectCadenceSeconds: intPtr(21600), RetryIntervalSeconds: intPtr(1800)},
		{Name: provFeedLocal, Capability: capColetar, Runtime: runtimeVPC, Activation: activationOnDemand,
			Enabled: vpcEnabled, Worker: "feed", App: "feed", Description: "Coletor de feeds (RSS/HN/HTML)",
			RunnerURL: runnerURL, CollectCadenceSeconds: intPtr(21600), RetryIntervalSeconds: intPtr(1800)},
	} {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	// extrair: deterministic HTML/boilerplate cleaning — accepts only news.
	for _, p := range []Provider{
		{Name: provExtrairNews, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
			Worker: "glean", App: "extract", Description: "Normalizador — feed (artigo)",
			Constraints: []byte(`{"accepts":["news"]}`), Enabled: true},
		{Name: provGleanLocal, Capability: capExtrair, Runtime: runtimeVPC, Activation: activationOnDemand,
			Worker: "glean", App: "extract", Description: "Normalizador — feed (artigo)",
			Constraints: []byte(`{"accepts":["news"]}`), RunnerURL: runnerURL, Enabled: vpcEnabled},
	} {
		if err := db.UpsertProvider(ctx, p); err != nil {
			return err
		}
	}
	// Preserves operator's enable across re-seeds; defaults to disabled on first seed.
	if err := seedOptInLaneFlow(ctx, db, newsFlowName, laneNews,
		[]string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}
