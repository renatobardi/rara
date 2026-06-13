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
	if got := names(rankProviders(cands, costHeavy, routerClock, routerHealthTTL, nil)); got[0] != "cheap" {
		t.Errorf("cost-heavy policy should prefer the cheapest, got order %v", got)
	}

	qualityHeavy := RoutingPolicy{CostWeight: 0, QualityWeight: 1}
	if got := names(rankProviders(cands, qualityHeavy, routerClock, routerHealthTTL, nil)); got[0] != "premium" {
		t.Errorf("quality-heavy policy should prefer the best quality, got order %v", got)
	}
}

// TestScoreNeutralWhenCostsEqual: with all costs equal the cost term is neutral, so a
// cost-heavy policy still lets quality break the tie (no division-by-zero on the span).
func TestScoreNeutralWhenCostsEqual(t *testing.T) {
	a := onDemand("a", 5, 0.40)
	b := onDemand("b", 5, 0.80)
	costHeavy := RoutingPolicy{CostWeight: 1, QualityWeight: 0.001}
	if got := names(rankProviders([]Provider{a, b}, costHeavy, routerClock, routerHealthTTL, nil)); got[0] != "b" {
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

	ranked := rankProviders([]Provider{datacenter, local}, RoutingPolicy{}, routerClock, routerHealthTTL, nil)
	if got := names(ranked); len(got) != 1 || got[0] != "asr-local" {
		t.Errorf("residential constraint should leave only the local provider, got %v", got)
	}
}

// TestRankConstraintUnknownFailsClosed: an unrecognized hard requirement makes the provider
// ineligible — the router never routes to a provider it cannot prove fits.
func TestRankConstraintUnknownFailsClosed(t *testing.T) {
	weird := Provider{Name: "needs-gpu", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"requires":"gpu"}`), Enabled: true}
	if ranked := rankProviders([]Provider{weird}, RoutingPolicy{}, routerClock, routerHealthTTL, nil); len(ranked) != 0 {
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

	ranked := rankProviders([]Provider{fresh, stale, never, asleep}, RoutingPolicy{}, routerClock, routerHealthTTL, nil)
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

	if got := names(rankProviders([]Provider{a, b}, policy, routerClock, routerHealthTTL, nil)); got[0] != "b" || got[1] != "a" {
		t.Errorf("fallback order should pin b before a, got %v", got)
	}
	// A non-listed provider falls in behind the pinned ones, by score.
	c := onDemand("c", 1, 0.99)
	if got := names(rankProviders([]Provider{a, b, c}, policy, routerClock, routerHealthTTL, nil)); got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Errorf("non-listed provider should follow the pinned chain, got %v", got)
	}
}

// TestRankExcludeForFallover (#2/#6): excluding the dead provider drops it from the chain so
// the next one becomes the head (the timeout->fallback path).
func TestRankExcludeForFallover(t *testing.T) {
	a := onDemand("a", 1, 0.9)
	b := onDemand("b", 2, 0.8)
	ranked := rankProviders([]Provider{a, b}, RoutingPolicy{QualityWeight: 1}, routerClock, routerHealthTTL, map[string]bool{"a": true})
	if got := names(ranked); len(got) != 1 || got[0] != "b" {
		t.Errorf("excluding a should leave only b, got %v", got)
	}
}

// TestRankNoEligible: an empty candidate set (or one fully filtered out) ranks to nothing,
// so Select returns ok=false and the item waits.
func TestRankNoEligible(t *testing.T) {
	if ranked := rankProviders(nil, RoutingPolicy{}, routerClock, routerHealthTTL, nil); ranked != nil {
		t.Errorf("no candidates should rank to nil, got %v", names(ranked))
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
	p, ok, err := rt.Select(ctx, capTranscrever, routerClock, routerHealthTTL)
	if err != nil || !ok {
		t.Fatalf("select: ok=%v err=%v", ok, err)
	}
	if p.Name != "cheap" {
		t.Errorf("global cost-heavy policy: selected %q, want cheap", p.Name)
	}

	// A capability-scoped policy overrides the global one -> quality wins.
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: capTranscrever, CostWeight: 0, QualityWeight: 1})
	p, ok, err = rt.Select(ctx, capTranscrever, routerClock, routerHealthTTL)
	if err != nil || !ok {
		t.Fatalf("select scoped: ok=%v err=%v", ok, err)
	}
	if p.Name != "premium" {
		t.Errorf("capability-scoped quality policy should override global, selected %q, want premium", p.Name)
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

	_, ok, err := NewRouter(db).Select(ctx, capTranscrever, routerClock, routerHealthTTL)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if ok {
		t.Error("an offline resident with no alternative should yield ok=false (item waits)")
	}
}

// mustProvider upserts a provider into the mock or fails the test.
func mustProvider(t *testing.T, db *MockDatabase, p Provider) {
	t.Helper()
	if err := db.UpsertProvider(context.Background(), p); err != nil {
		t.Fatalf("upsert provider %s: %v", p.Name, err)
	}
}
