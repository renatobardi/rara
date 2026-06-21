package main

import (
	"context"
	"encoding/json"
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
	// asr-youtube carries the residential requirement AND accepts only youtube (so it never
	// competes for a podcast item). The router enforces both.
	if got := string(db.providers[provASRYouTube].Constraints); got != `{"requires":"residential","accepts":["youtube"]}` {
		t.Errorf("asr-youtube constraints = %q, want residential + accepts youtube", got)
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
// DISTILL_PROVIDER; the cloud variants also pin LITELLM_MODEL (the value baked in the deploy
// YAML today). No secrets (DATABASE_URL, API keys) — those are resolved by the host/agent.
func TestSeedSharedProviderEnv(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := map[string]string{
		provGateBarato:      `{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"gate-barato","LITELLM_MODEL":"groq-fast"}`,
		provGateBaratoLocal: `{"SIFT_GATE":"gate_barato","SIFT_PROVIDER":"gate-barato-local"}`,
		provGateRico:        `{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"gate-rico","LITELLM_MODEL":"groq-fast"}`,
		provGateRicoLocal:   `{"SIFT_GATE":"gate_rico","SIFT_PROVIDER":"gate-rico-local"}`,
		provDistill:         `{"DISTILL_PROVIDER":"distill","LITELLM_MODEL":"groq-llama"}`,
		provDistillLocal:    `{"DISTILL_PROVIDER":"distill-local"}`, // model/engine from host LiteLLM config
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
	// YouTube-specific (harvest, shelf, asr-youtube). The learning-loop reviser is no longer a
	// control-plane provider — it moved out to rara-hone (a periodic job, off the routing path).
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

// TestLocalProvidersAreOnDemand asserts the three VPC-local shared providers (gate-barato-local,
// gate-rico-local, distill-local) are seeded as on_demand, not resident. They follow the
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
	// Simulate runner stamping heartbeat on distill-local after first seed.
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
		t.Error("re-seed zeroed HeartbeatAt on distill-local; TouchProviderHeartbeat's stamp must survive re-seed")
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

// TestSeedWorkerGrouping asserts that paired cloud/VPC providers share the same Worker value
// (the -local suffix is stripped) and that standalone providers keep their own name as Worker.
func TestSeedWorkerGrouping(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Pairs: both sides must collapse to the same worker name.
	pairs := [][2]string{
		{provDistill, provDistillLocal},
		{provGateBarato, provGateBaratoLocal},
		{provGateRico, provGateRicoLocal},
	}
	for _, pair := range pairs {
		cloud := db.providers[pair[0]]
		local := db.providers[pair[1]]
		if cloud.Worker != local.Worker {
			t.Errorf("pair (%q, %q): Worker mismatch %q vs %q; both must share the same worker",
				pair[0], pair[1], cloud.Worker, local.Worker)
		}
		if cloud.Worker != pair[0] {
			t.Errorf("pair (%q, %q): Worker = %q, want %q (logical name without -local)",
				pair[0], pair[1], cloud.Worker, pair[0])
		}
	}
	// Standalone providers: Worker == own name.
	for _, name := range []string{provHarvest, provShelf, provASRYouTube} {
		if p := db.providers[name]; p.Worker != name {
			t.Errorf("provider %q: Worker = %q, want %q", name, p.Worker, name)
		}
	}
}

// TestSeedWorkerRoundTrip asserts UpsertProvider + GetProvider preserves Worker.
func TestSeedWorkerRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := seedCapabilities(ctx, db); err != nil {
		t.Fatal(err)
	}
	p := Provider{
		Name: "distill", Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: true, Worker: "distill",
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
}

// TestSeedAppRoundTrip asserts UpsertProvider + GetProvider preserves App.
func TestSeedAppRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := seedCapabilities(ctx, db); err != nil {
		t.Fatal(err)
	}
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
	if got.App != "distill" {
		t.Errorf("App = %q, want %q", got.App, "distill")
	}
}

// TestSeedAppDefaultsToName asserts that upserting with App="" stores app = name.
func TestSeedAppDefaultsToName(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := seedCapabilities(ctx, db); err != nil {
		t.Fatal(err)
	}
	p := Provider{
		Name: "distill", Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: true, Worker: "distill",
		// App intentionally empty — guard must default it to Name
	}
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	got := db.providers["distill"]
	if got.App != "distill" {
		t.Errorf("App = %q, want %q (guard must default App to Name)", got.App, "distill")
	}
}

// TestSeedAppEqualsNameForAllProviders asserts every seeded provider has App == Name
// (the P1a invariant: app = name as long as app/name haven't diverged yet).
func TestSeedAppEqualsNameForAllProviders(t *testing.T) {
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
		if p.App != name {
			t.Errorf("provider %q: App = %q, want %q", name, p.App, name)
		}
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
