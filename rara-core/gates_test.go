package main

import (
	"context"
	"encoding/json"
	"testing"
)

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
			v, err := runCascade(ctx, gateBarato, c.in, prof, judge)
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
	// allow listed first, deny second — and vice-versa: both must drop.
	for _, rules := range [][]GateRule{
		{allow(matchTitleContains, "transformer"), deny(matchChannel, "ClickbaitTV")},
		{deny(matchChannel, "ClickbaitTV"), allow(matchTitleContains, "transformer")},
	} {
		prof := profileWith(nil, nil, nil, rules...)
		v, err := runCascade(ctx, gateBarato, in, prof, &fakeJudge{})
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
	// Two topic hits -> net 2 -> score 0.67 >= 0.6 keep threshold.
	prof := profileWith([]string{"platform engineering", "kubernetes"}, nil, nil)
	in := gateInput{Title: "Platform engineering with Kubernetes at scale", Channel: "DevOps Daily"}
	v, err := runCascade(ctx, gateBarato, in, prof, judge)
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
	prof := profileWith([]string{"kubernetes"}, nil, nil) // a single weak topic -> net 1 -> 0.5 < 0.6
	in := gateInput{Title: "A vague talk that barely mentions kubernetes once", Channel: "Unknown"}

	for _, want := range []string{decisionKeep, decisionDrop, decisionDefer} {
		score := 0.42
		judge := &fakeJudge{verdict: GateVerdict{Decision: want, Score: &score, DecidedBy: decidedByLLM, Reason: "borderline"}}
		v, err := runCascade(ctx, gateBarato, in, prof, judge)
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
	v, err := runCascade(ctx, gateBarato, in, prof, judge)
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

// TestProfileMatchScore checks the pure scoring curve in isolation. Tokens are distinct
// multi-char words so substring matching does not collide with incidental letters.
func TestProfileMatchScore(t *testing.T) {
	prof := profileWith([]string{"alpha", "bravo", "charlie"}, nil, []string{"zulu"})
	cases := []struct {
		name string
		in   gateInput
		want float64 // 0 means "below keep threshold (escalate)"
	}{
		{"no hit", gateInput{Title: "nothing here"}, 0},
		{"one hit -> 0.5 (escalate)", gateInput{Title: "only alpha"}, 0.5},
		{"two hits -> ~0.667 (keep)", gateInput{Title: "alpha and bravo"}, 1 - 1.0/3},
		{"anti cancels one", gateInput{Title: "alpha and bravo but zulu"}, 0.5}, // net 2-1=1
	}
	for _, c := range cases {
		if got := profileMatch(c.in, prof); got-c.want > 1e-9 || c.want-got > 1e-9 {
			t.Errorf("%s: profileMatch = %v, want %v", c.name, got, c.want)
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
