package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Router tests run on the pure rankProviders / scoreProviders functions and on Router.Select
// over the MockDatabase — zero I/O. They cover the four Phase 2 selection behaviours:
// cost<->quality weighting, hard-constraint filtering, health (heartbeat) exclusion, and
// ordered fallback.

// routerClock is a fixed "now" for the health gate; fresh/stale heartbeats are derived from
// it so the tests never touch the wall clock.
var routerClock = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

const routerHealthTTL = 5 * time.Minute

// routerItem is a neutral item for the existing selection tests: a public YouTube item. The
// providers those tests build declare no `accepts`/`sensitivity`, so the item's facets do not
// perturb them; the accepts/sensitivity behaviour is covered by its own tests below.
var routerItem = Item{Lane: laneYouTube, Sensitivity: sensitivityPublic}

func ptime(t time.Time) *time.Time { return &t }

// residential is the JSON constraint asr-youtube carries.
var residential = json.RawMessage(`{"requires":"residential"}`)

// names extracts provider names from a ranked slice, in order.
func names(ps []Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// onDemand builds an on_demand provider (health-exempt) so constraint/score tests are not
// perturbed by the health gate.
func onDemand(name string, cost, quality float64) Provider {
	return Provider{Name: name, Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Cost: cost, Quality: quality, Enabled: true}
}

// TestRankCostQualityWeight (#6 weight): the same two candidates rank differently as the
// policy slides from cost-heavy to quality-heavy. cheap is low-cost/low-quality; premium is
// high-cost/high-quality.
func TestRankCostQualityWeight(t *testing.T) {
	cheap := onDemand("cheap", 1, 0.50)
	premium := onDemand("premium", 10, 0.95)
	cands := []Provider{premium, cheap} // unsorted input

	costHeavy := RoutingPolicy{CostWeight: 1, QualityWeight: 0}
	if got := names(rankProviders(cands, costHeavy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "cheap" {
		t.Errorf("cost-heavy policy should prefer the cheapest, got order %v", got)
	}

	qualityHeavy := RoutingPolicy{CostWeight: 0, QualityWeight: 1}
	if got := names(rankProviders(cands, qualityHeavy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "premium" {
		t.Errorf("quality-heavy policy should prefer the best quality, got order %v", got)
	}
}

// TestScoreNeutralWhenCostsEqual: with all costs equal the cost term is neutral, so a
// cost-heavy policy still lets quality break the tie (no division-by-zero on the span).
func TestScoreNeutralWhenCostsEqual(t *testing.T) {
	a := onDemand("a", 5, 0.40)
	b := onDemand("b", 5, 0.80)
	costHeavy := RoutingPolicy{CostWeight: 1, QualityWeight: 0.001}
	if got := names(rankProviders([]Provider{a, b}, costHeavy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "b" {
		t.Errorf("equal costs: quality should decide, got %v", got)
	}
}

// TestRankConstraintFilter (#6 constraint): transcrever requires a residential IP. Both
// candidates declare it, but only the one on runtime=local can satisfy it — the cloudrun
// one is eliminated. (Both on_demand so the health gate is out of the picture.)
func TestRankConstraintFilter(t *testing.T) {
	local := Provider{Name: "asr-local", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationOnDemand, Constraints: residential, Quality: 0.9, Enabled: true}
	datacenter := Provider{Name: "asr-dc", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: residential, Quality: 0.9, Enabled: true}

	ranked := rankProviders([]Provider{datacenter, local}, RoutingPolicy{}, routerItem, routerClock, routerHealthTTL, nil)
	if got := names(ranked); len(got) != 1 || got[0] != "asr-local" {
		t.Errorf("residential constraint should leave only the local provider, got %v", got)
	}
}

// TestRankConstraintUnknownFailsClosed: an unrecognized hard requirement makes the provider
// ineligible — the router never routes to a provider it cannot prove fits.
func TestRankConstraintUnknownFailsClosed(t *testing.T) {
	weird := Provider{Name: "needs-gpu", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"requires":"gpu"}`), Enabled: true}
	if ranked := rankProviders([]Provider{weird}, RoutingPolicy{}, routerItem, routerClock, routerHealthTTL, nil); len(ranked) != 0 {
		t.Errorf("unknown hard requirement should fail closed, got %v", names(ranked))
	}
}

// TestRankHealthExclusion (#6 heartbeat): a resident whose heartbeat went STALE is excluded
// (the real "offline" signal), while a fresh one stays eligible. A never-seen resident gets
// bootstrap grace (starting-up, not dead), and on_demand providers are asleep-by-design and
// eligible with no heartbeat at all.
func TestRankHealthExclusion(t *testing.T) {
	fresh := Provider{Name: "resident-fresh", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, HeartbeatAt: ptime(routerClock.Add(-1 * time.Minute)), Enabled: true}
	stale := Provider{Name: "resident-stale", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, HeartbeatAt: ptime(routerClock.Add(-30 * time.Minute)), Enabled: true}
	never := Provider{Name: "resident-never", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, HeartbeatAt: nil, Enabled: true}
	asleep := Provider{Name: "ondemand-asleep", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, HeartbeatAt: nil, Enabled: true}

	ranked := rankProviders([]Provider{fresh, stale, never, asleep}, RoutingPolicy{}, routerItem, routerClock, routerHealthTTL, nil)
	got := map[string]bool{}
	for _, n := range names(ranked) {
		got[n] = true
	}
	if !got["resident-fresh"] {
		t.Error("resident with a fresh heartbeat should be eligible")
	}
	if got["resident-stale"] {
		t.Error("resident with a STALE heartbeat should be excluded (the offline signal)")
	}
	if !got["resident-never"] {
		t.Error("never-seen resident should get bootstrap grace (starting-up, not dead)")
	}
	if !got["ondemand-asleep"] {
		t.Error("on_demand provider should be health-exempt (asleep by design)")
	}
}

// TestRankOrderedFallback (#6 fallback): the policy's ordered fallback list pins the front
// of the chain, overriding the cost<->quality score. Here the cheaper/better-scoring "a"
// would win on score, but the fallback ["b","a"] puts b first.
func TestRankOrderedFallback(t *testing.T) {
	a := onDemand("a", 1, 0.9) // would win on score
	b := onDemand("b", 10, 0.5)
	policy := RoutingPolicy{CostWeight: 1, QualityWeight: 1, Fallback: json.RawMessage(`["b","a"]`)}

	if got := names(rankProviders([]Provider{a, b}, policy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "b" || got[1] != "a" {
		t.Errorf("fallback order should pin b before a, got %v", got)
	}
	// A non-listed provider falls in behind the pinned ones, by score.
	c := onDemand("c", 1, 0.99)
	if got := names(rankProviders([]Provider{a, b, c}, policy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Errorf("non-listed provider should follow the pinned chain, got %v", got)
	}
}

// TestRankExcludeForFallover (#2/#6): excluding the dead provider drops it from the chain so
// the next one becomes the head (the timeout->fallback path).
func TestRankExcludeForFallover(t *testing.T) {
	a := onDemand("a", 1, 0.9)
	b := onDemand("b", 2, 0.8)
	ranked := rankProviders([]Provider{a, b}, RoutingPolicy{QualityWeight: 1}, routerItem, routerClock, routerHealthTTL, map[string]bool{"a": true})
	if got := names(ranked); len(got) != 1 || got[0] != "b" {
		t.Errorf("excluding a should leave only b, got %v", got)
	}
}

// TestNeverRunResidentFalloverToCloud: the bootstrap-grace + timeout-fallback round trip.
// A never-run resident (heartbeat_at IS NULL) is eligible via bootstrap grace and comes
// first (VPC-first). When its host is actually down the caller excludes it and the cloud
// fallback is returned — confirming the ovo-galinha fix does not stall the pipeline.
func TestNeverRunResidentFalloverToCloud(t *testing.T) {
	vpc := Provider{Name: "distill-local", Capability: "destilar", Runtime: runtimeLocal,
		Activation: activationResident, HeartbeatAt: nil, Enabled: true, Quality: 0.9, Cost: 0}
	cloud := Provider{Name: "distill-cloud", Capability: "destilar", Runtime: runtimeCloudRun,
		Activation: activationOnDemand, HeartbeatAt: nil, Enabled: true, Quality: 0.8, Cost: 1}
	policy := RoutingPolicy{QualityWeight: 1, Fallback: json.RawMessage(`["distill-local","distill-cloud"]`)}

	// Round 1: never-run resident is eligible (bootstrap grace).
	r1 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, nil))
	if len(r1) == 0 || r1[0] != "distill-local" {
		t.Fatalf("round 1: want distill-local first (bootstrap grace), got %v", r1)
	}

	// Round 2: host was down — exclude the never-run resident; cloud must be returned.
	r2 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, map[string]bool{"distill-local": true}))
	if len(r2) != 1 || r2[0] != "distill-cloud" {
		t.Fatalf("round 2: want only distill-cloud after excluding dead resident, got %v", r2)
	}
}

// TestRankNoEligible: an empty candidate set (or one fully filtered out) ranks to nothing,
// so Select returns ok=false and the item waits.
func TestRankNoEligible(t *testing.T) {
	if ranked := rankProviders(nil, RoutingPolicy{}, routerItem, routerClock, routerHealthTTL, nil); ranked != nil {
		t.Errorf("no candidates should rank to nil, got %v", names(ranked))
	}
}

// TestRankAcceptsMatchesLane: `accepts` routes an item to the provider that can consume its
// SOURCE. transcrever carries two providers — asr-youtube accepts ["youtube"], asr-direct-audio
// accepts ["podcast"] — and a provider with no `accepts` serves any lane. The item's lane
// selects the eligible set.
func TestRankAcceptsMatchesLane(t *testing.T) {
	yt := Provider{Name: "asr-youtube", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"accepts":["youtube"]}`), Enabled: true}
	pod := Provider{Name: "asr-direct-audio", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"accepts":["podcast"]}`), Enabled: true}
	anyLane := onDemand("any-lane", 1, 0.5) // no accepts -> serves every lane

	in := func(ps []Provider) map[string]bool {
		m := map[string]bool{}
		for _, n := range names(ps) {
			m[n] = true
		}
		return m
	}
	cands := []Provider{yt, pod, anyLane}

	ytItem := Item{Lane: laneYouTube}
	got := in(rankProviders(cands, RoutingPolicy{}, ytItem, routerClock, routerHealthTTL, nil))
	if !got["asr-youtube"] || got["asr-direct-audio"] || !got["any-lane"] {
		t.Errorf("youtube item: want asr-youtube + any-lane, not asr-direct-audio; got %v", got)
	}

	podItem := Item{Lane: lanePodcast}
	got = in(rankProviders(cands, RoutingPolicy{}, podItem, routerClock, routerHealthTTL, nil))
	if got["asr-youtube"] || !got["asr-direct-audio"] || !got["any-lane"] {
		t.Errorf("podcast item: want asr-direct-audio + any-lane, not asr-youtube; got %v", got)
	}
}

// TestRankSensitivityExcludesThirdParty: a provider tagged third_party is eliminated for a
// `private` item (only local/self-host may process private content), but is eligible for a
// `public` item. An untagged provider serves both.
func TestRankSensitivityExcludesThirdParty(t *testing.T) {
	third := Provider{Name: "cloud-llm", Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"sensitivity":"third_party"}`), Enabled: true}
	selfHost := Provider{Name: "local-llm", Capability: capDestilar, Runtime: runtimeVPC,
		Activation: activationOnDemand, Enabled: true}
	cands := []Provider{third, selfHost}

	private := Item{Lane: laneEmail, Sensitivity: sensitivityPrivate}
	if got := names(rankProviders(cands, RoutingPolicy{}, private, routerClock, routerHealthTTL, nil)); len(got) != 1 || got[0] != "local-llm" {
		t.Errorf("private item must route only to self-host, got %v", got)
	}

	public := Item{Lane: laneYouTube, Sensitivity: sensitivityPublic}
	if got := rankProviders(cands, RoutingPolicy{}, public, routerClock, routerHealthTTL, nil); len(got) != 2 {
		t.Errorf("public item should reach both providers, got %v", names(got))
	}
}

// TestRouterSelectReadsPolicyAndProviders exercises the full Select path over the mock:
// it reads the providers and the policy from the store and returns the best candidate.
func TestRouterSelectReadsPolicyAndProviders(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("cheap", 1, 0.5))
	mustProvider(t, db, onDemand("premium", 10, 0.95))

	rt := NewRouter(db)

	// Cost-heavy global policy -> cheapest wins.
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 1, QualityWeight: 0})
	p, ok, err := rt.Select(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL)
	if err != nil || !ok {
		t.Fatalf("select: ok=%v err=%v", ok, err)
	}
	if p.Name != "cheap" {
		t.Errorf("global cost-heavy policy: selected %q, want cheap", p.Name)
	}

	// A capability-scoped policy overrides the global one -> quality wins.
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: capTranscrever, CostWeight: 0, QualityWeight: 1})
	p, ok, err = rt.Select(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL)
	if err != nil || !ok {
		t.Fatalf("select scoped: ok=%v err=%v", ok, err)
	}
	if p.Name != "premium" {
		t.Errorf("capability-scoped quality policy should override global, selected %q, want premium", p.Name)
	}
}

// TestOnDemandVPCStaleHeartbeatFallover: the VPC-local scenario after activation=on_demand.
// An on_demand VPC provider with an 8-hour-old heartbeat must still be selected first (health-
// exempt by design); when its host is down the caller excludes it and the cloud fallback takes
// over — no stall.
func TestOnDemandVPCStaleHeartbeatFallover(t *testing.T) {
	staleHB := ptime(routerClock.Add(-8 * time.Hour))
	vpc := Provider{Name: provDistillLocal, Capability: capDestilar, Runtime: runtimeVPC,
		Activation: activationOnDemand, HeartbeatAt: staleHB, Enabled: true, Quality: 0.92, Cost: 1.50}
	cloud := Provider{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, HeartbeatAt: nil, Enabled: true, Quality: 0.92, Cost: 2.00}
	policy := RoutingPolicy{CostWeight: 0.5, QualityWeight: 0.5, Fallback: json.RawMessage(`["distill-local","distill"]`)}

	// Round 1: VPC on_demand with stale heartbeat must be eligible (health-exempt).
	r1 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, nil))
	if len(r1) == 0 || r1[0] != provDistillLocal {
		t.Fatalf("round 1: want distill-local first (on_demand health-exempt despite stale heartbeat), got %v", r1)
	}

	// Round 2: agent down — exclude distill-local; cloud fallback must be returned, no stall.
	r2 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, map[string]bool{provDistillLocal: true}))
	if len(r2) != 1 || r2[0] != provDistill {
		t.Fatalf("round 2: want distill after excluding dead vpc, got %v", r2)
	}
}

// TestRouterSelectNoneEligible: when every candidate is filtered out (here: the only
// transcrever provider is an offline resident), Select reports ok=false.
func TestRouterSelectNoneEligible(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	// Resident provider with a stale heartbeat (offline) and no other candidate.
	mustProvider(t, db, Provider{Name: "asr", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationResident, HeartbeatAt: ptime(routerClock.Add(-1 * time.Hour)), Enabled: true})

	_, ok, err := NewRouter(db).Select(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if ok {
		t.Error("an offline resident with no alternative should yield ok=false (item waits)")
	}
}

// TestRouterSelectForStepOverridesFallback: when a step carries a per-step providers list
// in its options, SelectForStep uses that as the fallback order instead of the policy's.
// Here the global policy prefers "cheap" (cost-heavy), but the step says ["premium", "cheap"],
// so premium should be selected first.
func TestRouterSelectForStepOverridesFallback(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("cheap", 1, 0.5))
	mustProvider(t, db, onDemand("premium", 10, 0.95))
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 1, QualityWeight: 0})

	rt := NewRouter(db)

	// Without step override: cheap wins (global policy is cost-heavy).
	p, ok, err := rt.Select(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL)
	if err != nil || !ok || p.Name != "cheap" {
		t.Fatalf("baseline: want cheap, got %q (ok=%v, err=%v)", p.Name, ok, err)
	}

	// With step override ["premium","cheap"]: premium is pinned first.
	stepFb := json.RawMessage(`["premium","cheap"]`)
	p, ok, err = rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, stepFb)
	if err != nil || !ok {
		t.Fatalf("SelectForStep: ok=%v err=%v", ok, err)
	}
	if p.Name != "premium" {
		t.Errorf("step override should pin premium first, got %q", p.Name)
	}
}

// TestRouterSelectForStepNilFallback: nil stepFallback behaves identically to Select.
func TestRouterSelectForStepNilFallback(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("only", 1, 1))

	rt := NewRouter(db)
	p, ok, err := rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, nil)
	if err != nil || !ok || p.Name != "only" {
		t.Errorf("nil stepFallback: ok=%v err=%v name=%q, want only", ok, err, p.Name)
	}
}

// TestRouterSelectForStepInvalidFallbackJSON: malformed stepFallback returns an error rather
// than silently assigning garbage to policy.Fallback.
func TestRouterSelectForStepInvalidFallbackJSON(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("only", 1, 1))

	rt := NewRouter(db)
	_, _, err := rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, json.RawMessage(`not-valid-json`))
	if err == nil {
		t.Error("want error for invalid stepFallback JSON, got nil")
	}
}

// TestRouterSelectForStepEmptyArrayFallback: an empty JSON array must not override the global
// policy's fallback — the len(check) > 0 guard in router.go prevents it.
func TestRouterSelectForStepEmptyArrayFallback(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("cheap", 1, 0.5))
	mustProvider(t, db, onDemand("premium", 10, 0.95))
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 1, QualityWeight: 0})

	rt := NewRouter(db)
	// Empty array should fall through to the global policy (cost-heavy keeps cheap first).
	p, ok, err := rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, json.RawMessage(`[]`))
	if err != nil || !ok {
		t.Fatalf("empty stepFallback: ok=%v err=%v", ok, err)
	}
	if p.Name != "cheap" {
		t.Errorf("empty stepFallback should not override policy, got %q want cheap", p.Name)
	}
}

// mustProvider upserts a provider into the mock or fails the test.
func mustProvider(t *testing.T, db *MockDatabase, p Provider) {
	t.Helper()
	if err := db.UpsertProvider(context.Background(), p); err != nil {
		t.Fatalf("upsert provider %s: %v", p.Name, err)
	}
}
