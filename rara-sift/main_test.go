package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	addon "rara-addon"
)

// TestTruncateOnRune: the gate_rico transcript cap must never split a multi-byte UTF-8 rune
// (pt/en transcripts carry accents), so the result is always valid UTF-8 and at most max bytes.
func TestTruncateOnRune(t *testing.T) {
	if got := truncateOnRune("hello", 100); got != "hello" {
		t.Errorf("under cap should pass through, got %q", got)
	}
	s := strings.Repeat("ação ", 50) // lots of multi-byte runes
	for max := 1; max < len(s); max++ {
		got := truncateOnRune(s, max)
		if len(got) > max {
			t.Fatalf("max=%d: result %d bytes exceeds cap", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d: result is not valid UTF-8: %q", max, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("max=%d: result is not a prefix of the input", max)
		}
	}
}

// ---------------------------------------------------------------------------
// The pure cascade (rules -> profile -> LLM-judge). These tests need no DB: the
// cascade is a pure function over a profileDoc + gateInput + an LLMJudge seam.
// ---------------------------------------------------------------------------

// fakeJudge is the LLM-judge seam stubbed: it records whether it was consulted (so tests can
// assert the cheaper layers short-circuit it) and returns a canned verdict.
type fakeJudge struct {
	called  int
	verdict GateVerdict
	err     error
}

func (f *fakeJudge) Judge(_ context.Context, _ string, _ gateInput, _ profileDoc) (GateVerdict, error) {
	f.called++
	return f.verdict, f.err
}

// profileWith builds a profileDoc directly (the cascade is pure — no DB needed).
func profileWith(topics, authors, anti []string, rules ...GateRule) profileDoc {
	return profileDoc{Topics: topics, Authors: authors, AntiTopics: anti, KeepThreshold: defaultKeepThreshold, Rules: rules}
}

func allow(mt, v string) GateRule {
	return GateRule{Action: ruleAllow, MatchType: mt, Value: v, Enabled: true}
}
func deny(mt, v string) GateRule {
	return GateRule{Action: ruleDeny, MatchType: mt, Value: v, Enabled: true}
}

// TestCascadeRulesDecideWithoutLLM: a matching rule (allow or deny) decides at the cheapest
// layer — the profile and the LLM are never consulted.
func TestCascadeRulesDecideWithoutLLM(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		rule    GateRule
		in      gateInput
		wantDec string
	}{
		{"deny by channel drops", deny(matchChannel, "ClickbaitTV"),
			gateInput{Title: "anything", Channel: "ClickbaitTV"}, decisionDrop},
		{"allow by title keeps", allow(matchTitleContains, "transformer"),
			gateInput{Title: "The Transformer architecture", Channel: "Some Channel"}, decisionKeep},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			judge := &fakeJudge{}
			prof := profileWith(nil, nil, nil, c.rule)
			v, err := runCascade(ctx, capGateBarato, c.in, prof, judge)
			if err != nil {
				t.Fatal(err)
			}
			if v.Decision != c.wantDec || v.DecidedBy != decidedByRules {
				t.Errorf("verdict = %+v, want %s by rules", v, c.wantDec)
			}
			if v.Score != nil {
				t.Errorf("rules layer must not carry a score, got %v", *v.Score)
			}
			if judge.called != 0 {
				t.Errorf("LLM consulted %d times — rules must short-circuit it", judge.called)
			}
		})
	}
}

// TestCascadeDenyPrecedence: when both an allow and a deny rule match, deny wins — regardless
// of rule order (an explicit deny always drops).
func TestCascadeDenyPrecedence(t *testing.T) {
	ctx := context.Background()
	in := gateInput{Title: "transformer talk", Channel: "ClickbaitTV"}
	for _, rules := range [][]GateRule{
		{allow(matchTitleContains, "transformer"), deny(matchChannel, "ClickbaitTV")},
		{deny(matchChannel, "ClickbaitTV"), allow(matchTitleContains, "transformer")},
	} {
		prof := profileWith(nil, nil, nil, rules...)
		v, err := runCascade(ctx, capGateBarato, in, prof, &fakeJudge{})
		if err != nil {
			t.Fatal(err)
		}
		if v.Decision != decisionDrop || v.DecidedBy != decidedByRules {
			t.Errorf("deny precedence failed: verdict = %+v, want drop by rules", v)
		}
	}
}

// TestCascadeProfileKeepsStrongMatch: no rule fires, but the profile matches strongly
// (>= keep threshold) -> keep at the profile layer, with a score, without the LLM.
func TestCascadeProfileKeepsStrongMatch(t *testing.T) {
	ctx := context.Background()
	judge := &fakeJudge{}
	prof := profileWith([]string{"platform engineering", "kubernetes"}, nil, nil)
	in := gateInput{Title: "Platform engineering with Kubernetes at scale", Channel: "DevOps Daily"}
	v, err := runCascade(ctx, capGateBarato, in, prof, judge)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != decisionKeep || v.DecidedBy != decidedByProfile {
		t.Errorf("verdict = %+v, want keep by profile", v)
	}
	if v.Score == nil || *v.Score < prof.KeepThreshold {
		t.Errorf("profile keep must carry a score >= threshold, got %v", v.Score)
	}
	if judge.called != 0 {
		t.Errorf("strong profile match must short-circuit the LLM, called %d", judge.called)
	}
}

// TestCascadeEscalatesBorderlineToLLM: no rule, weak profile signal -> the cascade defers to
// the LLM-judge, and returns exactly its verdict (keep/drop/defer all pass through).
func TestCascadeEscalatesBorderlineToLLM(t *testing.T) {
	ctx := context.Background()
	prof := profileWith([]string{"kubernetes"}, nil, nil) // single weak topic -> net 1 -> 0.5 < 0.6
	in := gateInput{Title: "A vague talk that barely mentions kubernetes once", Channel: "Unknown"}

	for _, want := range []string{decisionKeep, decisionDrop, decisionDefer} {
		score := 0.42
		judge := &fakeJudge{verdict: GateVerdict{Decision: want, Score: &score, DecidedBy: decidedByLLM, Reason: "borderline"}}
		v, err := runCascade(ctx, capGateBarato, in, prof, judge)
		if err != nil {
			t.Fatal(err)
		}
		if judge.called != 1 {
			t.Errorf("borderline input must consult the LLM exactly once, got %d", judge.called)
		}
		if v.Decision != want || v.DecidedBy != decidedByLLM {
			t.Errorf("verdict = %+v, want %s by llm", v, want)
		}
	}
}

// TestCascadeAntiTopicEscalates: an anti-topic cancels a topic hit (net <= 0 -> score 0), so a
// would-be keep escalates to the LLM instead of being auto-kept by the profile.
func TestCascadeAntiTopicEscalates(t *testing.T) {
	ctx := context.Background()
	judge := &fakeJudge{verdict: GateVerdict{Decision: decisionDefer, DecidedBy: decidedByLLM}}
	prof := profileWith([]string{"kubernetes"}, nil, []string{"crypto"})
	in := gateInput{Title: "kubernetes for crypto mining", Channel: "x"} // 1 topic - 1 anti = 0
	v, err := runCascade(ctx, capGateBarato, in, prof, judge)
	if err != nil {
		t.Fatal(err)
	}
	if judge.called != 1 {
		t.Errorf("anti-topic-cancelled match must escalate to the LLM, called %d", judge.called)
	}
	if v.DecidedBy != decidedByLLM {
		t.Errorf("verdict = %+v, want llm", v)
	}
}

// TestProfileMatchScore checks the pure scoring curve in isolation.
func TestProfileMatchScore(t *testing.T) {
	prof := profileWith([]string{"alpha", "bravo", "charlie"}, nil, []string{"zulu"})
	cases := []struct {
		name string
		in   gateInput
		want float64
	}{
		{"no hit", gateInput{Title: "nothing here"}, 0},
		{"one hit -> 0.5 (escalate)", gateInput{Title: "only alpha"}, 0.5},
		{"two hits -> ~0.667 (keep)", gateInput{Title: "alpha and bravo"}, 1 - 1.0/3},
		{"anti cancels one", gateInput{Title: "alpha and bravo but zulu"}, 0.5},
	}
	for _, c := range cases {
		if got := profileMatch(c.in, prof); got-c.want > 1e-9 || c.want-got > 1e-9 {
			t.Errorf("%s: profileMatch = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestProfileMatchWordBoundary: a short topic must match as a WORD, not a substring.
func TestProfileMatchWordBoundary(t *testing.T) {
	prof := profileWith([]string{"ai"}, nil, nil)
	keeps := []string{"The new AI model", "ai: a primer", "talk about ai.", "(ai)"}
	for _, title := range keeps {
		if profileMatch(gateInput{Title: title}, prof) == 0 {
			t.Errorf("%q should match topic 'ai' as a word", title)
		}
	}
	noMatch := []string{"rain in spain", "now available", "how to maintain", "he said so", "container ships"}
	for _, title := range noMatch {
		if got := profileMatch(gateInput{Title: title}, prof); got != 0 {
			t.Errorf("%q must NOT match topic 'ai' (substring trap), got %v", title, got)
		}
	}
	phr := profileWith([]string{"platform engineering"}, nil, nil)
	if profileMatch(gateInput{Title: "notes on platform engineering at scale"}, phr) == 0 {
		t.Error("multi-word topic should still match")
	}
}

// TestContainsWord exercises the boundary helper directly.
func TestContainsWord(t *testing.T) {
	cases := []struct {
		hay, tok string
		want     bool
	}{
		{"the ai model", "ai", true},
		{"rain", "ai", false},
		{"available now", "ai", false},
		{"ai", "ai", true},
		{"llm and ai", "ai", true},
		{"aibo robot", "ai", false},
		{"", "ai", false},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := containsWord(c.hay, c.tok); got != c.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", c.hay, c.tok, got, c.want)
		}
	}
}

// TestParseProfileWeights honors a valid keep_threshold override and falls back otherwise.
func TestParseProfileWeights(t *testing.T) {
	base := InterestProfile{Version: 1, Topics: json.RawMessage(`["x"]`)}
	if d := parseProfile(base, nil); d.KeepThreshold != defaultKeepThreshold {
		t.Errorf("no weights -> default threshold, got %v", d.KeepThreshold)
	}
	base.Weights = json.RawMessage(`{"keep_threshold": 0.8}`)
	if d := parseProfile(base, nil); d.KeepThreshold != 0.8 {
		t.Errorf("valid override -> 0.8, got %v", d.KeepThreshold)
	}
	base.Weights = json.RawMessage(`{"keep_threshold": 5}`) // out of (0,1] -> ignored
	if d := parseProfile(base, nil); d.KeepThreshold != defaultKeepThreshold {
		t.Errorf("out-of-range override -> default, got %v", d.KeepThreshold)
	}
	if d := parseProfile(base, nil); len(d.Topics) != 1 || d.Topics[0] != "x" {
		t.Errorf("topics not parsed: %+v", d.Topics)
	}
}

// TestParseJudgeVerdict: the LLM response parsing fails SAFE — an unknown decision becomes
// defer (quarantine), an out-of-range score is dropped, and non-JSON is an error.
func TestParseJudgeVerdict(t *testing.T) {
	v, err := parseJudgeVerdict(`{"decision":"keep","score":0.9,"reason":"on topic"}`)
	if err != nil || v.Decision != decisionKeep || v.Score == nil || *v.Score != 0.9 || v.DecidedBy != decidedByLLM {
		t.Errorf("valid keep parse = %+v, err=%v", v, err)
	}
	if v, _ := parseJudgeVerdict(`{"decision":"banana"}`); v.Decision != decisionDefer {
		t.Errorf("unknown decision must fail safe to defer, got %q", v.Decision)
	}
	if v, _ := parseJudgeVerdict(`{"decision":"keep","score":7}`); v.Score != nil {
		t.Errorf("out-of-range score must be dropped, got %v", *v.Score)
	}
	if _, err := parseJudgeVerdict(`not json`); err == nil {
		t.Error("non-JSON content must error")
	}
}

// ---------------------------------------------------------------------------
// The handler — the domain glue behind addon.Run: load profile, read the item's
// metadata/text, run the cascade, WRITE the gate_decision, report its id. The
// reconciler (rara-core) routes keep/drop/defer from the row this writes.
// ---------------------------------------------------------------------------

// mockStore is the SiftStore stubbed: canned profile + input + a recorded decisions log.
type mockStore struct {
	prof      profileDoc
	profErr   error
	in        gateInput
	ready     bool
	inErr     error
	insertErr error

	decisions []GateDecision
	nextID    int
}

func (m *mockStore) LoadProfile(context.Context) (profileDoc, error) { return m.prof, m.profErr }
func (m *mockStore) ReadInput(context.Context, string, addon.Item) (gateInput, bool, error) {
	return m.in, m.ready, m.inErr
}
func (m *mockStore) InsertGateDecision(_ context.Context, d GateDecision) (int, error) {
	if m.insertErr != nil {
		return 0, m.insertErr
	}
	m.nextID++
	m.decisions = append(m.decisions, d)
	return m.nextID, nil
}

func borderlineStore() *mockStore {
	// Empty profile + no rules + neutral input -> the cascade escalates to the LLM, so the
	// fakeJudge's verdict is what the handler records (lets a test force keep/drop/defer).
	return &mockStore{prof: profileWith(nil, nil, nil), in: gateInput{Title: "neutral"}, ready: true, nextID: 0}
}

// TestSiftHandlerRecordsDecision: for each cascade verdict (keep/drop/defer) the handler appends
// a gate_decisions row with the decision + score + decided_by + reason, gated as configured, and
// returns the row id as the step's OutputRef. It must NOT touch item status (the reconciler routes).
func TestSiftHandlerRecordsDecision(t *testing.T) {
	for _, dec := range []string{decisionKeep, decisionDrop, decisionDefer} {
		t.Run(dec, func(t *testing.T) {
			ctx := context.Background()
			store := borderlineStore()
			score := 0.71
			judge := &fakeJudge{verdict: GateVerdict{Decision: dec, Score: &score, DecidedBy: decidedByLLM, Reason: "verdict"}}

			res, err := siftHandler(store, capGateBarato, judge)(ctx, addon.Item{ID: 42}, addon.Step{Seq: 2})
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if len(store.decisions) != 1 {
				t.Fatalf("expected 1 gate_decision, got %d", len(store.decisions))
			}
			d := store.decisions[0]
			if d.ItemID != 42 || d.Gate != capGateBarato || d.Decision != dec ||
				d.DecidedBy != decidedByLLM || d.Reason != "verdict" || d.Score == nil || *d.Score != 0.71 {
				t.Errorf("gate_decision = %+v, want item=42 %s by llm score 0.71", d, dec)
			}
			if res.OutputRef != "1" {
				t.Errorf("OutputRef = %q, want the gate_decision id %q", res.OutputRef, "1")
			}
			if res.Filtered {
				t.Error("a gate decision must not curate the item out; the reconciler routes")
			}
		})
	}
}

// TestSiftHandlerInputNotReadyRetryable: gate_rico before the to-text artifact lands -> the
// handler asks the SDK to requeue (ErrRetryable), not fail the item for good.
func TestSiftHandlerInputNotReadyRetryable(t *testing.T) {
	store := borderlineStore()
	store.ready = false // input not produced yet
	_, err := siftHandler(store, capGateRico, &fakeJudge{})(context.Background(), addon.Item{ID: 1}, addon.Step{Seq: 4})
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("input-not-ready should be retryable, got %v", err)
	}
	if len(store.decisions) != 0 {
		t.Error("no decision should be recorded when the input is not ready")
	}
}

// TestSiftHandlerJudgeErrorRetryable: a gateway blip on the borderline LLM call is transient — a
// good item must not be permanently failed by it, so the handler requeues.
func TestSiftHandlerJudgeErrorRetryable(t *testing.T) {
	store := borderlineStore()
	judge := &fakeJudge{err: errors.New("gateway 502")}
	_, err := siftHandler(store, capGateBarato, judge)(context.Background(), addon.Item{ID: 1}, addon.Step{Seq: 2})
	if !errors.Is(err, addon.ErrRetryable) {
		t.Errorf("judge error should be retryable, got %v", err)
	}
	if len(store.decisions) != 0 {
		t.Error("no decision should be recorded when the judge errored")
	}
}

// TestSiftHandlerProfileLoadErrorFails: a profile/rules read error is NOT a retryable miss — it is
// a real failure surfaced as-is (no decision written).
func TestSiftHandlerProfileLoadErrorFails(t *testing.T) {
	store := borderlineStore()
	store.profErr = errors.New("db down")
	_, err := siftHandler(store, capGateBarato, &fakeJudge{})(context.Background(), addon.Item{ID: 1}, addon.Step{Seq: 2})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, addon.ErrRetryable) {
		t.Errorf("a profile load error must not be retryable, got %v", err)
	}
}
