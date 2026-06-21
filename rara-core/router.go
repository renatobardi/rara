// router.go — Policy-driven provider router.
//
// Routing decision: enabled + constraint (requires/accepts/sensitivity) + health + fallback order.
// Given the providers of a capability and a routing policy, the router:
//
//  1. excludes providers named in the `exclude` set (timeout-fallback path);
//  2. eliminates providers that fail a HARD constraint;
//  3. eliminates UNHEALTHY providers (resident with stale heartbeat; on_demand are exempt);
//  4. orders survivors by `fallback` list (pinned first, in declared order); non-pinned providers
//     break ties by name for determinism;
//  5. returns the head — or nothing, in which case the item waits.
//
// Zero I/O: all tests exercise rankProviders as a pure function and Router.Select over MockDatabase.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"
)

// defaultHealthTTL is how recently a RESIDENT provider must have heartbeat to count as alive.
// on_demand providers are exempt (scale-to-zero: asleep until the reconciler wakes them).
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
// ok=false when none is eligible+healthy (the item waits). now/healthTTL drive the heartbeat
// health gate. `exclude` names providers to skip — the timeout->fallback path passes the dead
// provider so Select returns the NEXT one in the ordered candidate chain.
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
				log.Printf("router: stepFallback %v contains no known %s provider — check for typos; preserving policy fallback", check, capability)
			} else {
				policy.Fallback = stepFallback
			}
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
// scope, then to a zero default so the router always has a policy even before one is seeded.
func policyForCapability(ctx context.Context, db Database, capability string) (RoutingPolicy, error) {
	if p, ok, err := db.GetRoutingPolicy(ctx, capability); err != nil {
		return RoutingPolicy{}, fmt.Errorf("routing policy for %q: %w", capability, err)
	} else if ok {
		return p, nil
	}
	if p, ok, err := db.GetRoutingPolicy(ctx, policyScopeGlobal); err != nil {
		return RoutingPolicy{}, fmt.Errorf("global routing policy: %w", err)
	} else if ok {
		return p, nil
	}
	return RoutingPolicy{Scope: policyScopeGlobal}, nil
}

// policyFor returns the routing policy for a capability. Delegates to policyForCapability.
func (rt *Router) policyFor(ctx context.Context, capability string) (RoutingPolicy, error) {
	return policyForCapability(ctx, rt.db, capability)
}

// rankProviders returns the eligible, healthy providers in preference order (best first).
// Eligibility: passes constraints and health. Order: fallback-pinned first (in declared order),
// then by name for determinism.
func rankProviders(providers []Provider, policy RoutingPolicy, item Item, now time.Time, healthTTL time.Duration, exclude map[string]bool) []Provider {
	fbPos := fallbackPositions(policy.Fallback)

	eligible := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		if exclude[p.Name] {
			continue
		}
		if !constraintsSatisfied(p, item) {
			continue
		}
		if !providerHealthy(p, now, healthTTL) {
			continue
		}
		eligible = append(eligible, p)
	}
	if len(eligible) == 0 {
		return nil
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		pi, iIn := fbPos[eligible[i].Name]
		pj, jIn := fbPos[eligible[j].Name]
		if iIn && jIn {
			return pi < pj
		}
		if iIn != jIn {
			return iIn
		}
		return eligible[i].Name < eligible[j].Name
	})
	return eligible
}

// providerConstraints is the parsed shape of providers.constraints. Three keys are understood:
// `requires` (a runtime requirement), `accepts` (the item lanes this provider can consume), and
// `sensitivity` (a third-party tag). Unknown keys fail closed (DisallowUnknownFields), as do
// unknown requirement values.
type providerConstraints struct {
	Requires    string   `json:"requires"`
	Accepts     []string `json:"accepts"`
	Sensitivity string   `json:"sensitivity"`
}

// constraintsSatisfied reports whether a provider's constraints allow it to serve item.
// Three independent gates, all of which must pass:
//
//  1. requires (runtime): e.g. asr-youtube declares {"requires":"residential"} — only runtime=local
//     qualifies. Fail-closed: an unrecognized requirement value makes the provider ineligible.
//  2. accepts (source matching): when a provider declares `accepts`, the item's lane must be listed.
//     A provider with no `accepts` serves any lane.
//  3. sensitivity: a {"sensitivity":"third_party"} provider is excluded for `private` content.
func constraintsSatisfied(p Provider, item Item) bool {
	if len(p.Constraints) == 0 {
		return true
	}
	var c providerConstraints
	dec := json.NewDecoder(bytes.NewReader(p.Constraints))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		log.Printf("constraintsSatisfied: provider %q has invalid constraints JSON: %v", p.Name, err)
		return false // malformed or unknown-key JSON: fail closed
	}
	switch c.Requires {
	case "":
	case constraintResidential:
		if p.Runtime != runtimeLocal {
			return false
		}
	default:
		return false // unknown requirement: fail closed
	}
	if len(c.Accepts) > 0 && !containsString(c.Accepts, item.Lane) {
		return false
	}
	switch c.Sensitivity {
	case "":
		// no restriction
	case constraintThirdParty:
		if item.Sensitivity == sensitivityPrivate {
			return false
		}
	default:
		return false // unknown sensitivity value: fail closed
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

// providerHealthy reports whether a provider is healthy enough to receive work.
// on_demand providers (Cloud Run, scale-to-zero) are always healthy at selection time —
// they're asleep by design until the reconciler wakes them. A resident (Mac, VPC) is healthy
// when its heartbeat is fresh, or has never been seen (bootstrap grace: never-seen != dead).
func providerHealthy(p Provider, now time.Time, healthTTL time.Duration) bool {
	if p.Activation == activationOnDemand {
		return true
	}
	if p.HeartbeatAt == nil {
		return true // bootstrap grace
	}
	return now.Sub(*p.HeartbeatAt) <= healthTTL
}

// fallbackPositions parses the policy's ordered fallback list (a JSON array of provider names)
// into name->position (0-indexed). An empty or malformed list yields nil, leaving pure name-order.
// Duplicate names keep their first position.
func fallbackPositions(raw json.RawMessage) map[string]int {
	if len(raw) == 0 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		log.Printf("router: fallback policy has malformed JSON — falling back to name-order: %v", err)
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
