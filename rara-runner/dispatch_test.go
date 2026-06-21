package main

import (
	"context"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// mockDispatchDB is the in-memory fake for DispatchOnce tests.
type mockDispatchDB struct {
	steps         []AssignedStep
	providers     map[string]DispatchProvider
	dueCollectors []DispatchProvider
	attempted     map[string]int    // name -> TouchCollectorAttempted call count
	stampedErrors map[string]string // name -> last StampDispatchError msg
	clearedErrors map[string]int    // name -> ClearDispatchError call count
	listErr       error
	getErr        error
	collectErr    error
	attemptErr    error
	stampErr      error
	clearErr      error
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

func (m *mockDispatchDB) TouchCollectorAttempted(_ context.Context, name string) error {
	if m.attempted == nil {
		m.attempted = make(map[string]int)
	}
	m.attempted[name]++
	return m.attemptErr
}

func (m *mockDispatchDB) StampDispatchError(_ context.Context, name, msg string) error {
	if m.stampedErrors == nil {
		m.stampedErrors = make(map[string]string)
	}
	m.stampedErrors[name] = msg
	return m.stampErr
}

func (m *mockDispatchDB) ClearDispatchError(_ context.Context, name string) error {
	if m.clearedErrors == nil {
		m.clearedErrors = make(map[string]int)
	}
	m.clearedErrors[name]++
	return m.clearErr
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

func TestDispatchOnceTouchesAttemptAfterWake(t *testing.T) {
	// The dispatcher stamps last_attempt_at on every wake attempt (success or failure).
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "harvest", Runtime: runtimeCloudRun}},
	}
	runOnce(t, db)
	if db.attempted["harvest"] != 1 {
		t.Errorf("attempted[harvest] = %d, want 1; full map: %v", db.attempted["harvest"], db.attempted)
	}
}

func TestDispatchOnceAttemptStampedEvenOnRunnerError(t *testing.T) {
	// last_attempt_at is stamped even when runner.Run fails (Cloud Run API down, etc.).
	// This throttles the retry so a persistently failing collector doesn't spam wakes.
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "dial", Runtime: runtimeCloudRun}},
	}
	tr := &fakeTransport{err: errBoom{}}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if db.attempted["dial"] != 1 {
		t.Errorf("attempted[dial] = %d, want 1 (attempt always stamped)", db.attempted["dial"])
	}
}

func TestDispatchOnceSkipsRunWhenStampFails(t *testing.T) {
	// When TouchCollectorAttempted fails, Run must NOT be called — waking without stamping
	// would bypass the retry throttle (last_attempt_at not updated, so the next pass would see
	// the collector as still due and wake it again immediately).
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "harvest", Runtime: runtimeCloudRun}},
		attemptErr:    errBoom{},
	}
	tr := &fakeTransport{}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if len(tr.called) != 0 {
		t.Errorf("runner called %d times, want 0 when stamp fails (throttle protection)", len(tr.called))
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

// --- StampDispatchError / ClearDispatchError ------------------------------------

func TestDispatchOnceWorkerRunnerErrorStampsLastError(t *testing.T) {
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
	}
	tr := &fakeTransport{err: errBoom{}}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	msg, ok := db.stampedErrors["gate-barato"]
	if !ok {
		t.Fatal("StampDispatchError not called for failed worker wake")
	}
	if msg == "" {
		t.Error("StampDispatchError called with empty msg")
	}
}

func TestDispatchOnceWorkerRunnerSuccessClearsLastError(t *testing.T) {
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
	}
	runOnce(t, db)
	if db.clearedErrors["gate-barato"] != 1 {
		t.Errorf("ClearDispatchError called %d times, want 1 on success", db.clearedErrors["gate-barato"])
	}
}

func TestDispatchOnceCollectorRunnerErrorStampsLastError(t *testing.T) {
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "harvest", Runtime: runtimeCloudRun}},
	}
	tr := &fakeTransport{err: errBoom{}}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	msg, ok := db.stampedErrors["harvest"]
	if !ok {
		t.Fatal("StampDispatchError not called for failed collector wake")
	}
	if msg == "" {
		t.Error("StampDispatchError called with empty msg")
	}
}

func TestDispatchOnceCollectorRunnerSuccessClearsLastError(t *testing.T) {
	db := &mockDispatchDB{
		dueCollectors: []DispatchProvider{{Name: "harvest", Runtime: runtimeCloudRun}},
	}
	runOnce(t, db)
	if db.clearedErrors["harvest"] != 1 {
		t.Errorf("ClearDispatchError called %d times, want 1 on success", db.clearedErrors["harvest"])
	}
}

func TestDispatchOnceStampErrorIsBestEffort(t *testing.T) {
	// A StampDispatchError failure must not stop the loop or return an error.
	// We also verify StampDispatchError was actually called (not silently skipped).
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
		stampErr:  errBoom{},
	}
	tr := &fakeTransport{err: errBoom{}}
	if err := (&Dispatcher{db: db, runner: tr}).DispatchOnce(context.Background()); err != nil {
		t.Errorf("DispatchOnce must swallow stamp errors, got %v", err)
	}
	if _, ok := db.stampedErrors["gate-barato"]; !ok {
		t.Error("StampDispatchError was not called despite runner failure")
	}
}

func TestDispatchOnceClearErrorIsBestEffort(t *testing.T) {
	// A ClearDispatchError failure must not stop the loop or return an error.
	// We also verify ClearDispatchError was actually called (not silently skipped).
	db := &mockDispatchDB{
		steps:     []AssignedStep{{ItemID: 1, Seq: 2, AssignedProvider: "gate-barato"}},
		providers: map[string]DispatchProvider{"gate-barato": {Name: "gate-barato", Runtime: runtimeCloudRun}},
		clearErr:  errBoom{},
	}
	if err := runOnceErr((&Dispatcher{db: db, runner: &fakeTransport{}})); err != nil {
		t.Errorf("DispatchOnce must swallow clear errors, got %v", err)
	}
	if db.clearedErrors["gate-barato"] != 1 {
		t.Errorf("ClearDispatchError called %d times, want 1 despite clearErr", db.clearedErrors["gate-barato"])
	}
}

func TestStampDispatchErrorTruncatesLongMsg(t *testing.T) {
	// The pgxDispatchDB implementation must cap msg at maxDispatchErrorRunes before writing.
	// We test the cap function directly; db.go integration is validated by the rune count.
	longMsg := strings.Repeat("é", maxDispatchErrorRunes+10) // é = 2 bytes each
	capped := capDispatchError(longMsg)
	if utf8.RuneCountInString(capped) != maxDispatchErrorRunes {
		t.Errorf("capped rune count = %d, want %d", utf8.RuneCountInString(capped), maxDispatchErrorRunes)
	}
	if !utf8.ValidString(capped) {
		t.Error("capped string is not valid UTF-8")
	}
}

func TestStampDispatchErrorShortMsgUnchanged(t *testing.T) {
	short := "image not in allowlist"
	if got := capDispatchError(short); got != short {
		t.Errorf("capDispatchError(%q) = %q, want unchanged", short, got)
	}
}

func TestCapDispatchErrorStripsInvalidUTF8(t *testing.T) {
	// Invalid UTF-8 bytes must be removed before the string reaches Postgres (text column
	// rejects invalid UTF-8). Use a mix of valid ASCII + invalid bytes + valid tail.
	invalid := "prefix" + string([]byte{0xff, 0xfe}) + "suffix"
	got := capDispatchError(invalid)
	if !utf8.ValidString(got) {
		t.Errorf("capDispatchError output not valid UTF-8: %q", got)
	}
	if !strings.Contains(got, "prefix") {
		t.Errorf("capDispatchError stripped valid prefix bytes: %q", got)
	}
	if !strings.Contains(got, "suffix") {
		t.Errorf("capDispatchError stripped valid suffix bytes: %q", got)
	}
}

func TestSanitizeDispatchMsgRedactsBearer(t *testing.T) {
	cases := []struct{ in, wantContains, wantAbsent string }{
		{"cloud run token: Bearer eyJhbGci.secret123", "[REDACTED]", "eyJhbGci.secret123"},
		{"BEARER SuperSecretToken xyz", "[REDACTED]", "SuperSecretToken"},
		{"no token here: status 404", "status 404", ""},
	}
	for _, tc := range cases {
		got := sanitizeDispatchMsg(tc.in)
		if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
			t.Errorf("sanitizeDispatchMsg(%q) still contains %q: %q", tc.in, tc.wantAbsent, got)
		}
		if !strings.Contains(got, tc.wantContains) {
			t.Errorf("sanitizeDispatchMsg(%q) = %q, want to contain %q", tc.in, got, tc.wantContains)
		}
	}
}

// runOnceErr is like runOnce but returns any error from DispatchOnce instead of failing.
func runOnceErr(d *Dispatcher) error {
	return d.DispatchOnce(context.Background())
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
