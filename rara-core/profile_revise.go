// profile_revise.go — Phase 6 PIECE 2: the learning loop (the interest_profile reviser).
//
// A single closed loop through a HUMAN-READABLE artifact, no training infra (ARCHITECTURE-2.0,
// "The learning loop"): accumulated feedback -> (periodic) revise interest_profile -> next gate
// decisions. The revision is HYBRID — a deterministic engine and an LLM with strictly separated
// jobs:
//
//   - The DETERMINISTIC engine owns the STRUCTURED change. It aggregates feedback (thumbs +
//     quarantine review) attributing each signal to the concepts/entities/author of the
//     `structured` of the rated distillation, scores each term with a smoothed estimator
//     (Wilson lower bound), and promotes strong terms to topics/authors, demotes reliably
//     disliked ones to anti_topics, and nudges keep_threshold from the quarantine outcomes —
//     all under a hard CEILING on how much may change per revision. It NEVER invents a change
//     the counts do not justify.
//   - The LLM (via LiteLLM) owns ONLY the natural-language NARRATIVE — the prose the gate's
//     LLM-judge reads as context. It cannot touch a single structured field.
//
// A revision is appended as a NEW version with status `proposed`; it does NOT take effect until
// a human APPROVES it (ActivateInterestProfile / the surface's approve action). The gate cascade
// reads the ACTIVE version, never the latest — so a proposal is inert until approved.
//
// Everything here is PURE over the Database seam + two narrow seams (a distillation `structured`
// resolver and the narrator), so the whole reviser is unit-tested with the MockDatabase and
// fakes — zero I/O. The pgx resolver and the LiteLLM narrator live at the I/O edge (runners.go).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// reviseConfig tunes both the trigger (cadence/threshold/debounce) and the deterministic engine
// (the Wilson thresholds + the per-revision ceiling). defaultReviseConfig holds conservative
// values; the trigger knobs are env-overridable in runRevise.
type reviseConfig struct {
	// Trigger.
	Cadence           time.Duration // revise at least this often (if there is new feedback)
	FeedbackThreshold int           // ...or sooner, once this many new signals accumulate
	Debounce          time.Duration // never revise within this window of the last revision

	// Engine.
	PromoteThreshold  float64 // up-rate Wilson lower bound at/above which a term -> topics/authors
	DemoteThreshold   float64 // down-rate Wilson lower bound at/above which a term -> anti_topics
	MinSample         int     // minimum up+down before a term (or the quarantine set) may move
	MaxPromotions     int     // CEILING: terms promoted per revision (topics+authors combined)
	MaxDemotions      int     // CEILING: terms demoted to anti_topics per revision
	MaxThresholdDelta float64 // CEILING: |keep_threshold change| per revision
	MinKeepThreshold  float64 // keep_threshold clamp floor
	MaxKeepThreshold  float64 // keep_threshold clamp ceiling
	WilsonZ           float64 // Wilson z (1.96 ~ 95% lower bound)
}

func defaultReviseConfig() reviseConfig {
	return reviseConfig{
		Cadence:           7 * 24 * time.Hour,
		FeedbackThreshold: 20,
		Debounce:          24 * time.Hour,
		PromoteThreshold:  0.6,
		DemoteThreshold:   0.6,
		MinSample:         3,
		MaxPromotions:     3,
		MaxDemotions:      3,
		MaxThresholdDelta: 0.05,
		MinKeepThreshold:  0.3,
		MaxKeepThreshold:  0.9,
		WilsonZ:           1.96,
	}
}

// ---------------------------------------------------------------------------
// Engine inputs / outputs (pure)
// ---------------------------------------------------------------------------

// signalCount is the up/down tally attributed to one term.
type signalCount struct{ Up, Down int }

// feedbackAggregate is the per-term tally the engine consumes, plus the quarantine-review
// outcomes that tune keep_threshold. Built by aggregateFeedback at the I/O edge.
type feedbackAggregate struct {
	Topics         map[string]signalCount // concept/entity term -> tally (concepts ∪ entities)
	Authors        map[string]signalCount // source author -> tally
	QuarantineUp   int                    // quarantine reviews that RESCUED (false-negative signal)
	QuarantineDown int                    // quarantine reviews that CONFIRMED the drop
}

// revisedStructured is the structured half of a profile version (the engine's input base and its
// output). Terms are lowercased so the set arithmetic and the gate's case-insensitive matcher
// agree.
type revisedStructured struct {
	Topics        []string
	Authors       []string
	AntiTopics    []string
	KeepThreshold float64
}

// ---------------------------------------------------------------------------
// Seams (the only non-pure dependencies; faked in tests)
// ---------------------------------------------------------------------------

// DistillationStructured is the slice of a distillation's `structured` JSONB the reviser
// attributes a signal to: its concepts + entities (-> topics) and source author (-> authors).
type DistillationStructured struct {
	Concepts []string
	Entities []string
	Author   string
}

// DistillationResolver reads a distillation's structured terms by id. The pgx implementation
// (runners.go) SELECTs distillations.structured (cross-agent read, no FK — the 1.0 isolation
// convention); tests inject a fake. found=false for a distillation that has since vanished.
type DistillationResolver interface {
	Resolve(ctx context.Context, distillationID string) (DistillationStructured, bool, error)
}

// ProfileNarrator writes ONLY the natural-language narrative of a proposed profile (the gate's
// LLM-judge context). The LiteLLM implementation lives at the I/O edge; a failure is non-fatal
// (the reviser falls back to a deterministic template) so the structured revision is robust.
type ProfileNarrator interface {
	Narrate(ctx context.Context, old, proposed revisedStructured) (string, error)
}

// ---------------------------------------------------------------------------
// Trigger
// ---------------------------------------------------------------------------

// shouldRevise decides whether to propose a revision now. It fires when the cadence has elapsed
// OR enough new feedback has accumulated — but never while a proposal already awaits approval
// (no stacking), never within the debounce window of the last revision, and never with zero new
// signal (a revision with nothing to learn from would only create a no-op proposal to approve).
func shouldRevise(now, lastRevision time.Time, proposedPending bool, feedbackSince int, cfg reviseConfig) bool {
	if proposedPending {
		return false // a proposal is already pending approval — don't stack another
	}
	if feedbackSince == 0 {
		return false // nothing new to learn from
	}
	elapsed := now.Sub(lastRevision)
	if elapsed < cfg.Debounce {
		return false
	}
	return elapsed >= cfg.Cadence || feedbackSince >= cfg.FeedbackThreshold
}

// ---------------------------------------------------------------------------
// Deterministic engine
// ---------------------------------------------------------------------------

// wilsonLowerBound is the Wilson score interval's lower bound for a binomial proportion — a
// conservative estimate of the true positive rate that shrinks toward 0 on small samples, so a
// term seen up 2/2 does not outrank one up 40/45. Returns 0 for an empty sample.
func wilsonLowerBound(up, total int, z float64) float64 {
	if total <= 0 {
		return 0
	}
	n := float64(total)
	phat := float64(up) / n
	z2 := z * z
	denom := 1 + z2/n
	centre := phat + z2/(2*n)
	margin := z * math.Sqrt((phat*(1-phat)+z2/(4*n))/n)
	lb := (centre - margin) / denom
	if lb < 0 {
		return 0
	}
	return lb
}

// termCand is a ranked promote/demote candidate (author=false => a topic, true => an author).
type termCand struct {
	term   string
	author bool
	score  float64
}

// reviseProfileStructured is the deterministic engine: it derives the next structured profile
// from the base + the aggregated signal, under the per-revision ceiling. It promotes terms whose
// up-rate is reliably high, demotes terms whose down-rate is reliably high (moving them to
// anti_topics and out of topics/authors), and nudges keep_threshold from the quarantine
// outcomes. It NEVER changes more than the ceiling allows, and never invents a term with no
// supporting counts.
func reviseProfileStructured(base revisedStructured, agg feedbackAggregate, cfg reviseConfig) revisedStructured {
	topics := newStrSet(base.Topics)
	authors := newStrSet(base.Authors)
	anti := newStrSet(base.AntiTopics)

	var promote, demote []termCand
	collect := func(counts map[string]signalCount, isAuthor bool, present *strSet) {
		for raw, c := range counts {
			term := strings.TrimSpace(strings.ToLower(raw))
			if term == "" {
				continue
			}
			total := c.Up + c.Down
			if total < cfg.MinSample {
				continue
			}
			upLB := wilsonLowerBound(c.Up, total, cfg.WilsonZ)
			downLB := wilsonLowerBound(c.Down, total, cfg.WilsonZ)
			switch {
			case upLB >= cfg.PromoteThreshold && !present.has(term):
				promote = append(promote, termCand{term, isAuthor, upLB})
			case downLB >= cfg.DemoteThreshold && !anti.has(term):
				demote = append(demote, termCand{term, isAuthor, downLB})
			}
		}
	}
	collect(agg.Topics, false, topics)
	collect(agg.Authors, true, authors)
	sortCandidates(promote)
	sortCandidates(demote)

	// Demotions first (a downvoted term joins anti_topics), capped. Remove it from BOTH buckets,
	// not just the one it was signalled in, so a term can never sit in anti_topics AND topics/
	// authors at once (which would net out in profileMatch).
	for i, c := range demote {
		if i >= cfg.MaxDemotions {
			break
		}
		topics.del(c.term)
		authors.del(c.term)
		anti.add(c.term)
	}
	// Promotions (a term joins its bucket and leaves anti_topics if it was there), capped.
	for i, c := range promote {
		if i >= cfg.MaxPromotions {
			break
		}
		if c.author {
			authors.add(c.term)
		} else {
			topics.add(c.term)
		}
		anti.del(c.term)
	}

	return revisedStructured{
		Topics:        topics.sorted(),
		Authors:       authors.sorted(),
		AntiTopics:    anti.sorted(),
		KeepThreshold: reviseKeepThreshold(base.KeepThreshold, agg, cfg),
	}
}

// reviseKeepThreshold nudges the profile-layer keep threshold from the quarantine outcomes. A
// high RESCUE rate means the gate is deferring good items (false negatives) → LOWER the
// threshold (the profile layer keeps more outright). A high CONFIRM rate means the defers were
// correctly junk → RAISE it slightly. The move is bounded by the ceiling (the rate difference is
// in [-1,1], scaled by MaxThresholdDelta) and the result clamped to [Min,Max].
func reviseKeepThreshold(base float64, agg feedbackAggregate, cfg reviseConfig) float64 {
	if base <= 0 {
		base = defaultKeepThreshold
	}
	total := agg.QuarantineUp + agg.QuarantineDown
	if total < cfg.MinSample {
		return clampFloat(base, cfg.MinKeepThreshold, cfg.MaxKeepThreshold)
	}
	rescueLB := wilsonLowerBound(agg.QuarantineUp, total, cfg.WilsonZ)
	confirmLB := wilsonLowerBound(agg.QuarantineDown, total, cfg.WilsonZ)
	delta := (confirmLB - rescueLB) * cfg.MaxThresholdDelta
	return clampFloat(base+delta, cfg.MinKeepThreshold, cfg.MaxKeepThreshold)
}

// sortCandidates orders candidates by score desc, then term asc — deterministic, so the ceiling
// keeps the strongest-signalled changes and ties resolve stably.
func sortCandidates(cs []termCand) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].term < cs[j].term
	})
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

// ReviseProfile is the learning loop's one entry point. It decides (cadence/threshold/debounce)
// whether to revise; if so it aggregates the new feedback, runs the deterministic engine for the
// structured change, asks the narrator for the prose (non-fatal), and appends the result as a
// `proposed` version. It returns the proposed version number and revised=true when it proposed
// one, or (0,false,nil) when the trigger said "not yet" (or there is no active base to revise).
func ReviseProfile(ctx context.Context, db Database, resolver DistillationResolver, narrator ProfileNarrator, now time.Time, cfg reviseConfig) (int, bool, error) {
	active, found, err := db.GetActiveInterestProfile(ctx)
	if err != nil {
		return 0, false, err
	}
	if !found {
		log.Printf("revise: no active interest_profile to revise from; skipping")
		return 0, false, nil
	}

	profiles, err := db.ListInterestProfiles(ctx)
	if err != nil {
		return 0, false, err
	}
	nextVersion, proposedPending, lastRevision := 1, false, time.Time{}
	for _, p := range profiles {
		if p.Version >= nextVersion {
			nextVersion = p.Version + 1
		}
		if p.CreatedAt.After(lastRevision) {
			lastRevision = p.CreatedAt
		}
		if p.Status == profileProposed {
			proposedPending = true
		}
	}

	window, err := db.ListFeedbackSince(ctx, lastRevision)
	if err != nil {
		return 0, false, err
	}

	if !shouldRevise(now, lastRevision, proposedPending, len(window), cfg) {
		return 0, false, nil
	}

	agg, err := aggregateFeedback(ctx, resolver, window)
	if err != nil {
		return 0, false, err
	}
	base := baseFromProfile(active)
	proposed := reviseProfileStructured(base, agg, cfg)
	narrative := narrate(ctx, narrator, base, proposed)

	if err := db.InsertInterestProfile(ctx, InterestProfile{
		Version:    nextVersion,
		Topics:     marshalStringArray(proposed.Topics),
		Authors:    marshalStringArray(proposed.Authors),
		AntiTopics: marshalStringArray(proposed.AntiTopics),
		Weights:    weightsWithKeepThreshold(active.Weights, proposed.KeepThreshold),
		Narrative:  narrative,
		Status:     profileProposed,
	}); err != nil {
		return 0, false, err
	}
	log.Printf("revise: proposed interest_profile v%d (topics=%d authors=%d anti=%d keep_threshold=%.2f) — awaiting approval",
		nextVersion, len(proposed.Topics), len(proposed.Authors), len(proposed.AntiTopics), proposed.KeepThreshold)
	return nextVersion, true, nil
}

// aggregateFeedback attributes each signal in the window to terms (for distillation thumbs, via
// the resolved `structured`) or to the quarantine outcomes (for quarantine reviews). Distillation
// thumbs carry source user_explicit or kura_implicit; quarantine reviews carry quarantine_review.
func aggregateFeedback(ctx context.Context, resolver DistillationResolver, window []Feedback) (feedbackAggregate, error) {
	agg := feedbackAggregate{Topics: map[string]signalCount{}, Authors: map[string]signalCount{}}
	for _, fb := range window {
		up := fb.Signal == signalUp
		if fb.Signal != signalUp && fb.Signal != signalDown {
			continue // only up/down carry a learning direction
		}
		switch {
		case fb.Source == sourceQuarantineReview && fb.TargetType == targetItem:
			if up {
				agg.QuarantineUp++
			} else {
				agg.QuarantineDown++
			}
		case fb.TargetType == targetDistillation:
			ds, ok, err := resolver.Resolve(ctx, fb.TargetRef)
			if err != nil {
				return feedbackAggregate{}, err
			}
			if !ok {
				continue
			}
			for _, t := range ds.Concepts {
				bumpSignal(agg.Topics, t, up)
			}
			for _, e := range ds.Entities {
				bumpSignal(agg.Topics, e, up)
			}
			bumpSignal(agg.Authors, ds.Author, up)
		}
	}
	return agg, nil
}

func bumpSignal(m map[string]signalCount, raw string, up bool) {
	term := strings.TrimSpace(strings.ToLower(raw))
	if term == "" {
		return
	}
	c := m[term]
	if up {
		c.Up++
	} else {
		c.Down++
	}
	m[term] = c
}

// narrate asks the narrator for the prose, falling back to a deterministic template on any error
// or empty result — the structured revision must never be blocked by the optional LLM step.
func narrate(ctx context.Context, narrator ProfileNarrator, base, proposed revisedStructured) string {
	if narrator != nil {
		s, err := narrator.Narrate(ctx, base, proposed)
		if err != nil {
			log.Printf("revise: narrator failed, using template narrative: %v", err)
		} else if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return templateNarrative(proposed)
}

func templateNarrative(p revisedStructured) string {
	return fmt.Sprintf("Curation profile: %d topics, %d authors, %d anti-topics; keep_threshold %.2f. "+
		"(Deterministic summary — narrator unavailable.)",
		len(p.Topics), len(p.Authors), len(p.AntiTopics), p.KeepThreshold)
}

// baseFromProfile parses an active profile's JSONB into the engine's base struct.
func baseFromProfile(p InterestProfile) revisedStructured {
	return revisedStructured{
		Topics:        parseStringArray(p.Topics),
		Authors:       parseStringArray(p.Authors),
		AntiTopics:    parseStringArray(p.AntiTopics),
		KeepThreshold: keepThresholdFromWeights(p.Weights),
	}
}

// keepThresholdFromWeights reads weights.keep_threshold, defaulting when absent/invalid (mirrors
// parseProfile's rule in gates.go).
func keepThresholdFromWeights(raw json.RawMessage) float64 {
	if len(raw) > 0 {
		var w struct {
			KeepThreshold *float64 `json:"keep_threshold"`
		}
		if json.Unmarshal(raw, &w) == nil && w.KeepThreshold != nil && *w.KeepThreshold > 0 && *w.KeepThreshold <= 1 {
			return *w.KeepThreshold
		}
	}
	return defaultKeepThreshold
}

// weightsWithKeepThreshold preserves any other weight keys from the base and sets keep_threshold.
func weightsWithKeepThreshold(baseWeights json.RawMessage, keep float64) json.RawMessage {
	m := map[string]any{}
	if len(baseWeights) > 0 {
		_ = json.Unmarshal(baseWeights, &m)
	}
	m["keep_threshold"] = keep
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"keep_threshold":%g}`, keep))
	}
	return b
}

// marshalStringArray marshals to a JSON array, never null (an empty slice -> "[]").
func marshalStringArray(s []string) json.RawMessage {
	if len(s) == 0 {
		return json.RawMessage("[]")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

// parseDistillationStructured extracts the attribution terms from a distillation's `structured`
// JSONB (concepts ∪ entities -> topic signal; author -> author signal). It is tolerant of the
// two shapes the field takes in practice — a bare ["a","b"] array or an array of objects
// ({"name":...}/{"term":...}/{"label":...}) — so the reviser survives distill schema drift. Pure,
// so it is unit-tested; the pgx resolver (runners.go) only does the SELECT and calls this.
func parseDistillationStructured(raw []byte) DistillationStructured {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return DistillationStructured{}
	}
	return DistillationStructured{
		Concepts: jsonTerms(doc["concepts"]),
		Entities: jsonTerms(doc["entities"]),
		Author:   jsonStringField(doc["author"]),
	}
}

// jsonTerms parses a term array that is either a list of strings or a list of objects carrying
// the term under a common key.
func jsonTerms(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		return trimNonEmpty(ss)
	}
	var objs []map[string]any
	if json.Unmarshal(raw, &objs) == nil {
		var out []string
		for _, o := range objs {
			for _, k := range []string{"name", "term", "label", "value", "entity", "concept"} {
				if v, ok := o[k].(string); ok && strings.TrimSpace(v) != "" {
					out = append(out, v)
					break
				}
			}
		}
		return out
	}
	return nil
}

// jsonStringField reads a string field that may be a bare string or an object with a name.
func jsonStringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o map[string]any
	if json.Unmarshal(raw, &o) == nil {
		if v, ok := o["name"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func trimNonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// strSet — a small lowercase-normalized string set with deterministic output.
// ---------------------------------------------------------------------------

type strSet struct{ m map[string]struct{} }

func newStrSet(items []string) *strSet {
	s := &strSet{m: make(map[string]struct{}, len(items))}
	for _, it := range items {
		s.add(it)
	}
	return s
}

func (s *strSet) add(v string) {
	if t := strings.TrimSpace(strings.ToLower(v)); t != "" {
		s.m[t] = struct{}{}
	}
}

func (s *strSet) del(v string) { delete(s.m, strings.TrimSpace(strings.ToLower(v))) }

func (s *strSet) has(v string) bool {
	_, ok := s.m[strings.TrimSpace(strings.ToLower(v))]
	return ok
}

func (s *strSet) sorted() []string {
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
