package main

import (
	"context"
	"sort"
	"testing"
)

// mockDispatchDB is the in-memory fake for DispatchOnce tests.
type mockDispatchDB struct {
	steps     []AssignedStep
	providers map[string]DispatchProvider
	listErr   error
	getErr    error
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

// --- Dispatcher.DispatchOnce -------------------------------------------------

func TestDispatchOnceWakesAssignedProviders(t *testing.T) {
	db := &mockDispatchDB{
		steps: []AssignedStep{
			{ItemID: 1, Seq: 2, Capability: "gate_barato", AssignedProvider: "gate-barato"},
		},
		providers: map[string]DispatchProvider{
			"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun},
		},
	}
	runner := &fakeTransport{}
	d := Dispatcher{db: db, runner: runner}

	if err := d.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(runner.called) != 1 {
		t.Errorf("runner called %d times, want 1", len(runner.called))
	}
	if runner.called[0].App != "gate-barato" {
		t.Errorf("app = %q, want gate-barato", runner.called[0].App)
	}
}

func TestDispatchOnceCoalescesPerProvider(t *testing.T) {
	db := &mockDispatchDB{
		steps: []AssignedStep{
			{ItemID: 1, Seq: 2, Capability: "gate_barato", AssignedProvider: "gate-barato"},
			{ItemID: 2, Seq: 2, Capability: "gate_barato", AssignedProvider: "gate-barato"},
		},
		providers: map[string]DispatchProvider{
			"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun},
		},
	}
	runner := &fakeTransport{}
	d := Dispatcher{db: db, runner: runner}

	if err := d.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(runner.called) != 1 {
		t.Errorf("gate-barato called %d times, want 1 (coalesced per pass)", len(runner.called))
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
	runner := &fakeTransport{}
	d := Dispatcher{db: db, runner: runner}

	if err := d.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(runner.called) != 2 {
		t.Errorf("runner called %d times, want 2", len(runner.called))
	}
	woken := make([]string, len(runner.called))
	for i, r := range runner.called {
		woken[i] = r.App
	}
	sort.Strings(woken)
	if woken[0] != "asr-youtube" || woken[1] != "gate-barato" {
		t.Errorf("woken = %v, want [asr-youtube gate-barato]", woken)
	}
}

func TestDispatchOnceSkipsUnknownProvider(t *testing.T) {
	db := &mockDispatchDB{
		steps: []AssignedStep{
			{ItemID: 1, Seq: 2, AssignedProvider: "ghost-provider"},
		},
		providers: map[string]DispatchProvider{}, // not registered
	}
	runner := &fakeTransport{}
	d := Dispatcher{db: db, runner: runner}

	if err := d.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(runner.called) != 0 {
		t.Errorf("runner called %d times, want 0 for unknown provider", len(runner.called))
	}
}

func TestDispatchOnceNoSteps(t *testing.T) {
	db := &mockDispatchDB{steps: nil}
	runner := &fakeTransport{}
	d := Dispatcher{db: db, runner: runner}

	if err := d.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(runner.called) != 0 {
		t.Errorf("runner called %d times, want 0 when no steps assigned", len(runner.called))
	}
}

func TestDispatchOnceRunnerErrorIsLogged(t *testing.T) {
	// A runner error is best-effort: DispatchOnce returns nil (logging the error) so one failed
	// wake doesn't prevent the pass from completing cleanly.
	db := &mockDispatchDB{
		steps: []AssignedStep{
			{ItemID: 1, Seq: 2, AssignedProvider: "bad-provider"},
		},
		providers: map[string]DispatchProvider{
			"bad-provider": {Name: "bad-provider", Runtime: runtimeCloudRun},
		},
	}
	runner := &fakeTransport{err: errBoom{}}
	d := Dispatcher{db: db, runner: runner}

	if err := d.DispatchOnce(context.Background()); err != nil {
		t.Errorf("DispatchOnce must swallow runner errors, got %v", err)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
