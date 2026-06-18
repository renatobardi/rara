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

// policyFor returns the capability-scoped routing policy, falling back to the global
// scope, then to a neutral default (equal cost<->quality weight, no fallback) so the
// router always has a policy even before one is seeded.
func (rt *Router) policyFor(ctx context.Context, capability string) (RoutingPolicy, error) {
	if p, ok, err := rt.db.GetRoutingPolicy(ctx, capability); err != nil {
		return RoutingPolicy{}, err
	} else if ok {
		return p, nil
	}
	if p, ok, err := rt.db.GetRoutingPolicy(ctx, policyScopeGlobal); err != nil {
		return RoutingPolicy{}, err
	} else if ok {
		return p, nil
	}
	return RoutingPolicy{Scope: policyScopeGlobal, CostWeight: 0.5, QualityWeight: 0.5}, nil
}

// rankProviders returns the eligible, healthy providers for a routing decision in
// preference order (best first). Filter, then order:
//
//	excluded  — names in `exclude` are dropped (the dead provider on timeout->fallback)
//	eligible  — hard constraints satisfied (constraintsSatisfied)
//	healthy   — resident providers heartbeat fresh; on_demand are exempt (providerHealthy)
//
// Order: providers named in the policy's ordered `fallback` come first, in listed order
// (an explicit operator failover chain that overrides scoring); the rest follow by
// descending cost<->quality score, ties broken by name for determinism. It is pure — no
// store access — so the whole selection policy is unit-tested without I/O.
func rankProviders(providers []Provider, policy RoutingPolicy, item Item, now time.Time, healthTTL time.Duration, exclude map[string]bool) []Provider {
	cands := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if exclude[p.Name] {
			continue
		}
		if !constraintsSatisfied(p, item) {
			continue
		}
		if !providerHealthy(p, now, healthTTL) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return nil
	}
	scores := scoreProviders(cands, policy)
	fbPos := fallbackPositions(policy.Fallback)
	sort.SliceStable(cands, func(i, j int) bool {
		pi, iIn := fbPos[cands[i].Name]
		pj, jIn := fbPos[cands[j].Name]
		if iIn && jIn {
			return pi < pj // both pinned: explicit fallback order
		}
		if iIn != jIn {
			return iIn // a fallback-listed provider precedes a non-listed one
		}
		if si, sj := scores[cands[i].Name], scores[cands[j].Name]; si != sj {
			return si > sj // higher score first
		}
		return cands[i].Name < cands[j].Name // deterministic tie-break
	})
	return cands
}

// scoreProviders maps each candidate to a cost<->quality score (higher is better). Quality
// is already normalized in [0,1]; cost is min-max normalized across the candidate set and
// inverted, so the cheapest candidate gets full cost credit and the dearest gets none. The
// policy's weights trade the two off. With an all-equal cost set the cost term is neutral
// (every candidate gets full credit) and quality alone decides.
func scoreProviders(cands []Provider, policy RoutingPolicy) map[string]float64 {
	minCost, maxCost := cands[0].Cost, cands[0].Cost
	for _, p := range cands {
		if p.Cost < minCost {
			minCost = p.Cost
		}
		if p.Cost > maxCost {
			maxCost = p.Cost
		}
	}
	span := maxCost - minCost
	scores := make(map[string]float64, len(cands))
	for _, p := range cands {
		costCredit := 1.0 // all-equal costs: neutral, so quality decides
		if span > 0 {
			costCredit = 1 - (p.Cost-minCost)/span
		}
		scores[p.Name] = policy.QualityWeight*p.Quality + policy.CostWeight*costCredit
	}
	return scores
}

// providerConstraints is the parsed shape of providers.constraints. Three keys are
// understood: `requires` (a runtime requirement), `accepts` (the item lanes this provider can
// consume), and `sensitivity` (a third-party tag). Unknown keys are ignored, but an unknown
// requirement VALUE fails closed (constraintsSatisfied).
type providerConstraints struct {
	Requires    string   `json:"requires"`
	Accepts     []string `json:"accepts"`
	Sensitivity string   `json:"sensitivity"`
}

// constraintsSatisfied reports whether a provider can serve a given ITEM under the hard
// constraints it declares. Three independent gates, all of which must pass:
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
func constraintsSatisfied(p Provider, item Item) bool {
	if len(p.Constraints) == 0 {
		return true // no constraints declared: serves any item
	}
	var c providerConstraints
	if err := json.Unmarshal(p.Constraints, &c); err != nil {
		return false
	}
	// 1) runtime requirement
	switch c.Requires {
	case "":
		// no hard runtime requirement declared
	case constraintResidential:
		if p.Runtime != runtimeLocal {
			return false
		}
	default:
		return false // unknown hard requirement: fail closed
	}
	// 2) source matching: a declared accepts-list must include the item's lane
	if len(c.Accepts) > 0 && !containsString(c.Accepts, item.Lane) {
		return false
	}
	// 3) sensitivity: third-party providers cannot process private content
	if item.Sensitivity == sensitivityPrivate && c.Sensitivity == constraintThirdParty {
		return false
	}
	return true
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

// providerHealthy reports whether a provider is alive enough to receive work. on_demand
// providers (Cloud Run, scale-to-zero) are asleep by design until the reconciler wakes
// them, so they always pass the health gate at SELECTION time; their post-assignment
// liveness is tracked per-step on item_steps.heartbeat_at (the reconciler's stale backstop).
//
// A resident provider (the Mac scribe, a VPC worker) is meant to be awake and heartbeating.
// We can only call one DEAD once we have seen it alive and then lost it: a STALE heartbeat
// (older than healthTTL) excludes it, so its work fails over. A provider we have NEVER heard
// from gets bootstrap grace — it is treated as starting-up, not dead. Without that grace the
// lane would deadlock: a resident only stamps its first heartbeat when it CLAIMS work, which
// it can only do once the router has selected it. The worker becomes "known alive" on its
// first claim (worker.go), after which staleness — the real failure signal — applies.
func providerHealthy(p Provider, now time.Time, healthTTL time.Duration) bool {
	if p.Activation == activationOnDemand {
		return true
	}
	if p.HeartbeatAt == nil {
		return true // bootstrap grace: never-seen != dead
	}
	return now.Sub(*p.HeartbeatAt) <= healthTTL
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
