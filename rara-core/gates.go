// gates.go — Phase 3 deliverable #1: the curation cascade (the net-new value of 2.0).
//
// 1.0 distills everything; 2.0 SELECTS. A gate is just a capability (gate_barato on
// metadata before transcribing, gate_rico on full text before distilling). The decision
// is reached by a cheap->expensive cascade so the paid layer rarely runs:
//
//	rules (deterministic allow/deny, ~free)
//	   │ undecided
//	   ▼
//	interest_profile match (cheap, deterministic)
//	   │ on the fence
//	   ▼
//	LLM-judge (expensive — only the borderline middle)
//
// Each layer either DECIDES (returns a verdict) or ESCALATES (falls through). The result —
// keep / drop / defer — is recorded as a gate_decisions row (the worker writes it) and the
// reconciler routes the item from it (keep -> advance, drop -> filtered, defer -> quarantine).
//
// Everything here is PURE: runCascade takes the parsed profile + rules + an LLMJudge seam
// and returns a verdict with zero I/O, so the whole selection policy is unit-tested with a
// fake judge. The I/O edge (reading metadata/text, loading the profile, calling LiteLLM)
// lives in runners.go; this file is the decision logic only.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GateVerdict is one cascade decision. Score is the confidence in [0,1] (nil for the rules
// layer, which is deterministic and needs none). Rank is reserved for gate_rico cross-item
// ordering (nil this phase — true ranking needs batch context, deferred). DecidedBy names
// the layer that decided (rules | profile | llm) for the audit trail.
type GateVerdict struct {
	Decision  string
	Score     *float64
	Rank      *int
	DecidedBy string
	Reason    string
}

// gateInput is what a gate judges: metadata (title/channel) for gate_barato; metadata plus
// the full Text for gate_rico.
type gateInput struct {
	Title   string
	Channel string
	Text    string // empty for gate_barato (metadata only)
}

// profileDoc is the parsed interest_profile + the rules, ready for the cascade. The raw
// JSONB columns are parsed once at the I/O edge (parseProfile) so the cascade stays pure.
type profileDoc struct {
	Topics        []string
	Authors       []string
	AntiTopics    []string
	KeepThreshold float64 // profile-match score at/above which the profile layer keeps
	Rules         []GateRule
}

// LLMJudge is the borderline decider — the only paid layer. In production it is the LiteLLM
// gateway (runners.go); in tests it is a fake, so the cascade's escalation logic is verified
// without any network call. It judges only what rules + profile left "on the fence".
type LLMJudge interface {
	Judge(ctx context.Context, gate string, in gateInput, prof profileDoc) (GateVerdict, error)
}

// defaultKeepThreshold is the profile-match score at/above which the profile layer keeps an
// item outright (skipping the LLM). Below it, the item escalates. A first-cut value tuned by
// interest_profile.weights {"keep_threshold": x}; deliberately conservative so the profile
// never DROPS on its own (absence of a match is not rejection — that would re-create the
// cold-start false-negatives quarantine exists to fight). Only rules and the LLM drop.
const defaultKeepThreshold = 0.6

// runCascade walks the three layers in cost order, returning the first decision. rules and
// the LLM can keep/drop/defer; the profile layer only keeps-or-escalates (see above). The
// LLM is consulted ONLY when both cheaper layers abstain — the whole point of the cascade.
func runCascade(ctx context.Context, gate string, in gateInput, prof profileDoc, judge LLMJudge) (GateVerdict, error) {
	// 1) Rules — deterministic allow/deny, ~free.
	if v, decided := applyRules(in, prof.Rules); decided {
		return v, nil
	}
	// 2) interest_profile match — cheap, deterministic. Keeps a strong match; otherwise
	//    escalates (never drops — that is the LLM's or an explicit deny rule's job).
	score := profileMatch(in, prof)
	if score >= prof.KeepThreshold {
		s := score
		return GateVerdict{
			Decision: decisionKeep, Score: &s, DecidedBy: decidedByProfile,
			Reason: fmt.Sprintf("interest_profile match %.2f >= keep threshold %.2f", score, prof.KeepThreshold),
		}, nil
	}
	// 3) LLM-judge — the borderline middle only. May keep, drop, or (low confidence) defer.
	return judge.Judge(ctx, gate, in, prof)
}

// applyRules evaluates the deterministic layer. Deny precedence: a matched deny rule drops
// the item regardless of any allow match (an explicit deny always wins). A matched allow (and
// no deny) keeps it. No match -> not decided (escalate). Disabled rules are ignored (the read
// already filters them, but we double-check so a directly-built profileDoc behaves the same).
func applyRules(in gateInput, rules []GateRule) (GateVerdict, bool) {
	allowReason := ""
	for _, r := range rules {
		if !r.Enabled || !ruleMatches(r, in) {
			continue
		}
		if r.Action == ruleDeny {
			return GateVerdict{
				Decision: decisionDrop, DecidedBy: decidedByRules,
				Reason: "matched deny rule " + r.MatchType + "=" + r.Value,
			}, true
		}
		if allowReason == "" {
			allowReason = "matched allow rule " + r.MatchType + "=" + r.Value
		}
	}
	if allowReason != "" {
		return GateVerdict{Decision: decisionKeep, DecidedBy: decidedByRules, Reason: allowReason}, true
	}
	return GateVerdict{}, false
}

// ruleMatches reports whether a rule fires for the input. channel is an exact,
// case-insensitive name match; title_contains is a case-insensitive substring.
func ruleMatches(r GateRule, in gateInput) bool {
	switch r.MatchType {
	case matchChannel:
		return in.Channel != "" && strings.EqualFold(strings.TrimSpace(in.Channel), strings.TrimSpace(r.Value))
	case matchTitleContains:
		return r.Value != "" && strings.Contains(strings.ToLower(in.Title), strings.ToLower(r.Value))
	default:
		return false // unknown match_type: never fires (fail-closed)
	}
}

// profileMatch scores the input against the interest_profile in [0,1] (higher = more
// on-topic). Deterministic and cheap: count topic/author hits across title+channel+text,
// subtract anti-topic hits, and map the net through a saturating curve (net 1 -> 0.5, 2 ->
// 0.67, 3 -> 0.75 ...). A non-positive net scores 0 (escalate, never auto-drop). It is a
// first-cut heuristic meant to be tuned as feedback accumulates — the substance is that it
// is a pure function the cascade can lean on before paying for the LLM.
func profileMatch(in gateInput, prof profileDoc) float64 {
	hay := strings.ToLower(in.Title + "\n" + in.Channel + "\n" + in.Text)
	hits := 0
	for _, t := range prof.Topics {
		if t != "" && containsWord(hay, strings.ToLower(t)) {
			hits++
		}
	}
	for _, a := range prof.Authors {
		if a == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(in.Channel), strings.TrimSpace(a)) || containsWord(hay, strings.ToLower(a)) {
			hits++
		}
	}
	anti := 0
	for _, x := range prof.AntiTopics {
		if x != "" && containsWord(hay, strings.ToLower(x)) {
			anti++
		}
	}
	net := hits - anti
	if net <= 0 {
		return 0
	}
	return 1 - 1/(1+float64(net))
}

// containsWord reports whether token occurs in haystack delimited by word boundaries
// (the string edge or a non-alphanumeric byte on each side). Both args must already be
// lowercased. This avoids the substring trap where a short topic like "ai" would otherwise
// match "rain", "available" or "maintain"; a multi-word phrase ("platform engineering")
// still matches as a delimited run. Boundary detection is byte-level (ASCII alphanumerics) —
// any non-ASCII byte counts as a boundary, which is the right call for these tokens.
func containsWord(haystack, token string) bool {
	if token == "" {
		return false
	}
	for from := 0; from <= len(haystack)-len(token); {
		i := strings.Index(haystack[from:], token)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(token)
		beforeOK := start == 0 || !isAlnumByte(haystack[start-1])
		afterOK := end == len(haystack) || !isAlnumByte(haystack[end])
		if beforeOK && afterOK {
			return true
		}
		from = start + 1
	}
	return false
}

func isAlnumByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z'
}

// parseProfile turns the raw interest_profile JSONB columns + the enabled rules into the
// cascade's profileDoc. Lives at the I/O edge of the pure cascade: callers (the gate runner)
// load the rows and call this once. A malformed array yields an empty slice (the layer just
// contributes nothing), and the keep threshold falls back to the default unless weights
// carries a valid {"keep_threshold": x} in (0,1].
func parseProfile(p InterestProfile, rules []GateRule) profileDoc {
	doc := profileDoc{
		Topics:        parseStringArray(p.Topics),
		Authors:       parseStringArray(p.Authors),
		AntiTopics:    parseStringArray(p.AntiTopics),
		KeepThreshold: defaultKeepThreshold,
		Rules:         rules,
	}
	if len(p.Weights) > 0 {
		var w struct {
			KeepThreshold *float64 `json:"keep_threshold"`
		}
		if json.Unmarshal(p.Weights, &w) == nil && w.KeepThreshold != nil &&
			*w.KeepThreshold > 0 && *w.KeepThreshold <= 1 {
			doc.KeepThreshold = *w.KeepThreshold
		}
	}
	return doc
}

func parseStringArray(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}
