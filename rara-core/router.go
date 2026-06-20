// router.go — Phase 2 deliverable #1: the policy-driven provider router.
//
// Phase 1 selected "the first enabled provider" of a capability. Phase 2 replaces that
// stub with a real router — the same cost/quality/fallback/constraints pattern LiteLLM
// applies to models, applied one level down to WHERE work runs. Given the providers of a
// capability and a routing policy, the router:
//
//  1. eliminates providers that fail a HARD constraint (e.g. asr-youtube requires a
//     residential IP -> any cloudrun/vpc candidate is dropped);
//  2. eliminates UNHEALTHY providers (a resident worker whose heartbeat went STALE is
//     offline; a never-seen resident gets bootstrap grace; on_demand workers are
//     asleep-by-design and exempt);
//  3. orders the survivors by a cost<->quality score, with the policy's ordered `fallback`
//     list pinning the front of the chain (an explicit operator failover order);
//  4. returns the best candidate — or nothing, in which case the item waits (the caller
//     surfaces a clear "no eligible provider" error rather than routing blind).
//
// The router is the single, centralized, auditable selection point. It reads config
// (providers + policy) and makes a pure decision; it never assigns or activates — that is
// the reconciler's job. Everything below is exercised by router_test.go with a MockDatabase
// (Select) and as a pure function (rankProviders), zero I/O.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"
)

// defaultHealthTTL is how recently a RESIDENT provider must have heartbeat to count as
// alive for selection. on_demand providers are exempt (scale-to-zero: asleep until the
// reconciler wakes them). Tunable via ROUTE_HEALTH_SECONDS on the reconciler.
const defaultHealthTTL = 5 * time.Minute

// Router selects a provider for a capability by policy. It holds only the store seam; the
// clock and health window are passed in per call so the reconciler owns time (and tests
// are deterministic).
type Router struct {
	db Database
}

// NewRouter wires a router over the persistence seam.
func NewRouter(db Database) *Router { return &Router{db: db} }

// Select returns the preferred provider for a capability serving a specific ITEM, or
// ok=false when none is eligible+healthy (the item waits). The item is needed because
// eligibility now depends on the item's facets: a provider's `accepts` must include the
// item's lane (source matching), and a third-party provider is excluded for `private`
// content (sensitivity). now/healthTTL drive the heartbeat health gate. `exclude` names
// providers to skip — the timeout->fallback path passes the dead provider so Select returns
// the NEXT one in the ordered candidate chain.
func (rt *Router) Select(ctx context.Context, capability string, item Item, now time.Time, healthTTL time.Duration, exclude ...string) (Provider, bool, error) {
	return rt.SelectForStep(ctx, capability, item, now, healthTTL, nil, exclude...)
}

// SelectForStep is like Select but if stepFallback is a non-empty JSON array it overrides
// the policy's Fallback for this specific step, giving per-step host-priority control
// without touching the global routing policy.
func (rt *Router) SelectForStep(ctx context.Context, capability string, item Item, now time.Time, healthTTL time.Duration, stepFallback json.RawMessage, exclude ...string) (Provider, bool, error) {
	providers, err := rt.db.ListProvidersForCapability(ctx, capability)
	if err != nil {
		return Provider{}, false, err
	}
	policy, err := rt.policyFor(ctx, capability)
	if err != nil {
		return Provider{}, false, err
	}
	if len(stepFallback) > 0 {
		// Unmarshal once here to validate shape + length; fallbackPositions will
		// unmarshal policy.Fallback again. Double-unmarshal is intentional: the
		// raw bytes are kept so downstream callers work on the canonical form.
		var check []string
		if err := json.Unmarshal(stepFallback, &check); err != nil {
			return Provider{}, false, fmt.Errorf("select for step: invalid stepFallback JSON: %w", err)
		}
		if len(check) > 0 {
			known := make(map[string]bool, len(providers))
			for _, p := range providers {
				known[p.Name] = true
			}
			anyKnown := false
			for _, name := range check {
				if known[name] {
					anyKnown = true
					break
				}
			}
			if !anyKnown {
				log.Printf("router: stepFallback %v contains no known %s provider — check for typos", check, capability)
			}
			policy.Fallback = stepFallback
		}
	}
	ex := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		ex[n] = true
	}
	ranked := rankProviders(providers, policy, item, now, healthTTL, ex)
	if len(ranked) == 0 {
		return Provider{}, false, nil
	}
	return ranked[0], true, nil
}

// policyForCapability returns the capability-scoped routing policy, falling back to the global
// scope, then to a neutral default (equal cost<->quality weight, no fallback) so the router
// always has a policy even before one is seeded. Both Router and Core use this function.
func policyForCapability(ctx context.Context, db Database, capability string) (RoutingPolicy, error) {
	if p, ok, err := db.GetRoutingPolicy(ctx, capability); err != nil {
		return RoutingPolicy{}, err
	} else if ok {
		return p, nil
	}
	if p, ok, err := db.GetRoutingPolicy(ctx, policyScopeGlobal); err != nil {
		return RoutingPolicy{}, err
	} else if ok {
		return p, nil
	}
	return RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 0.5, QualityWeight: 0.5}, nil
}

// policyFor returns the routing policy for a capability. Delegates to policyForCapability.
func (rt *Router) policyFor(ctx context.Context, capability string) (RoutingPolicy, error) {
	return policyForCapability(ctx, rt.db, capability)
}

// Candidate is the explain-level view of a single provider in a routing decision.
// explainProviders returns one per input provider; rankProviders derives its output
// from these — no rule is duplicated between the two.
type Candidate struct {
	Name        string  `json:"name"`
	Eligible    bool    `json:"eligible"`
	Healthy     bool    `json:"healthy"`
	Reason      string  `json:"reason"` // empty when eligible+healthy resident; informational for on_demand
	CostCredit  float64 `json:"cost_credit"`
	Quality     float64 `json:"quality"`
	Score       float64 `json:"score"`
	FallbackPos int     `json:"fallback_pos"` // 0 = not pinned; 1+ = position in fallback list
	Selected    bool    `json:"selected"`
}

// explainProviders evaluates all providers for a routing decision without discarding
// any: each Candidate carries eligibility, health, scoring, and fallback position, and
// the winner is marked Selected. rankProviders derives its output from these, so the
// selection rule lives here only.
//
// Scoring uses min-max normalization over the eligible+healthy set (the same range
// rankProviders would use), giving ineligible providers a comparable display score.
// The sort key is: fallback-pinned first (ascending FallbackPos), then Score descending,
// then Name for determinism — identical to rankProviders.
func explainProviders(providers []Provider, policy RoutingPolicy, item Item, now time.Time, healthTTL time.Duration, exclude map[string]bool) []Candidate {
	fbPos := fallbackPositions(policy.Fallback)

	// First pass: eligibility and health for every provider; collect the eligible+healthy
	// set for score normalization.
	type raw struct {
		eligible bool
		healthy  bool
		reason   string
	}
	raws := make([]raw, len(providers))
	eligibleSet := make([]Provider, 0, len(providers))

	for i, p := range providers {
		r := raw{healthy: true}
		if exclude[p.Name] {
			r.eligible = false
			r.reason = "excluded"
		} else {
			r.eligible, r.reason = constraintsExplain(p, item)
		}
		hOk, hReason := providerHealthyExplain(p, now, healthTTL)
		r.healthy = hOk
		if r.eligible {
			if !hOk {
				r.reason = hReason
			} else if hReason != "" {
				r.reason = hReason // informational (on_demand: health exempt)
			}
		}
		raws[i] = r
		if r.eligible && r.healthy {
			eligibleSet = append(eligibleSet, p)
		}
	}

	// Score normalization over the eligible+healthy set (min-max on cost, same as rankProviders used).
	var minCost, maxCost, span float64
	if len(eligibleSet) > 0 {
		minCost, maxCost = eligibleSet[0].Cost, eligibleSet[0].Cost
		for _, p := range eligibleSet {
			if p.Cost < minCost {
				minCost = p.Cost
			}
			if p.Cost > maxCost {
				maxCost = p.Cost
			}
		}
		span = maxCost - minCost
	}

	costCredit := func(cost float64) float64 {
		if span == 0 {
			return 1.0
		}
		cc := 1 - (cost-minCost)/span
		if cc < 0 {
			cc = 0
		} else if cc > 1 {
			cc = 1
		}
		return cc
	}

	// Build candidates and find the winner by sorting the eligible+healthy subset.
	cands := make([]Candidate, len(providers))
	eligCands := make([]Candidate, 0, len(eligibleSet))
	for i, p := range providers {
		cc := costCredit(p.Cost)
		fp := 0
		if pos, ok := fbPos[p.Name]; ok {
			fp = pos + 1 // 1-indexed
		}
		c := Candidate{
			Name:        p.Name,
			Eligible:    raws[i].eligible,
			Healthy:     raws[i].healthy,
			Reason:      raws[i].reason,
			CostCredit:  cc,
			Quality:     p.Quality,
			Score:       policy.QualityWeight*p.Quality + policy.CostWeight*cc,
			FallbackPos: fp,
		}
		cands[i] = c
		if c.Eligible && c.Healthy {
			eligCands = append(eligCands, c)
		}
	}

	if len(eligCands) > 0 {
		sort.SliceStable(eligCands, func(i, j int) bool {
			return candidateLess(eligCands[i], eligCands[j])
		})
		winner := eligCands[0].Name
		for i := range cands {
			if cands[i].Name == winner {
				cands[i].Selected = true
				break
			}
		}
	}

	return cands
}

// candidateLess is the sort key for eligible Candidates: fallback-pinned first (ascending
// FallbackPos), then Score descending, then Name for determinism.
func candidateLess(a, b Candidate) bool {
	aIn := a.FallbackPos > 0
	bIn := b.FallbackPos > 0
	if aIn && bIn {
		return a.FallbackPos < b.FallbackPos
	}
	if aIn != bIn {
		return aIn
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.Name < b.Name
}

// rankProviders returns the eligible, healthy providers for a routing decision in
// preference order (best first). It is a thin filter over explainProviders — all selection
// rules live there; this function only extracts and sorts the eligible+healthy subset.
func rankProviders(providers []Provider, policy RoutingPolicy, item Item, now time.Time, healthTTL time.Duration, exclude map[string]bool) []Provider {
	explained := explainProviders(providers, policy, item, now, healthTTL, exclude)

	provByName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		provByName[p.Name] = p
	}

	eligible := make([]Candidate, 0, len(providers))
	for _, c := range explained {
		if c.Eligible && c.Healthy {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return candidateLess(eligible[i], eligible[j])
	})

	result := make([]Provider, len(eligible))
	for i, c := range eligible {
		result[i] = provByName[c.Name]
	}
	return result
}

// providerConstraints is the parsed shape of providers.constraints. Three keys are
// understood: `requires` (a runtime requirement), `accepts` (the item lanes this provider can
// consume), and `sensitivity` (a third-party tag). Unknown keys are ignored, but an unknown
// requirement VALUE fails closed (constraintsExplain).
type providerConstraints struct {
	Requires    string   `json:"requires"`
	Accepts     []string `json:"accepts"`
	Sensitivity string   `json:"sensitivity"`
}

// constraintsExplain is the single source of truth for constraint evaluation. It returns
// (satisfied, reason) where reason is empty on success and describes the first failing gate.
//
// Three independent gates, all of which must pass:
//
//  1. requires (runtime): asr-youtube declares {"requires":"residential"} because YouTube
//     blocks audio download from datacenter IPs, so only runtime=local qualifies — a
//     cloudrun/vpc candidate declaring the same requirement is eliminated.
//  2. accepts (source matching): when a provider declares `accepts`, the item's lane must be
//     listed. This is what lets transcrever carry TWO providers — asr-youtube accepts
//     ["youtube"], asr-direct-audio accepts ["podcast"] — and routes each item to the one
//     that can actually consume its source. A provider with no `accepts` consumes any lane
//     (coletar/gates/destilar, and back-compat).
//  3. sensitivity: a provider tagged {"sensitivity":"third_party"} sends content off-box, so
//     it is excluded for a `private` item (only local/self-host may process private content).
//
// Fail-closed on the unknowns: malformed constraints or an unrecognized requirement value
// make the provider ineligible, so the router never assigns a provider it cannot PROVE fits
// (the item waits, with a clear error, rather than routing blind).
func constraintsExplain(p Provider, item Item) (bool, string) {
	if len(p.Constraints) == 0 {
		return true, "" // no constraints declared: serves any item
	}
	var c providerConstraints
	if err := json.Unmarshal(p.Constraints, &c); err != nil {
		return false, "constraint: malformed JSON"
	}
	// 1) runtime requirement
	switch c.Requires {
	case "":
		// no hard runtime requirement declared
	case constraintResidential:
		if p.Runtime != runtimeLocal {
			return false, "constraint: requires residential"
		}
	default:
		return false, "constraint: unknown requirement"
	}
	// 2) source matching: a declared accepts-list must include the item's lane
	if len(c.Accepts) > 0 && !containsString(c.Accepts, item.Lane) {
		return false, "constraint: lane not accepted"
	}
	// 3) sensitivity: third-party providers cannot process private content
	if item.Sensitivity == sensitivityPrivate && c.Sensitivity == constraintThirdParty {
		return false, "constraint: third_party excluded for private"
	}
	return true, ""
}

// containsString reports whether v is in xs.
func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// providerHealthyExplain is the single source of truth for provider health. It returns
// (healthy, reason) where reason is:
//   - "on_demand: health exempt" for on_demand providers (informational; they are always healthy)
//   - "heartbeat stale" for resident providers with an old heartbeat
//   - "" for a fresh or never-seen resident (bootstrap grace)
//
// providerHealthy wraps it for callers that only need the bool.
//
// on_demand providers (Cloud Run, scale-to-zero) are asleep by design until the reconciler
// wakes them, so they always pass the health gate at SELECTION time. A resident provider
// (the Mac scribe, a VPC worker) is meant to be awake and heartbeating. A STALE heartbeat
// (older than healthTTL) excludes it; a provider we have NEVER heard from gets bootstrap
// grace — treated as starting-up, not dead — to avoid the ovo-galinha deadlock (a resident
// only stamps its first heartbeat when it CLAIMS work, which requires being selected first).
func providerHealthyExplain(p Provider, now time.Time, healthTTL time.Duration) (bool, string) {
	if p.Activation == activationOnDemand {
		return true, "on_demand: health exempt"
	}
	if p.HeartbeatAt == nil {
		return true, "" // bootstrap grace: never-seen != dead
	}
	if now.Sub(*p.HeartbeatAt) > healthTTL {
		return false, "heartbeat stale"
	}
	return true, ""
}


// fallbackPositions parses the policy's ordered fallback list (a JSON array of provider
// names) into name->position. An empty or malformed list yields no positions, leaving pure
// score ordering. Duplicate names keep their first position.
func fallbackPositions(raw json.RawMessage) map[string]int {
	if len(raw) == 0 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil
	}
	pos := make(map[string]int, len(names))
	for i, n := range names {
		if _, ok := pos[n]; !ok {
			pos[n] = i
		}
	}
	return pos
}
