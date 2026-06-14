package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// feedback.source values used only as test fixtures. Production hone branches solely on
// sourceQuarantineReview (store.go); the distillation-attribution path keys off target_type, not
// source, so these two carry no production constant.
const (
	sourceUserExplicit = "user_explicit"
	sourceKURAImplicit = "kura_implicit"
)

// testReviseConfig is a relaxed config that makes the deterministic engine easy to reason about
// in unit tests (low z, small samples), independent of the production defaults.
func testReviseConfig() reviseConfig {
	return reviseConfig{
		Cadence:           7 * 24 * time.Hour,
		FeedbackThreshold: 5,
		Debounce:          1 * time.Hour,
		PromoteThreshold:  0.5,
		DemoteThreshold:   0.5,
		MinSample:         2,
		MaxPromotions:     3,
		MaxDemotions:      3,
		MaxThresholdDelta: 0.10,
		MinKeepThreshold:  0.3,
		MaxKeepThreshold:  0.9,
		WilsonZ:           1.0,
	}
}

// ---------------------------------------------------------------------------
// wilsonLowerBound — pure smoothed estimator.
// ---------------------------------------------------------------------------

func TestWilsonLowerBoundProperties(t *testing.T) {
	// Empty sample -> 0.
	if got := wilsonLowerBound(0, 0, 1.96); got != 0 {
		t.Errorf("empty sample LB = %v, want 0", got)
	}
	// More evidence at the same rate -> higher lower bound (small-sample conservatism).
	small := wilsonLowerBound(2, 2, 1.96)
	big := wilsonLowerBound(40, 40, 1.96)
	if !(big > small) {
		t.Errorf("LB should grow with sample size at the same rate: 2/2=%.3f, 40/40=%.3f", small, big)
	}
	// The lower bound is below the point estimate.
	if lb := wilsonLowerBound(8, 10, 1.96); !(lb < 0.8) {
		t.Errorf("LB(8/10)=%.3f should be below phat=0.8", lb)
	}
	// All-negative sample -> 0 positive lower bound.
	if lb := wilsonLowerBound(0, 5, 1.96); lb != 0 {
		t.Errorf("LB(0/5)=%.3f, want 0", lb)
	}
}

// ---------------------------------------------------------------------------
// reviseProfileStructured — the deterministic engine.
// ---------------------------------------------------------------------------

func TestRevisePromotesStrongTerms(t *testing.T) {
	cfg := testReviseConfig()
	base := revisedStructured{Topics: []string{"ai"}, KeepThreshold: 0.6}
	agg := feedbackAggregate{
		Topics:  map[string]signalCount{"kubernetes": {Up: 3}},
		Authors: map[string]signalCount{"renato": {Up: 4}},
	}
	out := reviseProfileStructured(base, agg, cfg)
	if !contains(out.Topics, "kubernetes") || !contains(out.Topics, "ai") {
		t.Errorf("topics = %v, want ai + kubernetes promoted", out.Topics)
	}
	if !contains(out.Authors, "renato") {
		t.Errorf("authors = %v, want renato promoted", out.Authors)
	}
}

func TestReviseDemotesDislikedTerms(t *testing.T) {
	cfg := testReviseConfig()
	base := revisedStructured{Topics: []string{"ai", "crypto"}, KeepThreshold: 0.6}
	agg := feedbackAggregate{Topics: map[string]signalCount{"crypto": {Down: 4}}}
	out := reviseProfileStructured(base, agg, cfg)
	if contains(out.Topics, "crypto") {
		t.Errorf("crypto should be demoted out of topics: %v", out.Topics)
	}
	if !contains(out.AntiTopics, "crypto") {
		t.Errorf("crypto should land in anti_topics: %v", out.AntiTopics)
	}
	if !contains(out.Topics, "ai") {
		t.Errorf("ai (untouched) should remain: %v", out.Topics)
	}
}

func TestRevivePromotionFromAntiTopics(t *testing.T) {
	// A term previously demoted that now earns strong upvotes is rehabilitated (anti -> topics).
	cfg := testReviseConfig()
	base := revisedStructured{AntiTopics: []string{"llm"}, KeepThreshold: 0.6}
	agg := feedbackAggregate{Topics: map[string]signalCount{"llm": {Up: 5}}}
	out := reviseProfileStructured(base, agg, cfg)
	if contains(out.AntiTopics, "llm") || !contains(out.Topics, "llm") {
		t.Errorf("llm should move anti -> topics: topics=%v anti=%v", out.Topics, out.AntiTopics)
	}
}

func TestReviseRespectsPromotionCeiling(t *testing.T) {
	cfg := testReviseConfig()
	cfg.MaxPromotions = 2
	base := revisedStructured{KeepThreshold: 0.6}
	// Five strong candidates with distinct strengths; only the top 2 by score may be added.
	agg := feedbackAggregate{Topics: map[string]signalCount{
		"a": {Up: 2}, "b": {Up: 4}, "c": {Up: 8}, "d": {Up: 16}, "e": {Up: 3},
	}}
	out := reviseProfileStructured(base, agg, cfg)
	if len(out.Topics) != 2 {
		t.Fatalf("ceiling MaxPromotions=2 not respected: %v", out.Topics)
	}
	// The two strongest (largest samples at rate 1.0) are d and c.
	if !contains(out.Topics, "d") || !contains(out.Topics, "c") {
		t.Errorf("ceiling should keep the strongest-signalled terms, got %v", out.Topics)
	}
}

func TestReviseDoesNotInventBelowMinSample(t *testing.T) {
	cfg := testReviseConfig() // MinSample = 2
	base := revisedStructured{Topics: []string{"ai"}, KeepThreshold: 0.6}
	agg := feedbackAggregate{Topics: map[string]signalCount{"kubernetes": {Up: 1}}} // 1 < MinSample
	out := reviseProfileStructured(base, agg, cfg)
	if contains(out.Topics, "kubernetes") {
		t.Errorf("a single signal must not promote: %v", out.Topics)
	}
	if len(out.Topics) != 1 {
		t.Errorf("nothing should change below MinSample: %v", out.Topics)
	}
}

func TestReviseKeepThresholdLowersOnRescues(t *testing.T) {
	cfg := testReviseConfig()
	base := revisedStructured{KeepThreshold: 0.6}
	// Mostly rescues -> the gate is too strict (false negatives) -> lower the threshold.
	agg := feedbackAggregate{QuarantineUp: 8, QuarantineDown: 0}
	out := reviseProfileStructured(base, agg, cfg)
	if !(out.KeepThreshold < 0.6) {
		t.Errorf("rescue-heavy quarantine should lower keep_threshold, got %.3f", out.KeepThreshold)
	}
	if out.KeepThreshold < 0.6-cfg.MaxThresholdDelta-1e-9 {
		t.Errorf("keep_threshold moved beyond the ceiling: %.3f", out.KeepThreshold)
	}
}

func TestReviseKeepThresholdRaisesOnConfirms(t *testing.T) {
	cfg := testReviseConfig()
	base := revisedStructured{KeepThreshold: 0.6}
	agg := feedbackAggregate{QuarantineUp: 0, QuarantineDown: 8}
	out := reviseProfileStructured(base, agg, cfg)
	if !(out.KeepThreshold > 0.6) {
		t.Errorf("confirm-heavy quarantine should raise keep_threshold, got %.3f", out.KeepThreshold)
	}
}

func TestReviseKeepThresholdClamped(t *testing.T) {
	cfg := testReviseConfig()
	cfg.MaxThresholdDelta = 0.5
	base := revisedStructured{KeepThreshold: 0.85}
	agg := feedbackAggregate{QuarantineUp: 0, QuarantineDown: 20}
	out := reviseProfileStructured(base, agg, cfg)
	if out.KeepThreshold > cfg.MaxKeepThreshold+1e-9 {
		t.Errorf("keep_threshold must be clamped to MaxKeepThreshold=%.2f, got %.3f", cfg.MaxKeepThreshold, out.KeepThreshold)
	}
}

// ---------------------------------------------------------------------------
// shouldRevise — the trigger.
// ---------------------------------------------------------------------------

func TestShouldRevise(t *testing.T) {
	cfg := testReviseConfig() // Cadence 7d, Threshold 5, Debounce 1h
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lastWeek := now.Add(-8 * 24 * time.Hour)
	lastHour := now.Add(-90 * time.Minute)
	justNow := now.Add(-10 * time.Minute)

	cases := []struct {
		name    string
		last    time.Time
		pending bool
		since   int
		want    bool
	}{
		{"cadence elapsed with feedback", lastWeek, false, 1, true},
		{"threshold met within cadence", lastHour, false, 5, true},
		{"below threshold, within cadence", lastHour, false, 4, false},
		{"proposal pending blocks", lastWeek, true, 50, false},
		{"zero feedback never fires", lastWeek, false, 0, false},
		{"debounce blocks recent revision", justNow, false, 50, false},
	}
	for _, c := range cases {
		if got := shouldRevise(now, c.last, c.pending, c.since, cfg); got != c.want {
			t.Errorf("%s: shouldRevise = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseDistillationStructured — flexible JSON shapes.
// ---------------------------------------------------------------------------

func TestParseDistillationStructured(t *testing.T) {
	// Bare string arrays + a string author.
	a := parseDistillationStructured([]byte(`{"concepts":["kubernetes","ai"],"entities":["istio"],"author":"Renato"}`))
	if len(a.Concepts) != 2 || a.Entities[0] != "istio" || a.Author != "Renato" {
		t.Errorf("string-array shape parsed wrong: %+v", a)
	}
	// Object arrays + object author.
	b := parseDistillationStructured([]byte(`{"concepts":[{"name":"devops"}],"entities":[{"label":"k8s"}],"author":{"name":"Ana"}}`))
	if len(b.Concepts) != 1 || b.Concepts[0] != "devops" || b.Entities[0] != "k8s" || b.Author != "Ana" {
		t.Errorf("object shape parsed wrong: %+v", b)
	}
	// Empty / malformed -> empty struct, no panic.
	if c := parseDistillationStructured([]byte(`not json`)); len(c.Concepts) != 0 || c.Author != "" {
		t.Errorf("malformed structured should yield empty: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// aggregateFeedback — attribution over the resolver seam.
// ---------------------------------------------------------------------------

type fakeResolver struct {
	m map[string]DistillationStructured
}

func (f fakeResolver) Resolve(_ context.Context, id string) (DistillationStructured, bool, error) {
	d, ok := f.m[id]
	return d, ok, nil
}

func TestAggregateFeedback(t *testing.T) {
	ctx := context.Background()
	resolver := fakeResolver{m: map[string]DistillationStructured{
		"d1": {Concepts: []string{"kubernetes"}, Author: "Renato"},
		"d2": {Concepts: []string{"kubernetes"}, Entities: []string{"istio"}, Author: "Renato"},
	}}
	window := []Feedback{
		{TargetType: targetDistillation, TargetRef: "d1", Signal: signalUp, Source: sourceUserExplicit},
		{TargetType: targetDistillation, TargetRef: "d2", Signal: signalUp, Source: sourceKURAImplicit},
		{TargetType: targetDistillation, TargetRef: "d2", Signal: signalDown, Source: sourceUserExplicit},
		{TargetType: targetItem, TargetRef: "7", Signal: signalUp, Source: sourceQuarantineReview},
		{TargetType: targetItem, TargetRef: "8", Signal: signalDown, Source: sourceQuarantineReview},
		{TargetType: targetDistillation, TargetRef: "missing", Signal: signalUp, Source: sourceUserExplicit},
	}
	agg, err := aggregateFeedback(ctx, resolver, window)
	if err != nil {
		t.Fatal(err)
	}
	// kubernetes: up from d1 + up from d2, down from d2 -> {Up:2, Down:1}.
	if c := agg.Topics["kubernetes"]; c.Up != 2 || c.Down != 1 {
		t.Errorf("kubernetes tally = %+v, want {2,1}", c)
	}
	if c := agg.Topics["istio"]; c.Up != 1 || c.Down != 1 {
		t.Errorf("istio tally = %+v, want {1,1}", c)
	}
	if c := agg.Authors["renato"]; c.Up != 2 || c.Down != 1 {
		t.Errorf("renato tally = %+v, want {2,1}", c)
	}
	if agg.QuarantineUp != 1 || agg.QuarantineDown != 1 {
		t.Errorf("quarantine = up%d/down%d, want 1/1", agg.QuarantineUp, agg.QuarantineDown)
	}
}

// ---------------------------------------------------------------------------
// ReviseProfile — orchestration over the seams.
// ---------------------------------------------------------------------------

type fakeNarrator struct {
	text  string
	err   error
	calls int
}

func (f *fakeNarrator) Narrate(_ context.Context, _, _ revisedStructured) (string, error) {
	f.calls++
	return f.text, f.err
}

// seedActiveProfile inserts an active v1 with a known created_at so ReviseProfile can window
// feedback after it.
func seedActiveProfile(t *testing.T, db *MockDatabase, createdAt time.Time, topics string) {
	t.Helper()
	if err := db.InsertInterestProfile(context.Background(), InterestProfile{
		Version: 1, Topics: json.RawMessage(topics), Weights: json.RawMessage(`{"keep_threshold":0.6}`),
		Status: profileActive, CreatedAt: createdAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviseProfileProposesNewVersion(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedActiveProfile(t, db, t0, `["ai"]`)

	resolver := fakeResolver{m: map[string]DistillationStructured{
		"d1": {Concepts: []string{"kubernetes"}},
	}}
	// Three up-thumbs on d1 (kubernetes), each after the active profile's created_at.
	for i := 0; i < 3; i++ {
		if err := db.InsertFeedback(ctx, Feedback{
			TargetType: targetDistillation, TargetRef: "d1", Signal: signalUp,
			Source: sourceUserExplicit, CreatedAt: t0.Add(time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	narrator := &fakeNarrator{text: "Reader likes ai and kubernetes."}
	now := t0.Add(8 * 24 * time.Hour) // cadence elapsed

	version, revised, err := ReviseProfile(ctx, db, resolver, narrator, now, testReviseConfig())
	if err != nil || !revised {
		t.Fatalf("expected a revision: revised=%v err=%v", revised, err)
	}
	if version != 2 {
		t.Fatalf("proposed version = %d, want 2", version)
	}
	prop, ok := db.profiles[2]
	if !ok {
		t.Fatal("v2 not inserted")
	}
	if prop.Status != profileProposed {
		t.Errorf("revision must be proposed, got %q", prop.Status)
	}
	if prop.Narrative != "Reader likes ai and kubernetes." || narrator.calls != 1 {
		t.Errorf("narrative not taken from narrator: %q (calls=%d)", prop.Narrative, narrator.calls)
	}
	if !contains(parseStringArray(prop.Topics), "kubernetes") {
		t.Errorf("proposed topics should include kubernetes: %s", prop.Topics)
	}
	// The active profile is unchanged — a proposal is inert until approved.
	if act, _, _ := db.GetActiveInterestProfile(ctx); act.Version != 1 {
		t.Errorf("active should still be v1, got v%d", act.Version)
	}
}

func TestReviseProfileNarratorFallback(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedActiveProfile(t, db, t0, `["ai"]`)
	if err := db.InsertFeedback(ctx, Feedback{TargetType: targetItem, TargetRef: "1", Signal: signalUp, Source: sourceQuarantineReview, CreatedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// A nil narrator -> the proposal carries the deterministic template narrative.
	version, revised, err := ReviseProfile(ctx, db, fakeResolver{}, nil, t0.Add(8*24*time.Hour), testReviseConfig())
	if err != nil || !revised {
		t.Fatalf("expected a revision: %v %v", revised, err)
	}
	if db.profiles[version].Narrative == "" {
		t.Error("a nil narrator should still yield a template narrative")
	}
}

func TestReviseProfileDebouncedByPendingProposal(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedActiveProfile(t, db, t0, `["ai"]`)
	// A proposal already awaits approval.
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2, Status: profileProposed, CreatedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertFeedback(ctx, Feedback{TargetType: targetItem, TargetRef: "1", Signal: signalUp, Source: sourceQuarantineReview, CreatedAt: t0.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, revised, err := ReviseProfile(ctx, db, fakeResolver{}, nil, t0.Add(30*24*time.Hour), testReviseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if revised {
		t.Error("must not stack a second proposal while one is pending")
	}
}

func TestReviseProfileNoActiveBase(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase() // no profile at all
	_, revised, err := ReviseProfile(ctx, db, fakeResolver{}, nil, time.Now(), testReviseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if revised {
		t.Error("with no active base there is nothing to revise")
	}
}

// versionTakenDB wraps the mock but fails every InsertInterestProfile with errVersionExists,
// modelling a race: a concurrent proposal (a human surface add, or an overlapping run) claimed the
// computed version number between the reviser's read and its insert.
type versionTakenDB struct{ *MockDatabase }

func (v versionTakenDB) InsertInterestProfile(_ context.Context, _ InterestProfile) error {
	return errVersionExists
}

// TestReviseProfileVersionTakenSkips covers the shared proposal-version namespace: a taken version
// surfaces as the typed errVersionExists (not a raw duplicate-key error), and ReviseProfile reports
// revised=false so the caller (main) can skip the run gracefully instead of dying.
func TestReviseProfileVersionTakenSkips(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedActiveProfile(t, db, t0, `["ai"]`)
	if err := db.InsertFeedback(ctx, Feedback{TargetType: targetItem, TargetRef: "1", Signal: signalUp, Source: sourceQuarantineReview, CreatedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, revised, err := ReviseProfile(ctx, versionTakenDB{db}, fakeResolver{}, nil, t0.Add(8*24*time.Hour), testReviseConfig())
	if !errors.Is(err, errVersionExists) {
		t.Fatalf("a taken version should surface errVersionExists, got revised=%v err=%v", revised, err)
	}
	if revised {
		t.Error("a collided version must not report a successful revision")
	}
}

// ---------------------------------------------------------------------------
// Append-only / one-active invariants at the hone seam (the half of the contract hone owns:
// it APPENDS a proposed version; activation stays in rara-core's surface).
// ---------------------------------------------------------------------------

func TestInsertInterestProfileVersionImmutable(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1, Status: profileActive}); err != nil {
		t.Fatal(err)
	}
	// Re-inserting the same version yields the typed errVersionExists (ON CONFLICT DO NOTHING →
	// 0 rows) — a benign "already taken", not a hard failure.
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1, Status: profileProposed}); !errors.Is(err, errVersionExists) {
		t.Fatalf("a duplicate version should yield errVersionExists, got %v", err)
	}
	// A second active row is rejected (mirrors the partial unique index); a proposed one is fine.
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2, Status: profileActive}); err == nil {
		t.Error("a second active interest_profile should be rejected")
	}
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2, Status: profileProposed}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// configFromEnv — env overrides + --force collapse.
// ---------------------------------------------------------------------------

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("REVISE_CADENCE_HOURS", "48")
	t.Setenv("REVISE_FEEDBACK_THRESHOLD", "7")
	t.Setenv("REVISE_DEBOUNCE_HOURS", "2")
	cfg := configFromEnv(false)
	if cfg.Cadence != 48*time.Hour || cfg.FeedbackThreshold != 7 || cfg.Debounce != 2*time.Hour {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
	// --force collapses the trigger gate (but the engine still no-ops with no new feedback).
	forced := configFromEnv(true)
	if forced.Cadence != 0 || forced.Debounce != 0 || forced.FeedbackThreshold != 1 {
		t.Errorf("--force should collapse cadence/debounce/threshold: %+v", forced)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
