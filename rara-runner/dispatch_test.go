package main

import (
	"context"
	"sort"
	"testing"
)

// mockDispatchDB is the in-memory fake for DispatchOnce tests.
type mockDispatchDB struct {
	steps         []AssignedStep
	providers     map[string]DispatchProvider
	dueCollectors []DispatchProvider
	touched       map[string]int // name -> call count; detects double-touch bugs
	listErr       error
	getErr        error
	collectErr    error
	touchErr      error
}

func (m *mockDispatchDB) ListAssignedSteps(_ context.Context) ([]AssignedStep, error) {
	return m.steps, m.listErr
}

func (m *mockDispatchDB) GetProvider(_ context.Context, name string) (DispatchProvider, bool, error) {
	if m.getErr != nil {
		return DispatchProvider{}, false, m.getErr
	}
	p, ok := m.providers[name]
	return p, ok, nil
}

func (m *mockDispatchDB) ListDueCollectors(_ context.Context) ([]DispatchProvider, error) {
	return m.dueCollectors, m.collectErr
}

func (m *mockDispatchDB) TouchCollectorDispatched(_ context.Context, name string) error {
	if m.touched == nil {
		m.touched = make(map[string]int)
	}
	m.touched[name]++
	return m.touchErr
}

// runOnce is a test helper that wires up a Dispatcher and calls DispatchOnce once.
func runOnce(t *testing.T, db DispatchDB) *fakeTransport {
	t.Helper()
	tr := &fakeTransport{}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	return tr
}

// --- Dispatcher.DispatchOnce -------------------------------------------------

func TestDispatchOnceWakesAssignedProviders(t *testing.T) {
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, Capability: "gate_barato", AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 1 {
		t.Errorf("runner called %d times, want 1", len(tr.called))
	}
	if tr.called[0].App != "gate-barato" {
		t.Errorf("app = %q, want gate-barato", tr.called[0].App)
	}
}

func TestDispatchOncePassesProviderEnv(t *testing.T) {
	// The dispatcher must copy the provider's per-run Env into the RunRequest so the transport
	// (Cloud Run overrides / docker -e) injects it. Without this, host providers wake with no config.
	env := map[string]string{"DISTILL_RECIPE": "opus", "LITELLM_MODEL": "gemini-flash"}
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun, Env: env}},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(tr.called))
	}
	got := tr.called[0].Env
	if len(got) != 2 || got["DISTILL_RECIPE"] != "opus" || got["LITELLM_MODEL"] != "gemini-flash" {
		t.Errorf("req.Env = %v, want %v", got, env)
	}
	// RunRequest must own a copy: mutating it must not bleed back into the provider's map.
	got["DISTILL_RECIPE"] = "mutated"
	if env["DISTILL_RECIPE"] != "opus" {
		t.Errorf("mutating req.Env changed provider Env: %v", env)
	}
}

func TestDispatchOnceEnvWithSpecialChars(t *testing.T) {
	// Dispatcher must copy Env values verbatim — no escaping, quoting, or stripping.
	// The transport layer (Cloud Run / docker -e) owns injection safety; the Dispatcher
	// must not mangle the values it receives.
	env := map[string]string{
		"QUOTED":   `va"lue`,
		"NEWLINE":  "val\nue",
		"METACHAR": "val$ue;|`",
	}
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun, Env: env}},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(tr.called))
	}
	got := tr.called[0].Env
	for k, want := range env {
		if got[k] != want {
			t.Errorf("Env[%q] = %q, want %q", k, got[k], want)
		}
	}
}

func TestDispatchOnceEmptyEnvStillWakes(t *testing.T) {
	// Both nil Env and explicit empty map must wake normally — no panic, no env injected.
	for _, provEnv := range []map[string]string{nil, {}} {
		db := &mockDispatchDB{
			steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
			providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun, Env: provEnv}},
		}
		tr := runOnce(t, db)
		if len(tr.called) != 1 {
			t.Fatalf("runner called %d times, want 1 (provEnv=%v)", len(tr.called), provEnv)
		}
		if len(tr.called[0].Env) != 0 {
			t.Errorf("req.Env = %v, want empty (provEnv=%v)", tr.called[0].Env, provEnv)
		}
	}
}

func TestDispatchOnceCoalescesPerProvider(t *testing.T) {
	db := &mockDispatchDB{
		steps: []AssignedStep{
			{ItemID: 1, Seq: 2, Capability: "gate_barato", AssignedProvider: "gate-barato"},
			{ItemID: 2, Seq: 2, Capability: "gate_barato", AssignedProvider: "gate-barato"},
		},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 1 {
		t.Errorf("gate-barato called %d times, want 1 (coalesced per pass)", len(tr.called))
	}
}

func TestDispatchOnceMultipleProviders(t *testing.T) {
	db := &mockDispatchDB{
		steps: []AssignedStep{
			{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"},
			{ItemID: 1, Seq: 3, AssignedProvider: "asr-youtube"},
		},
		providers: map[string]DispatchProvider{
			"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun},
			"asr-youtube": {Name: "asr-youtube", Runtime: runtimeLocal, RunnerURL: "http://mac.tailnet:8473"},
		},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 2 {
		t.Errorf("runner called %d times, want 2", len(tr.called))
	}
	woken := make([]string, len(tr.called))
	for i, r := range tr.called {
		woken[i] = r.App
	}
	sort.Strings(woken)
	if woken[0] != "asr-youtube" || woken[1] != "gate-barato" {
		t.Errorf("woken = %v, want [asr-youtube gate-barato]", woken)
	}
}

func TestDispatchOnceSkipsUnknownProvider(t *testing.T) {
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "ghost-provider"}},
		providers: map[string]DispatchProvider{},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 0 {
		t.Errorf("runner called %d times, want 0 for unknown provider", len(tr.called))
	}
}

func TestDispatchOnceNoSteps(t *testing.T) {
	tr := runOnce(t, &mockDispatchDB{steps: nil})
	if len(tr.called) != 0 {
		t.Errorf("runner called %d times, want 0 when no steps assigned", len(tr.called))
	}
}

func TestDispatchOnceRunnerErrorIsLogged(t *testing.T) {
	// A runner error is best-effort: DispatchOnce returns nil (logging the error) so one failed
	// wake doesn't prevent the pass from completing cleanly.
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "bad-provider"}},
		providers: map[string]DispatchProvider{"bad-provider": {Name: "bad-provider", Runtime: runtimeCloudRun}},
	}
	tr := &fakeTransport{err: errBoom{}}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Errorf("DispatchOnce must swallow runner errors, got %v", err)
	}
}

func TestDispatchOnceDBErrorPropagates(t *testing.T) {
	// A DB error on ListAssignedSteps is fatal for the pass — returned, not swallowed.
	db := &mockDispatchDB{listErr: errBoom{}}
	if err := (&Dispatcher{db: db, runner: &fakeTransport{}}).DispatchOnce(context.Background()); err == nil {
		t.Error("want error when db.ListAssignedSteps fails, got nil")
	}
}

// --- Collector dispatch (ListDueCollectors + TouchCollectorDispatched) ----------

func TestDispatchOnceDueCollectorIsWoken(t *testing.T) {
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "dial", Runtime: runtimeCloudRun}},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(tr.called))
	}
	if tr.called[0].App != "dial" {
		t.Errorf("app = %q, want dial", tr.called[0].App)
	}
}

func TestDispatchOnceTouchesCollectorAfterWake(t *testing.T) {
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "harvest", Runtime: runtimeCloudRun}},
	}
	runOnce(t, db)
	if db.touched["harvest"] != 1 {
		t.Errorf("touched[harvest] = %d, want 1; full map: %v", db.touched["harvest"], db.touched)
	}
}

func TestDispatchOnceCollectorNotTouchedOnRunnerError(t *testing.T) {
	// A runner error (e.g. Cloud Run API down) must NOT stamp last_collect_at — the collector
	// was never actually woken, so the next dispatch pass should retry it.
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "dial", Runtime: runtimeCloudRun}},
	}
	tr := &fakeTransport{err: errBoom{}}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(db.touched) != 0 {
		t.Errorf("touched after runner error: %v, want empty map", db.touched)
	}
}

func TestDispatchOnceCollectorsAndWorkersInSamePass(t *testing.T) {
	// Collectors and assigned workers are both dispatched in the same DispatchOnce pass.
	db := &mockDispatchDB{
		steps:         []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers:     map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
		dueCollectors: []DispatchProvider{{Name: "harvest", Runtime: runtimeCloudRun}},
	}
	tr := runOnce(t, db)
	if len(tr.called) != 2 {
		t.Errorf("runner called %d times, want 2 (1 worker + 1 collector)", len(tr.called))
	}
	woken := make([]string, len(tr.called))
	for i, r := range tr.called {
		woken[i] = r.App
	}
	sort.Strings(woken)
	if woken[0] != "gate-barato" || woken[1] != "harvest" {
		t.Errorf("woken = %v, want [gate-barato harvest]", woken)
	}
}

func TestDispatchOnceCollectorListErrorPropagates(t *testing.T) {
	db := &mockDispatchDB{collectErr: errBoom{}}
	if err := (&Dispatcher{db: db, runner: &fakeTransport{}}).DispatchOnce(context.Background()); err == nil {
		t.Error("want error when db.ListDueCollectors fails, got nil")
	}
}

func TestDispatchOnceNoDueCollectors(t *testing.T) {
	db := &mockDispatchDB{dueCollectors: nil}
	tr := runOnce(t, db)
	if len(tr.called) != 0 {
		t.Errorf("runner called %d times, want 0 when no collectors due", len(tr.called))
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
