package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
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
	// caption-mac carries the residential requirement AND accepts only youtube (so it never
	// competes for a podcast item). The router enforces both.
	if got := string(db.providers[provASRYouTube].Constraints); got != `{"requires":"residential","accepts":["youtube"]}` {
		t.Errorf("caption-mac constraints = %q, want residential + accepts youtube", got)
	}

	// Flow: single youtube lane at version 1, seeded DISABLED (opt-in lane).
	f, ok := db.flows[youtubeFlowName]
	if !ok {
		t.Fatalf("flow %q not seeded", youtubeFlowName)
	}
	if f.SourceType != laneYouTube || f.Version != 1 || f.Enabled {
		t.Errorf("flow = %+v, want youtube/v1/disabled", f)
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
	var topics []string
	if err := json.Unmarshal(p.Topics, &topics); err != nil || len(topics) == 0 {
		t.Errorf("profile v1 should seed starter topics (err=%v, topics=%v)", err, topics)
	}
}

// TestSeedSharedProviderEnv asserts the shared work providers carry the per-run NON-secret
// config their worker image reads from the environment (the dispatcher injects this on wake).
// Identity keys mirror what each main.go reads: sift -> SIFT_GATE+SIFT_PROVIDER, distill ->
// DISTILL_PROVIDER. Both cloud and VPC variants pin LITELLM_MODEL; distill-vpc also carries
// CURATE_ENGINE=litellm so it doesn't default to gemini (which requires GEMINI_API_KEY on the
// VPC host). No secrets (DATABASE_URL, API keys) — those are resolved by the host/agent.
func TestSeedSharedProviderEnv(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := map[string]string{
		provGateBarato:      `{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"sift-cloud","LITELLM_MODEL":"groq-fast"}`,
		provGateBaratoLocal: `{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"sift-vpc","LITELLM_MODEL":"groq-fast"}`,
		provGateRico:        `{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"assay-cloud","LITELLM_MODEL":"groq-fast"}`,
		provGateRicoLocal:   `{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"assay-vpc","LITELLM_MODEL":"groq-fast"}`,
		provDistill:         `{"DISTILL_PROVIDER":"distill-cloud","LITELLM_MODEL":"groq-llama"}`,
		provDistillLocal:    `{"DISTILL_PROVIDER":"distill-vpc","CURATE_ENGINE":"litellm","LITELLM_MODEL":"groq-llama"}`,
	}
	for name, wantEnv := range want {
		if got := string(db.providers[name].Env); got != wantEnv {
			t.Errorf("provider %q env = %q, want %q", name, got, wantEnv)
		}
	}
	// No secret ever leaks into env (the host/agent resolves DATABASE_URL and API keys).
	for name, p := range db.providers {
		envUpper := strings.ToUpper(string(p.Env))
		for _, secret := range []string{"DATABASE_URL", "API_KEY", "_KEY", "_SECRET", "PASSWORD", "TOKEN"} {
			if strings.Contains(envUpper, secret) {
				t.Errorf("provider %q env leaks a secret-shaped key %q: %s", name, secret, p.Env)
			}
		}
	}
}

// TestSeedIdempotent asserts re-seeding converges (no duplicate rows, stable flow id).
// TestSeedExtractTranscribeProviderEnv asserts the extract (GLEAN_PROVIDER) and
// transcribe (SCRIBE_PROVIDER) providers carry their claim-identity env for BOTH the
// cloud and VPC placements. Without it the dispatcher wakes the worker with an empty
// GLEAN_PROVIDER/SCRIBE_PROVIDER and the process log.Fatalf's on startup — the bug
// that stalled extract (glean/winnow/scrub) and echo after the VPC cutover, while
// gate/distill (already seeded with their identity env) kept running.
func TestSeedExtractTranscribeProviderEnv(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, seed := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
	} {
		if err := seed(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	want := map[string]string{
		// extract — GLEAN_PROVIDER = placement name (claim identity).
		provExtrairEmail:  `{"GLEAN_PROVIDER":"winnow-cloud"}`,
		provWinnowLocal:   `{"GLEAN_PROVIDER":"winnow-vpc"}`,
		provExtrairNews:   `{"GLEAN_PROVIDER":"glean-cloud"}`,
		provGleanLocal:    `{"GLEAN_PROVIDER":"glean-vpc"}`,
		provExtrairLinked: `{"GLEAN_PROVIDER":"scrub-cloud"}`,
		provScrubLocal:    `{"GLEAN_PROVIDER":"scrub-vpc"}`,
		// transcribe — SCRIBE_PROVIDER = placement name.
		provASRYouTube:     `{"SCRIBE_PROVIDER":"caption-mac"}`,
		provASRDirectAudio: `{"SCRIBE_PROVIDER":"echo-cloud"}`,
		provEchoLocal:      `{"SCRIBE_PROVIDER":"echo-vpc"}`,
	}
	for name, wantEnv := range want {
		if got := string(db.providers[name].Env); got != wantEnv {
			t.Errorf("provider %q env = %q, want %q", name, got, wantEnv)
		}
	}
}

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
	// 6 shared LLM providers (sift-cloud/vpc, assay-cloud/vpc, distill-cloud/vpc) + 5
	// YouTube-specific (harvest-cloud, harvest-vpc, shelf-cloud, shelf-vpc, caption-mac).
	if len(db.providers) != 11 {
		t.Errorf("expected 11 providers after re-seed, got %d", len(db.providers))
	}
	if len(db.flowSteps) != 5 {
		t.Errorf("expected 5 flow steps after re-seed, got %d", len(db.flowSteps))
	}
	// interest_profile is seeded exactly once — re-seeding must not create a v2 or error.
	if len(db.profiles) != 1 {
		t.Errorf("expected interest_profile seeded once, got %d versions", len(db.profiles))
	}
}

// TestVPCFirstRoutingPolicy asserts that VPC-first routing is enforced via per-capability
// routing_policies.fallback after seeding — the VPC variant must appear before its cloud peer
// in the fallback list for each LLM capability.
func TestVPCFirstRoutingPolicy(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := []struct {
		scope string
		vpc   string
		cloud string
	}{
		{capGateBarato, provGateBaratoLocal, provGateBarato},
		{capGateRico, provGateRicoLocal, provGateRico},
		{capDestilar, provDistillLocal, provDistill},
	}
	for _, tt := range cases {
		pol, ok, err := db.GetRoutingPolicy(ctx, tt.scope)
		if err != nil || !ok {
			t.Errorf("routing policy for %q: ok=%v err=%v", tt.scope, ok, err)
			continue
		}
		var fallback []string
		if err := json.Unmarshal(pol.Fallback, &fallback); err != nil || len(fallback) < 2 {
			t.Errorf("%q fallback %q: want JSON array with ≥2 entries", tt.scope, pol.Fallback)
			continue
		}
		if fallback[0] != tt.vpc {
			t.Errorf("%q fallback[0] = %q, want %q (VPC must be first)", tt.scope, fallback[0], tt.vpc)
		}
		if fallback[1] != tt.cloud {
			t.Errorf("%q fallback[1] = %q, want %q (cloud must be second)", tt.scope, fallback[1], tt.cloud)
		}
	}
}

// TestLocalProvidersAreOnDemand asserts the three VPC shared providers (sift-vpc,
// assay-vpc, distill-vpc) are seeded as on_demand, not resident. They follow the
// spawn-and-exit model: woken per-item by rara-runner, not polling continuously. The router
// exempts on_demand from the heartbeat health gate, so a stale timestamp never excludes them.
func TestLocalProvidersAreOnDemand(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, name := range []string{provDistillLocal, provGateBaratoLocal, provGateRicoLocal} {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("provider %q not seeded", name)
			continue
		}
		if p.Activation != activationOnDemand {
			t.Errorf("provider %q activation = %q, want on_demand (spawn-and-exit model; resident would stale-exclude on aged heartbeat)", name, p.Activation)
		}
	}
}

// TestSeedYouTubeLanePreservesEnabled ensures a re-seed never silently disables a lane the
// operator already enabled (the opt-in contract).
func TestSeedYouTubeLanePreservesEnabled(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Simulate operator enabling the lane via the public API.
	f, found, err := db.GetFlow(ctx, youtubeFlowName)
	if err != nil || !found {
		t.Fatalf("GetFlow: err=%v found=%v", err, found)
	}
	f.Enabled = true
	if _, err := db.UpsertFlow(ctx, f); err != nil {
		t.Fatalf("UpsertFlow: %v", err)
	}

	// Re-seed must not flip it back to disabled.
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	f, found, err = db.GetFlow(ctx, youtubeFlowName)
	if err != nil || !found {
		t.Fatalf("GetFlow after re-seed: err=%v found=%v", err, found)
	}
	if !f.Enabled {
		t.Error("re-seed flipped operator-enabled youtube flow back to disabled")
	}
}

// TestCollectorCadencesSeeded verifies every scheduled collector provider has
// collect_cadence_seconds set and is enabled after each lane seed. The dispatcher relies on
// both: a nil cadence means never woken; disabled means skipped by ListDueCollectors.
func TestCollectorCadencesSeeded(t *testing.T) {
	const (
		cadenceDaily = 86400 // harvest, shelf, dial
		cadence6h    = 21600 // feed, courier, clip
	)

	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	collectors := map[string]int{ // provider name -> expected cadence seconds
		provHarvest:          cadenceDaily,
		provShelf:            cadenceDaily,
		provDial:             cadenceDaily,
		provFeed:             cadence6h,
		provCourier:          cadence6h,
		provBrightDataLinked: cadence6h, // "clip" — rara-clip job
	}
	for name, wantCadence := range collectors {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("collector provider %q not seeded", name)
			continue
		}
		if !p.Enabled {
			t.Errorf("provider %q: Enabled = false, want true", name)
		}
		if p.CollectCadenceSeconds == nil {
			t.Errorf("provider %q: CollectCadenceSeconds is nil, want %d", name, wantCadence)
			continue
		}
		if *p.CollectCadenceSeconds != wantCadence {
			t.Errorf("provider %q: cadence = %d, want %d", name, *p.CollectCadenceSeconds, wantCadence)
		}
	}
}

// TestSeedPreservesHeartbeatAtOnReseed guards against re-seed wiping the heartbeat that
// TouchProviderHeartbeat stamps. The runner touches providers.heartbeat_at on each wake
// (proof of life); seed must never zero it, or the router's health gate will exclude a
// healthy provider until it next wakes.
func TestSeedPreservesHeartbeatAtOnReseed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Simulate runner stamping heartbeat on distill-vpc after first seed.
	if err := db.TouchProviderHeartbeat(ctx, provDistillLocal); err != nil {
		t.Fatal(err)
	}
	if db.providers[provDistillLocal].HeartbeatAt == nil {
		t.Fatal("precondition: HeartbeatAt should be set after TouchProviderHeartbeat")
	}
	// Re-seed must NOT zero heartbeat_at.
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if db.providers[provDistillLocal].HeartbeatAt == nil {
		t.Error("re-seed zeroed HeartbeatAt on distill-vpc; TouchProviderHeartbeat's stamp must survive re-seed")
	}
}

// TestSeedPreservesLastCollectAtOnReseed guards against re-seed zeroing last_collect_at on
// collector providers. last_collect_at is stamped by the dispatcher after each successful
// collector wake; zeroing it resets the cadence clock (rara-harvest/dial/feed all run again
// immediately instead of waiting their daily/6h window). Mirrors TestSeedPreservesHeartbeatAtOnReseed.
func TestSeedPreservesLastCollectAtOnReseed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Simulate dispatcher stamping last_collect_at on each collector after first seed.
	now := time.Now()
	collectors := []string{provHarvest, provShelf, provDial, provFeed, provCourier}
	for _, name := range collectors {
		if _, ok := db.providers[name]; !ok {
			t.Fatalf("precondition: provider %q not seeded", name)
		}
		p := db.providers[name]
		p.LastCollectAt = &now
		db.providers[name] = p
	}
	// Re-seed must NOT zero last_collect_at (cadence clock must survive).
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
	}
	for _, name := range collectors {
		if db.providers[name].LastCollectAt == nil {
			t.Errorf("re-seed zeroed LastCollectAt on %q; dispatcher cadence clock must survive re-seed", name)
		}
	}
}

// TestSeedCollectorRetryIntervalSeeded verifies every scheduled collector has
// retry_interval_seconds=1800 after seeding, and that non-collector providers leave it nil.
// Mirrors TestCollectorCadencesSeeded.
func TestSeedCollectorRetryIntervalSeeded(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	const wantRetry = 1800
	collectors := []string{provHarvest, provShelf, provDial, provFeed, provCourier, provBrightDataLinked}
	for _, name := range collectors {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("collector provider %q not seeded", name)
			continue
		}
		if p.RetryIntervalSeconds == nil {
			t.Errorf("provider %q: RetryIntervalSeconds is nil, want %d", name, wantRetry)
		} else if *p.RetryIntervalSeconds != wantRetry {
			t.Errorf("provider %q: RetryIntervalSeconds = %d, want %d", name, *p.RetryIntervalSeconds, wantRetry)
		}
	}
	// Non-collectors must not have a retry interval set.
	nonCollectors := []string{provDistill, provDistillLocal, provGateBarato, provGateBaratoLocal, provGateRico, provGateRicoLocal}
	for _, name := range nonCollectors {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("provider %q not seeded", name)
			continue
		}
		if p.RetryIntervalSeconds != nil {
			t.Errorf("provider %q: RetryIntervalSeconds = %d, want nil (non-collector)", name, *p.RetryIntervalSeconds)
		}
	}
}

// TestSeedPreservesLastAttemptAtOnReseed guards against re-seed zeroing last_attempt_at on
// collector providers. last_attempt_at is stamped by the dispatcher on every wake attempt;
// zeroing it resets the retry throttle (a collector in backoff would be dispatched immediately).
// Mirrors TestSeedPreservesLastCollectAtOnReseed.
func TestSeedPreservesLastAttemptAtOnReseed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Simulate dispatcher stamping last_attempt_at on each collector after first seed.
	now := time.Now()
	collectors := []string{provHarvest, provShelf, provDial, provFeed, provCourier, provBrightDataLinked}
	for _, name := range collectors {
		if _, ok := db.providers[name]; !ok {
			t.Fatalf("precondition: provider %q not seeded", name)
		}
		p := db.providers[name]
		p.LastAttemptAt = &now
		db.providers[name] = p
	}
	// Re-seed must NOT zero last_attempt_at (retry throttle must survive).
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
	}
	for _, name := range collectors {
		if db.providers[name].LastAttemptAt == nil {
			t.Errorf("re-seed zeroed LastAttemptAt on %q; dispatcher retry throttle must survive re-seed", name)
		}
	}
}

// TestSeedWorkerGrouping asserts that paired cloud/VPC providers share the same Worker
// codename (the binary that implements both), and that each placement's Worker differs from
// its Name (placement name = <worker>-<runtime>; worker = logical binary codename).
func TestSeedWorkerGrouping(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Pairs: cloud and VPC placement must share the same Worker codename.
	pairs := [][3]string{ // [cloud_name, vpc_name, expected_worker]
		{provDistill, provDistillLocal, "distill"},
		{provGateBarato, provGateBaratoLocal, "sift"},
		{provGateRico, provGateRicoLocal, "assay"},
	}
	for _, pair := range pairs {
		cloud := db.providers[pair[0]]
		local := db.providers[pair[1]]
		if cloud.Worker != local.Worker {
			t.Errorf("pair (%q, %q): Worker mismatch %q vs %q; both must share the same worker",
				pair[0], pair[1], cloud.Worker, local.Worker)
		}
		if cloud.Worker != pair[2] {
			t.Errorf("pair (%q, %q): Worker = %q, want %q",
				pair[0], pair[1], cloud.Worker, pair[2])
		}
	}
	// Single-placement providers each have their own Worker codename (differs from Name).
	singles := map[string]string{ // placement_name -> expected_worker
		provHarvest:    "harvest",
		provShelf:      "shelf",
		provASRYouTube: "caption",
	}
	for name, wantWorker := range singles {
		if p := db.providers[name]; p.Worker != wantWorker {
			t.Errorf("provider %q: Worker = %q, want %q", name, p.Worker, wantWorker)
		}
	}
}

// TestSeedWorkerAndAppRoundTrip asserts UpsertProvider + GetProvider preserves both
// Worker and App, and that the App guard defaults App to Name when left empty.
func TestSeedWorkerAndAppRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := seedCapabilities(ctx, db); err != nil {
		t.Fatal(err)
	}

	// Explicit App: round-trip preserves both Worker and App.
	p := Provider{
		Name: "distill", Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: true, Worker: "distill", App: "distill",
	}
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	got, ok, err := db.GetProvider(ctx, "distill")
	if err != nil || !ok {
		t.Fatalf("GetProvider: ok=%v err=%v", ok, err)
	}
	if got.Worker != "distill" {
		t.Errorf("Worker = %q, want %q", got.Worker, "distill")
	}
	if got.App != "distill" {
		t.Errorf("App = %q, want %q", got.App, "distill")
	}

	// Empty App: guard must default it to Name.
	p2 := Provider{
		Name: "distill", Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: true, Worker: "distill",
	}
	if err := db.UpsertProvider(ctx, p2); err != nil {
		t.Fatalf("UpsertProvider (empty App): %v", err)
	}
	if db.providers["distill"].App != "distill" {
		t.Errorf("App guard: got %q, want %q", db.providers["distill"].App, "distill")
	}
}

// TestSeedAllProvidersHaveApp asserts every seeded provider has a non-empty App (the
// dispatch target that P1b decoupled from Name). After P2b-gate-B, gate providers
// (sift/assay) share the consolidated 'gate' app; Name remains the <worker>-<runtime> codename.
func TestSeedAllProvidersHaveApp(t *testing.T) {
	t.Setenv("DISTILL_MODEL", "groq-llama")
	t.Setenv("GATE_MODEL", "groq-fast")

	ctx := context.Background()
	db := newMockDatabase()

	seedFns := []func(context.Context, Database) error{
		SeedYouTubeLane,
		SeedPodcastLane,
		SeedEmailLane,
		SeedNewsLane,
		SeedLinkedInLane,
	}
	for _, fn := range seedFns {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for name, p := range db.providers {
		if p.App == "" {
			t.Errorf("provider %q: App is empty; every provider must have a dispatch target", name)
		}
	}
	// Spot-check: App is the consolidated job/image name for dispatch.
	wantApp := map[string]string{
		provDistill:         "distill",
		provDistillLocal:    "distill",
		provGateBarato:      "gate",
		provGateBaratoLocal: "gate",
		provGateRico:        "gate",
		provGateRicoLocal:   "gate",
		provASRYouTube:      "transcribe",
		provASRDirectAudio:  "transcribe",
		provExtrairEmail:    "extract",
		provExtrairNews:     "extract",
	}
	for name, wantA := range wantApp {
		if p := db.providers[name]; p.App != wantA {
			t.Errorf("provider %q: App = %q, want %q", name, p.App, wantA)
		}
	}
}

// TestVPCRunnerURLValidation verifies that vpcRunner() only enables VPC mode for valid bare
// http/https URLs, rejecting credentials-embedded URLs, non-http schemes, and query strings —
// the dispatcher POSTs to runner_url with a Bearer token so malformed URLs must be rejected at
// seed time, not stored silently.
func TestVPCRunnerURLValidation(t *testing.T) {
	cases := []struct {
		env     string
		wantURL string
		enabled bool
	}{
		{"http://100.66.254.24:9000", "http://100.66.254.24:9000", true},
		{"https://runner.tailnet:8080", "https://runner.tailnet:8080", true},
		{"", "", false},                             // unset → disabled
		{"ftp://runner:9000", "", false},            // wrong scheme
		{"http://user:pass@runner:9000", "", false}, // embedded credentials
		{"http://runner:9000?key=val", "", false},   // query string
		{"http://runner:9000#frag", "", false},      // fragment
		{"not-a-url", "", false},                    // not a URL
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("RUNNER_LOCAL_URL", tc.env)
			gotURL, gotEnabled := vpcRunner()
			if gotEnabled != tc.enabled {
				t.Errorf("env=%q: enabled=%v, want %v", tc.env, gotEnabled, tc.enabled)
			}
			if gotURL != tc.wantURL {
				t.Errorf("env=%q: url=%q, want %q", tc.env, gotURL, tc.wantURL)
			}
		})
	}
}

// TestSeedVPCLocalProvidersGetRunnerURLFromEnv guards against re-seed zeroing runner_url on
// the three VPC on_demand providers. runner_url is the tailnet endpoint the dispatcher POSTs
// to wake a worker; zeroing it causes "no transport path" dispatch failures.
func TestSeedVPCLocalProvidersGetRunnerURLFromEnv(t *testing.T) {
	const wantURL = "http://100.66.254.24:9000"
	t.Setenv("RUNNER_LOCAL_URL", wantURL)

	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{provDistillLocal, provGateBaratoLocal, provGateRicoLocal} {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("provider %q not seeded", name)
			continue
		}
		if p.RunnerURL != wantURL {
			t.Errorf("provider %q: RunnerURL = %q, want %q (from RUNNER_LOCAL_URL)", name, p.RunnerURL, wantURL)
		}
	}
}

// TestSeedProviderDescriptions asserts every seeded provider carries a non-empty human-readable
// description (config-as-data for the console UI). Spot-checks a few key placements.
func TestSeedProviderDescriptions(t *testing.T) {
	t.Setenv("DISTILL_MODEL", "groq-llama")
	t.Setenv("GATE_MODEL", "groq-fast")

	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range []func(context.Context, Database) error{
		SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
	} {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for name, p := range db.providers {
		if p.Description == "" {
			t.Errorf("provider %q: Description is empty; every provider must have a description", name)
		}
	}
	// Spot-check a few key placements.
	wantDesc := map[string]string{
		provDistill:         "Destilador (LLM)",
		provDistillLocal:    "Destilador (LLM)",
		provGateBarato:      "Filtro — metadados (barato)",
		provGateBaratoLocal: "Filtro — metadados (barato)",
		provGateRico:        "Filtro — texto completo (rico)",
		provASRYouTube:      "Transcritor — vídeo YouTube (Mac)",
		provASRDirectAudio:  "Transcritor — áudio/podcast",
		provExtrairEmail:    "Normalizador — e-mail",
	}
	for name, want := range wantDesc {
		if p := db.providers[name]; p.Description != want {
			t.Errorf("provider %q: Description = %q, want %q", name, p.Description, want)
		}
	}
}

// allSeedFns is every lane seeder, used by tests that need the full provider universe.
var allSeedFns = []func(context.Context, Database) error{
	SeedYouTubeLane, SeedPodcastLane, SeedEmailLane, SeedNewsLane, SeedLinkedInLane,
}

// assertVPCMirrorsCloud checks that a VPC provider is correctly seeded as a clone of its cloud
// sibling: same Worker/App/Capability/Constraints/Description/cadence/retry, runtime=vpc,
// activation=on_demand, Enabled=true, RunnerURL=wantURL.
func assertVPCMirrorsCloud(t *testing.T, vpcName, cloudName string, vpc, cloud Provider, wantURL string) {
	t.Helper()
	if !vpc.Enabled {
		t.Errorf("provider %q: Enabled=false with RUNNER_LOCAL_URL set", vpcName)
	}
	if vpc.Runtime != runtimeVPC {
		t.Errorf("provider %q: Runtime=%q, want %q", vpcName, vpc.Runtime, runtimeVPC)
	}
	if vpc.Activation != activationOnDemand {
		t.Errorf("provider %q: Activation=%q, want %q", vpcName, vpc.Activation, activationOnDemand)
	}
	if vpc.RunnerURL != wantURL {
		t.Errorf("provider %q: RunnerURL=%q, want %q", vpcName, vpc.RunnerURL, wantURL)
	}
	if vpc.Worker != cloud.Worker {
		t.Errorf("provider %q: Worker=%q, want %q (same as cloud sibling)", vpcName, vpc.Worker, cloud.Worker)
	}
	if vpc.App != cloud.App {
		t.Errorf("provider %q: App=%q, want %q (same as cloud sibling)", vpcName, vpc.App, cloud.App)
	}
	if vpc.Capability != cloud.Capability {
		t.Errorf("provider %q: Capability=%q, want %q (same as cloud sibling)", vpcName, vpc.Capability, cloud.Capability)
	}
	if string(vpc.Constraints) != string(cloud.Constraints) {
		t.Errorf("provider %q: Constraints=%q, want %q (same as cloud sibling)", vpcName, vpc.Constraints, cloud.Constraints)
	}
	if vpc.Description != cloud.Description {
		t.Errorf("provider %q: Description=%q, want %q (same as cloud sibling)", vpcName, vpc.Description, cloud.Description)
	}
	// Scheduling fields: collectors carry cadence/retry so the dispatcher can wake the VPC
	// variant on the same schedule as the cloud one.
	cadenceEqual := (vpc.CollectCadenceSeconds == nil) == (cloud.CollectCadenceSeconds == nil) &&
		(vpc.CollectCadenceSeconds == nil || *vpc.CollectCadenceSeconds == *cloud.CollectCadenceSeconds)
	if !cadenceEqual {
		t.Errorf("provider %q: CollectCadenceSeconds mismatch with cloud sibling %q", vpcName, cloudName)
	}
	retryEqual := (vpc.RetryIntervalSeconds == nil) == (cloud.RetryIntervalSeconds == nil) &&
		(vpc.RetryIntervalSeconds == nil || *vpc.RetryIntervalSeconds == *cloud.RetryIntervalSeconds)
	if !retryEqual {
		t.Errorf("provider %q: RetryIntervalSeconds mismatch with cloud sibling %q", vpcName, cloudName)
	}
}

// TestSeedNewVPCProvidersWithRunnerURL verifies that all per-lane VPC provider variants are
// seeded enabled=true, runtime=vpc, activation=on_demand, RunnerURL filled, and mirror their
// cloud sibling's Worker/App/Capability/Constraints/Description/cadence/retry, when
// RUNNER_LOCAL_URL is set (TestMain sets it globally for the test binary).
func TestSeedNewVPCProvidersWithRunnerURL(t *testing.T) {
	const wantURL = "http://test-runner:8080" // value from TestMain

	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range allSeedFns {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	pairs := [][2]string{
		{provHarvestLocal, provHarvest},
		{provShelfLocal, provShelf},
		{provDialLocal, provDial},
		{provFeedLocal, provFeed},
		{provCourierLocal, provCourier},
		{provClipLocal, provBrightDataLinked},
		{provEchoLocal, provASRDirectAudio},
		{provWinnowLocal, provExtrairEmail},
		{provGleanLocal, provExtrairNews},
		{provScrubLocal, provExtrairLinked},
	}

	for _, p := range pairs {
		vpcName, cloudName := p[0], p[1]
		vpc, vpcOK := db.providers[vpcName]
		cloud, cloudOK := db.providers[cloudName]
		if !vpcOK {
			t.Errorf("provider %q not seeded", vpcName)
			continue
		}
		if !cloudOK {
			t.Errorf("cloud sibling %q not seeded (precondition)", cloudName)
			continue
		}
		assertVPCMirrorsCloud(t, vpcName, cloudName, vpc, cloud, wantURL)
	}
}

// TestSeedNewVPCProvidersDisabledWithoutRunnerURL verifies that all per-lane VPC provider
// variants are seeded enabled=false when RUNNER_LOCAL_URL is empty.
func TestSeedNewVPCProvidersDisabledWithoutRunnerURL(t *testing.T) {
	t.Setenv("RUNNER_LOCAL_URL", "") // override TestMain's value

	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range allSeedFns {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	allVPC := []string{
		provHarvestLocal, provShelfLocal, provDialLocal, provFeedLocal,
		provCourierLocal, provClipLocal, provEchoLocal,
		provWinnowLocal, provGleanLocal, provScrubLocal,
		// existing LLM VPC providers must also stay disabled
		provDistillLocal, provGateBaratoLocal, provGateRicoLocal,
	}
	for _, name := range allVPC {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("provider %q not seeded", name)
			continue
		}
		if p.Enabled {
			t.Errorf("provider %q: Enabled=true without RUNNER_LOCAL_URL; must be disabled until OPS sets the env", name)
		}
	}
}

// TestVPCFirstRoutingPolicyNewWorkers verifies that each affected capability has a routing
// policy with the VPC variant before its cloud sibling, and that the lane-isolation constraints
// are preserved (the accepts filter keeps workers separated even when all are in one policy).
func TestVPCFirstRoutingPolicyNewWorkers(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range allSeedFns {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	type pair struct{ vpc, cloud string }
	cases := []struct {
		capability string
		pairs      []pair
	}{
		{capColetar, []pair{
			{provHarvestLocal, provHarvest},
			{provShelfLocal, provShelf},
			{provDialLocal, provDial},
			{provFeedLocal, provFeed},
			{provCourierLocal, provCourier},
			{provClipLocal, provBrightDataLinked},
		}},
		{capTranscrever, []pair{
			{provEchoLocal, provASRDirectAudio},
		}},
		{capExtrair, []pair{
			{provWinnowLocal, provExtrairEmail},
			{provGleanLocal, provExtrairNews},
			{provScrubLocal, provExtrairLinked},
		}},
	}

	for _, tc := range cases {
		pol, ok, err := db.GetRoutingPolicy(ctx, tc.capability)
		if err != nil || !ok {
			t.Errorf("routing policy for %q: ok=%v err=%v", tc.capability, ok, err)
			continue
		}
		var fallback []string
		if err := json.Unmarshal(pol.Fallback, &fallback); err != nil || len(fallback) == 0 {
			t.Errorf("%q fallback %q: want non-empty JSON array", tc.capability, pol.Fallback)
			continue
		}
		pos := make(map[string]int, len(fallback))
		for i, name := range fallback {
			pos[name] = i
		}
		for _, p := range tc.pairs {
			vpcIdx, vpcOK := pos[p.vpc]
			cloudIdx, cloudOK := pos[p.cloud]
			if !vpcOK {
				t.Errorf("%q fallback: %q not found", tc.capability, p.vpc)
				continue
			}
			if !cloudOK {
				t.Errorf("%q fallback: %q not found", tc.capability, p.cloud)
				continue
			}
			if vpcIdx >= cloudIdx {
				t.Errorf("%q fallback: %q (pos %d) must come before %q (pos %d)", tc.capability, p.vpc, vpcIdx, p.cloud, cloudIdx)
			}
		}
	}
}

// TestCaptionStashUnchangedByVPCSeed verifies that the residential caption-mac provider and
// the vpc-resident stash provider are not given a VPC placement (they are already correct and
// excluded from this migration wave).
func TestCaptionStashUnchangedByVPCSeed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range allSeedFns {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// caption-mac must stay runtime=local / activation=resident (residential constraint — no VPC).
	caption := db.providers[provASRYouTube]
	if caption.Runtime != runtimeLocal {
		t.Errorf("caption-mac: Runtime=%q, want %q (must stay residential-local)", caption.Runtime, runtimeLocal)
	}
	if caption.Activation != activationResident {
		t.Errorf("caption-mac: Activation=%q, want %q", caption.Activation, activationResident)
	}
	// No "caption-vpc" placement must exist.
	if _, exists := db.providers["caption-vpc"]; exists {
		t.Error("caption-vpc must NOT be seeded (residential constraint requires Mac IP)")
	}

	// stash must stay runtime=vpc / activation=resident (it is already VPC surface — no on_demand twin).
	stash := db.providers[provManualInbox]
	if stash.Runtime != runtimeVPC {
		t.Errorf("stash: Runtime=%q, want %q", stash.Runtime, runtimeVPC)
	}
	if stash.Activation != activationResident {
		t.Errorf("stash: Activation=%q, want %q", stash.Activation, activationResident)
	}
}

// TestVPCProviderEnvCarriesEngineAndModel asserts that the three LLM VPC providers carry the
// engine selector and model in their body env — without these the worker falls back to the gemini
// engine (which requires GEMINI_API_KEY, absent on the VPC host) and crashes on boot.
// TestMain sets DISTILL_MODEL and GATE_MODEL; this test reads them so the expected values track
// whatever TestMain configures rather than being hardcoded twice.
func TestVPCProviderEnvCarriesEngineAndModel(t *testing.T) {
	distillModel := os.Getenv("DISTILL_MODEL")
	gateModel := os.Getenv("GATE_MODEL")
	if distillModel == "" || gateModel == "" {
		t.Skip("DISTILL_MODEL and GATE_MODEL must be set (TestMain sets them for the full suite)")
	}

	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type wantEnv struct {
		curateEngine string // empty = not required (gate has no CURATE_ENGINE)
		litellmModel string
		identity     string // provider-specific identity key=value to check
	}
	cases := map[string]wantEnv{
		provDistillLocal:    {curateEngine: "litellm", litellmModel: distillModel, identity: `"DISTILL_PROVIDER":"distill-vpc"`},
		provGateBaratoLocal: {litellmModel: gateModel, identity: `"SIFT_PROVIDER":"sift-vpc"`},
		provGateRicoLocal:   {litellmModel: gateModel, identity: `"SIFT_PROVIDER":"assay-vpc"`},
	}

	for name, want := range cases {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("provider %q not seeded", name)
			continue
		}
		env := string(p.Env)
		if !strings.Contains(env, `"LITELLM_MODEL":"`+want.litellmModel+`"`) {
			t.Errorf("provider %q env missing LITELLM_MODEL=%q: %s", name, want.litellmModel, env)
		}
		if want.curateEngine != "" && !strings.Contains(env, `"CURATE_ENGINE":"`+want.curateEngine+`"`) {
			t.Errorf("provider %q env missing CURATE_ENGINE=%q: %s", name, want.curateEngine, env)
		}
		if !strings.Contains(env, want.identity) {
			t.Errorf("provider %q env missing identity %q: %s", name, want.identity, env)
		}
	}
}

// TestCollectorAcceptsConstraints verifies that every scheduled collector provider carries an
// accepts constraint restricting it to its own lane. This prevents cross-lane assignment if
// the item router ever evaluates coletar steps, and makes the capColetar routing policy safe
// to group all collectors in a single fallback list.
func TestCollectorAcceptsConstraints(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, fn := range allSeedFns {
		if err := fn(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Each scheduled collector must declare exactly which lane it accepts.
	wantAccepts := map[string]string{
		provHarvest:          `{"accepts":["youtube"]}`,
		provHarvestLocal:     `{"accepts":["youtube"]}`,
		provShelf:            `{"accepts":["youtube"]}`,
		provShelfLocal:       `{"accepts":["youtube"]}`,
		provDial:             `{"accepts":["podcast"]}`,
		provDialLocal:        `{"accepts":["podcast"]}`,
		provFeed:             `{"accepts":["news"]}`,
		provFeedLocal:        `{"accepts":["news"]}`,
		provCourier:          `{"accepts":["email"]}`,
		provCourierLocal:     `{"accepts":["email"]}`,
		provBrightDataLinked: `{"accepts":["linkedin"]}`,
		provClipLocal:        `{"accepts":["linkedin"]}`,
	}
	for name, wantConstraints := range wantAccepts {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("collector %q not seeded", name)
			continue
		}
		if got := string(p.Constraints); got != wantConstraints {
			t.Errorf("collector %q: Constraints=%q, want %q", name, got, wantConstraints)
		}
	}
}
