package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Router tests exercise rankProviders as a pure function and Router.Select over MockDatabase —
// zero I/O. They cover the four selection behaviours: hard-constraint filtering, health
// (heartbeat) exclusion, ordered fallback, and name-based deterministic tiebreaking.

// routerClock is a fixed "now" for the health gate; fresh/stale heartbeats are derived from
// it so the tests never touch the wall clock.
var routerClock = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

const routerHealthTTL = 5 * time.Minute

// routerItem is a neutral item for the existing selection tests: a public YouTube item. The
// providers those tests build declare no `accepts`/`sensitivity`, so the item's facets do not
// perturb them; the accepts/sensitivity behaviour is covered by its own tests below.
var routerItem = Item{Lane: laneYouTube, Sensitivity: sensitivityPublic}

func ptime(t time.Time) *time.Time { return &t }

// residential is the JSON constraint caption-mac carries.
var residential = json.RawMessage(`{"requires":"residential"}`)

// names extracts provider names from a ranked slice, in order.
func names(ps []Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// onDemand builds an on_demand provider (health-exempt) so constraint tests are not perturbed
// by the health gate.
func onDemand(name string) Provider {
	return Provider{Name: name, Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: true}
}

// TestRankNameTiebreak: without a fallback list, non-pinned providers order alphabetically so
// the selection is always deterministic.
func TestRankNameTiebreak(t *testing.T) {
	b := onDemand("b")
	a := onDemand("a")
	if got := names(rankProviders([]Provider{b, a}, RoutingPolicy{}, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "a" {
		t.Errorf("alphabetical tiebreak: want a first, got %v", got)
	}
}

// TestRankConstraintFilter: transcrever requires a residential IP. Both candidates declare it,
// but only the one on runtime=local can satisfy it — the cloudrun one is eliminated.
// (Both on_demand so the health gate is out of the picture.)
func TestRankConstraintFilter(t *testing.T) {
	local := Provider{Name: "asr-local", Capability: capTranscrever, Runtime: runtimeLocal,
		Activation: activationOnDemand, Constraints: residential, Enabled: true}
	datacenter := Provider{Name: "asr-dc", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: residential, Enabled: true}

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

// TestRankOrderedFallback: the policy's ordered fallback list pins the front of the chain.
// "a" would sort first alphabetically, but fallback ["b","a"] puts b first.
func TestRankOrderedFallback(t *testing.T) {
	a := onDemand("a")
	b := onDemand("b")
	policy := RoutingPolicy{Fallback: json.RawMessage(`["b","a"]`)}

	if got := names(rankProviders([]Provider{a, b}, policy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "b" || got[1] != "a" {
		t.Errorf("fallback order should pin b before a, got %v", got)
	}
	// A non-listed provider falls in behind the pinned ones, by name.
	c := onDemand("c")
	if got := names(rankProviders([]Provider{a, b, c}, policy, routerItem, routerClock, routerHealthTTL, nil)); got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Errorf("non-listed provider should follow the pinned chain, got %v", got)
	}
}

// TestRankExcludeForFallover: excluding the dead provider drops it from the chain so
// the next one becomes the head (the timeout->fallback path).
func TestRankExcludeForFallover(t *testing.T) {
	a := onDemand("a")
	b := onDemand("b")
	ranked := rankProviders([]Provider{a, b}, RoutingPolicy{}, routerItem, routerClock, routerHealthTTL, map[string]bool{"a": true})
	if got := names(ranked); len(got) != 1 || got[0] != "b" {
		t.Errorf("excluding a should leave only b, got %v", got)
	}
}

// TestNeverRunResidentFalloverToCloud: the bootstrap-grace + timeout-fallback round trip.
// A never-run resident (heartbeat_at IS NULL) is eligible via bootstrap grace and comes
// first (VPC-first). When its host is actually down the caller excludes it and the cloud
// fallback is returned — confirming the ovo-galinha fix does not stall the pipeline.
func TestNeverRunResidentFalloverToCloud(t *testing.T) {
	vpc := Provider{Name: "distill-vpc", Capability: "destilar", Runtime: runtimeLocal,
		Activation: activationResident, HeartbeatAt: nil, Enabled: true}
	cloud := Provider{Name: "distill-cloud", Capability: "destilar", Runtime: runtimeCloudRun,
		Activation: activationOnDemand, HeartbeatAt: nil, Enabled: true}
	policy := RoutingPolicy{Fallback: json.RawMessage(`["distill-vpc","distill-cloud"]`)}

	// Round 1: never-run resident is eligible (bootstrap grace).
	r1 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, nil))
	if len(r1) == 0 || r1[0] != "distill-vpc" {
		t.Fatalf("round 1: want distill-vpc first (bootstrap grace), got %v", r1)
	}

	// Round 2: host was down — exclude the never-run resident; cloud must be returned.
	r2 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, map[string]bool{"distill-vpc": true}))
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
// SOURCE. transcrever carries two providers — caption-mac accepts ["youtube"], echo-cloud
// accepts ["podcast"] — and a provider with no `accepts` serves any lane. The item's lane
// selects the eligible set.
func TestRankAcceptsMatchesLane(t *testing.T) {
	yt := Provider{Name: "caption-mac", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"accepts":["youtube"]}`), Enabled: true}
	pod := Provider{Name: "echo-cloud", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Constraints: json.RawMessage(`{"accepts":["podcast"]}`), Enabled: true}
	anyLane := onDemand("any-lane") // no accepts -> serves every lane

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
	if !got["caption-mac"] || got["echo-cloud"] || !got["any-lane"] {
		t.Errorf("youtube item: want caption-mac + any-lane, not echo-cloud; got %v", got)
	}

	podItem := Item{Lane: lanePodcast}
	got = in(rankProviders(cands, RoutingPolicy{}, podItem, routerClock, routerHealthTTL, nil))
	if got["caption-mac"] || !got["echo-cloud"] || !got["any-lane"] {
		t.Errorf("podcast item: want echo-cloud + any-lane, not caption-mac; got %v", got)
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
	mustProvider(t, db, onDemand("cheap"))
	mustProvider(t, db, onDemand("premium"))

	rt := NewRouter(db)

	// No fallback policy: alphabetical order -> cheap wins (cheap < premium).
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := rt.Select(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL)
	if err != nil || !ok {
		t.Fatalf("select: ok=%v err=%v", ok, err)
	}
	if p.Name != "cheap" {
		t.Errorf("no fallback: want cheap (alphabetical), got %q", p.Name)
	}

	// A capability-scoped policy with fallback pins premium first.
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: capTranscrever, Fallback: json.RawMessage(`["premium","cheap"]`)}); err != nil {
		t.Fatal(err)
	}
	p, ok, err = rt.Select(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL)
	if err != nil || !ok {
		t.Fatalf("select scoped: ok=%v err=%v", ok, err)
	}
	if p.Name != "premium" {
		t.Errorf("capability-scoped fallback should pin premium first, got %q", p.Name)
	}
}

// TestOnDemandVPCStaleHeartbeatFallover: the VPC-local scenario after activation=on_demand.
// An on_demand VPC provider with an 8-hour-old heartbeat must still be selected first (health-
// exempt by design); when its host is down the caller excludes it and the cloud fallback takes
// over — no stall.
func TestOnDemandVPCStaleHeartbeatFallover(t *testing.T) {
	staleHB := ptime(routerClock.Add(-8 * time.Hour))
	vpc := Provider{Name: provDistillLocal, Capability: capDestilar, Runtime: runtimeVPC,
		Activation: activationOnDemand, HeartbeatAt: staleHB, Enabled: true}
	cloud := Provider{Name: provDistill, Capability: capDestilar, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, HeartbeatAt: nil, Enabled: true}
	policy := RoutingPolicy{Fallback: json.RawMessage(`["` + provDistillLocal + `","` + provDistill + `"]`)}

	// Round 1: VPC on_demand with stale heartbeat must be eligible (health-exempt).
	r1 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, nil))
	if len(r1) == 0 || r1[0] != provDistillLocal {
		t.Fatalf("round 1: want %s first (on_demand health-exempt despite stale heartbeat), got %v", provDistillLocal, r1)
	}

	// Round 2: agent down — exclude VPC; cloud fallback must be returned, no stall.
	r2 := names(rankProviders([]Provider{vpc, cloud}, policy, routerItem, routerClock, routerHealthTTL, map[string]bool{provDistillLocal: true}))
	if len(r2) != 1 || r2[0] != provDistill {
		t.Fatalf("round 2: want %s after excluding dead vpc, got %v", provDistill, r2)
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
	mustProvider(t, db, onDemand("cheap"))
	mustProvider(t, db, onDemand("premium"))
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: policyScopeGlobal}); err != nil {
		t.Fatal(err)
	}

	rt := NewRouter(db)

	// Without step override: alphabetical order -> cheap wins (cheap < premium).
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
	mustProvider(t, db, onDemand("only"))

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
	mustProvider(t, db, onDemand("only"))

	rt := NewRouter(db)
	_, _, err := rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, json.RawMessage(`not-valid-json`))
	if err == nil {
		t.Error("want error for invalid stepFallback JSON, got nil")
	}
}

// TestRouterSelectForStepEmptyArrayFallback: an empty JSON array must not override the global
// policy's fallback — the len(check) > 0 guard in router.go prevents it. Global policy prefers
// "premium"; an empty stepFallback must preserve that, so premium wins (not alphabetical "cheap").
func TestRouterSelectForStepEmptyArrayFallback(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("cheap"))
	mustProvider(t, db, onDemand("premium"))
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{
		Scope:    policyScopeGlobal,
		Fallback: json.RawMessage(`["premium","cheap"]`),
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewRouter(db)
	// Empty array should fall through to the non-empty global policy (premium first).
	p, ok, err := rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, json.RawMessage(`[]`))
	if err != nil || !ok {
		t.Fatalf("empty stepFallback: ok=%v err=%v", ok, err)
	}
	if p.Name != "premium" {
		t.Errorf("empty stepFallback should preserve global policy, got %q want premium", p.Name)
	}
}

// TestRankProvidersExcludesDisabled: a disabled provider must be filtered out even
// when passed directly to rankProviders (defense-in-depth; the DB layer also filters).
func TestRankProvidersExcludesDisabled(t *testing.T) {
	disabled := Provider{Name: "off", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: false}
	enabled := Provider{Name: "on", Capability: capTranscrever, Runtime: runtimeCloudRun,
		Activation: activationOnDemand, Enabled: true}
	policy := RoutingPolicy{}
	got := names(rankProviders([]Provider{disabled, enabled}, policy, routerItem, routerClock, routerHealthTTL, nil))
	if len(got) != 1 || got[0] != "on" {
		t.Errorf("want [on], got %v (disabled provider must be excluded)", got)
	}
}

// TestConstraintsSatisfiedRejectsUnknownKeys: an unrecognized key in constraints must cause
// fail-closed (provider excluded), not silently ignored.
func TestConstraintsSatisfiedRejectsUnknownKeys(t *testing.T) {
	p := Provider{Name: "p", Constraints: json.RawMessage(`{"unknown_security_field":"value"}`)}
	if constraintsSatisfied(p, routerItem) {
		t.Error("unknown constraint key should fail closed (return false), but returned true")
	}
}

// TestConstraintsSatisfiedRejectsUnknownSensitivity: an unrecognized sensitivity value must
// fail closed — same pattern as the requires field's switch statement.
func TestConstraintsSatisfiedRejectsUnknownSensitivity(t *testing.T) {
	p := Provider{Name: "p", Constraints: json.RawMessage(`{"sensitivity":"typo_thirdparty"}`)}
	if constraintsSatisfied(p, routerItem) {
		t.Error("unknown sensitivity value should fail closed (return false), but returned true")
	}
}

// TestRouterSelectForStepTypoFallbackPreservesGlobalPolicy: a stepFallback with all-typo
// provider names must NOT override the global policy — the global policy ("premium" first)
// must be preserved, so premium wins rather than falling to alphabetical (cheap).
func TestRouterSelectForStepTypoFallbackPreservesGlobalPolicy(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	mustProvider(t, db, onDemand("cheap"))
	mustProvider(t, db, onDemand("premium"))
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{
		Scope:    policyScopeGlobal,
		Fallback: json.RawMessage(`["premium","cheap"]`),
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewRouter(db)
	// stepFallback names all typos — no known provider; must preserve the global policy.
	p, ok, err := rt.SelectForStep(ctx, capTranscrever, routerItem, routerClock, routerHealthTTL, json.RawMessage(`["typo-a","typo-b"]`))
	if err != nil || !ok {
		t.Fatalf("SelectForStep: ok=%v err=%v", ok, err)
	}
	if p.Name != "premium" {
		t.Errorf("typo stepFallback must preserve global policy (want premium), got %q", p.Name)
	}
}

// mustProvider upserts a provider into the mock or fails the test.
func mustProvider(t *testing.T, db *MockDatabase, p Provider) {
	t.Helper()
	if err := db.UpsertProvider(context.Background(), p); err != nil {
		t.Fatalf("upsert provider %s: %v", p.Name, err)
	}
}
