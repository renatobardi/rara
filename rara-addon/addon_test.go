package addon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory Store: it enforces the same contract the pgx impl does (the claim's
// (capability, assigned_provider) filter + FIFO + single-claim transition) so the loop is tested
// with zero I/O. It is concurrency-safe so the resident heartbeat/poll/poke goroutines can touch
// it under the race detector.
type fakeStore struct {
	mu        sync.Mutex
	steps     map[stepKey]stepRec
	order     map[stepKey]int // insertion order -> FIFO by the SERIAL id the pgx impl uses
	nextOrder int
	items     map[int]Item
	hb        map[string]int // provider -> heartbeat count
	filtered  []int          // item ids curated out, in order
	claimErr  error          // optional injected error on the next Claim
}

type stepKey struct{ itemID, seq int }

// stepRec is the stored item_step: the claimed Step plus what Mark/Requeue wrote (output_ref +
// error), which the production Step type deliberately does not carry (the handler never reads them).
type stepRec struct {
	Step
	outputRef string
	errMsg    string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		steps: map[stepKey]stepRec{},
		order: map[stepKey]int{},
		items: map[int]Item{},
		hb:    map[string]int{},
	}
}

// addItem registers a spine item.
func (s *fakeStore) addItem(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[it.ID] = it
}

// addStep registers a pending item_step (the reconciler's assignment), preserving insertion order.
func (s *fakeStore) addStep(st Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.Status == "" {
		st.Status = StatusPending
	}
	k := stepKey{st.ItemID, st.Seq}
	if _, ok := s.order[k]; !ok {
		s.nextOrder++
		s.order[k] = s.nextOrder
	}
	s.steps[k] = stepRec{Step: st}
}

func (s *fakeStore) getStep(itemID, seq int) stepRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steps[stepKey{itemID, seq}]
}

func (s *fakeStore) hbCount(provider string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hb[provider]
}

func (s *fakeStore) Claim(_ context.Context, capability, provider string) (*Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		err := s.claimErr
		s.claimErr = nil
		return nil, err
	}
	bestKey, bestOrder, found := stepKey{}, int(^uint(0)>>1), false
	for k, st := range s.steps {
		if st.Capability == capability && st.AssignedProvider == provider && st.Status == StatusPending {
			if o := s.order[k]; o < bestOrder { // lowest insertion order = FIFO
				bestKey, bestOrder, found = k, o, true
			}
		}
	}
	if !found {
		return nil, nil
	}
	rec := s.steps[bestKey]
	rec.Status = StatusRunning // pending -> running, atomically leaving the frontier
	rec.Attempt++
	now := time.Now()
	rec.HeartbeatAt = &now
	s.steps[bestKey] = rec
	claimed := rec.Step
	return &claimed, nil
}

func (s *fakeStore) Heartbeat(_ context.Context, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hb[provider]++
	return nil
}

func (s *fakeStore) GetItem(_ context.Context, id int) (Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	return it, ok, nil
}

func (s *fakeStore) Mark(_ context.Context, step Step, status, outputRef, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stepKey{step.ItemID, step.Seq}
	rec := s.steps[k] // preserve heartbeat the claim stamped (targeted-update semantics)
	rec.Status = status
	rec.outputRef = outputRef
	rec.errMsg = errMsg
	s.steps[k] = rec
	return nil
}

func (s *fakeStore) Requeue(_ context.Context, step Step, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stepKey{step.ItemID, step.Seq}
	rec := s.steps[k]
	rec.Status = StatusPending
	rec.HeartbeatAt = nil // reads as un-owned again
	rec.outputRef = ""
	rec.errMsg = errMsg
	s.steps[k] = rec
	return nil
}

func (s *fakeStore) FilterItem(_ context.Context, item Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[item.ID]
	if it.Status == "filtered" {
		return nil
	}
	it.Status = "filtered"
	s.items[item.ID] = it
	s.filtered = append(s.filtered, item.ID)
	return nil
}

var _ Store = (*fakeStore)(nil)

// okHandler returns a fixed output_ref and records the items it saw.
func okHandler(ref string, seen *[]string) Handler {
	return func(_ context.Context, item Item, _ Step) (Result, error) {
		*seen = append(*seen, item.SourceRef)
		return Result{OutputRef: ref}, nil
	}
}

// seedOneStep registers one item + one pending step assigned to provider, returns the key parts.
func seedOneStep(s *fakeStore, itemID, seq int, cap, provider, sourceRef string) {
	s.addItem(Item{ID: itemID, SourceRef: sourceRef, Status: "discovered"})
	s.addStep(Step{ItemID: itemID, Seq: seq, Capability: cap, AssignedProvider: provider})
}

const (
	capT = "transcrever"
	prov = "asr-youtube"
)

// --- claim mechanics -------------------------------------------------------

// TestRunDrainsAndMarksDone: a drain claims the pending step, runs it, and records the domain row
// id back as output_ref with status done — and the claim stamped the provider heartbeat.
func TestRunDrainsAndMarksDone(t *testing.T) {
	s := newFakeStore()
	seedOneStep(s, 1, 3, capT, prov, "vid1")
	var seen []string
	cfg := Config{Capability: capT, Provider: prov, Store: s}

	if err := Run(context.Background(), cfg, okHandler("transcript-7", &seen)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 1 || seen[0] != "vid1" {
		t.Errorf("handler saw %v, want [vid1]", seen)
	}
	got := s.getStep(1, 3)
	if got.Status != StatusDone || got.outputRef != "transcript-7" {
		t.Errorf("step = %+v, want done output_ref=transcript-7", got)
	}
	if got.Attempt != 1 {
		t.Errorf("claim should bump attempt to 1, got %d", got.Attempt)
	}
	if got.HeartbeatAt == nil {
		t.Error("claim should stamp a heartbeat (preserved on done)")
	}
	if s.hbCount(prov) == 0 {
		t.Error("claim should stamp the provider heartbeat")
	}
}

// TestClaimNoDoubleClaimFIFO: two pending steps are claimed in insertion order, each once; a third
// claim returns nothing (the SKIP LOCKED contract).
func TestClaimNoDoubleClaimFIFO(t *testing.T) {
	s := newFakeStore()
	seedOneStep(s, 1, 3, capT, prov, "a")
	seedOneStep(s, 2, 3, capT, prov, "b")

	first, err := s.Claim(context.Background(), capT, prov)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v / %v", first, err)
	}
	second, err := s.Claim(context.Background(), capT, prov)
	if err != nil || second == nil {
		t.Fatalf("second claim: %v / %v", second, err)
	}
	if first.ItemID == second.ItemID {
		t.Fatalf("double-claimed the same step (item %d)", first.ItemID)
	}
	if first.ItemID != 1 || second.ItemID != 2 {
		t.Errorf("claim order = (%d,%d), want FIFO (1,2)", first.ItemID, second.ItemID)
	}
	if third, _ := s.Claim(context.Background(), capT, prov); third != nil {
		t.Errorf("third claim should be empty, got item %d", third.ItemID)
	}
}

// TestClaimProviderIsolation: two pending steps of one capability assigned to DIFFERENT providers —
// each worker claims only the step routed to its own provider, never the sibling's. This is the
// contract's whole point (a private item on distill-local must not be pulled by a third party).
func TestClaimProviderIsolation(t *testing.T) {
	s := newFakeStore()
	s.addItem(Item{ID: 1, SourceRef: "a"})
	s.addItem(Item{ID: 2, SourceRef: "b"})
	s.addStep(Step{ItemID: 1, Seq: 3, Capability: capT, AssignedProvider: "prov-a"})
	s.addStep(Step{ItemID: 2, Seq: 3, Capability: capT, AssignedProvider: "prov-b"})

	a, err := s.Claim(context.Background(), capT, "prov-a")
	if err != nil || a == nil || a.ItemID != 1 {
		t.Fatalf("prov-a should claim its own step 1, got %v / %v", a, err)
	}
	if again, _ := s.Claim(context.Background(), capT, "prov-a"); again != nil {
		t.Errorf("prov-a must NOT see prov-b's step, claimed item %d", again.ItemID)
	}
	b, _ := s.Claim(context.Background(), capT, "prov-b")
	if b == nil || b.ItemID != 2 {
		t.Errorf("prov-b should claim its own step 2, got %v", b)
	}
}

// TestRunIsolatesProviderDrain: Run for prov-a drains only prov-a's assignment; prov-b's step is
// left pending for its own worker.
func TestRunIsolatesProviderDrain(t *testing.T) {
	s := newFakeStore()
	s.addItem(Item{ID: 1, SourceRef: "a"})
	s.addItem(Item{ID: 2, SourceRef: "b"})
	s.addStep(Step{ItemID: 1, Seq: 3, Capability: capT, AssignedProvider: "prov-a"})
	s.addStep(Step{ItemID: 2, Seq: 3, Capability: capT, AssignedProvider: "prov-b"})

	var seen []string
	if err := Run(context.Background(), Config{Capability: capT, Provider: "prov-a", Store: s}, okHandler("x", &seen)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.getStep(1, 3).Status != StatusDone {
		t.Error("prov-a's step should be done")
	}
	if got := s.getStep(2, 3).Status; got != StatusPending {
		t.Errorf("prov-b's step must be untouched (pending), got %q", got)
	}
}

// --- result / failure paths ------------------------------------------------

// TestRunHandlerErrorFailsStep: a non-retryable handler error marks the step failed with the
// message.
func TestRunHandlerErrorFailsStep(t *testing.T) {
	s := newFakeStore()
	seedOneStep(s, 1, 3, capT, prov, "vid1")
	h := func(context.Context, Item, Step) (Result, error) { return Result{}, errors.New("asr exploded") }

	if err := Run(context.Background(), Config{Capability: capT, Provider: prov, Store: s}, h); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := s.getStep(1, 3)
	if got.Status != StatusFailed || got.errMsg != "asr exploded" {
		t.Errorf("step = %+v, want failed with the error recorded", got)
	}
}

// TestRunFiltersBenignNoContent: a Filtered result marks the step done with its output AND curates
// the item out (terminal `filtered`), so it is never driven into a downstream step that must fail.
func TestRunFiltersBenignNoContent(t *testing.T) {
	s := newFakeStore()
	seedOneStep(s, 1, 3, capT, prov, "vid1")
	h := func(context.Context, Item, Step) (Result, error) {
		return Result{OutputRef: "transcript-empty", Filtered: true}, nil
	}

	if err := Run(context.Background(), Config{Capability: capT, Provider: prov, Store: s}, h); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := s.getStep(1, 3)
	if got.Status != StatusDone || got.outputRef != "transcript-empty" {
		t.Errorf("step = %+v, want done with output_ref", got)
	}
	if it, _, _ := s.GetItem(context.Background(), 1); it.Status != "filtered" {
		t.Errorf("item status = %q, want filtered", it.Status)
	}
	if len(s.filtered) != 1 || s.filtered[0] != 1 {
		t.Errorf("filtered = %v, want [1]", s.filtered)
	}
}

// TestRequeueThenFailAtCeiling: a retryable miss re-queues the step (pending, heartbeat cleared,
// attempt kept) instead of failing it — until MaxAttempts, after which it fails for good.
func TestRequeueThenFailAtCeiling(t *testing.T) {
	s := newFakeStore()
	seedOneStep(s, 1, 3, capT, prov, "vid1")
	h := func(context.Context, Item, Step) (Result, error) {
		return Result{}, fmt.Errorf("batch not ready: %w", ErrRetryable)
	}
	cfg := Config{Capability: capT, Provider: prov, Store: s}

	// One runOnce: transient -> re-queued pending, not failed; heartbeat cleared.
	w := &worker{cfg: withDefaults(cfg), h: h}
	if _, err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	got := s.getStep(1, 3)
	if got.Status != StatusPending {
		t.Fatalf("after a transient miss the step should be re-queued pending, got %q", got.Status)
	}
	if got.HeartbeatAt != nil {
		t.Error("re-queued step should have its heartbeat cleared")
	}

	// Draining keeps retrying until the ceiling, then fails for good.
	if err := Run(context.Background(), cfg, h); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got = s.getStep(1, 3)
	if got.Status != StatusFailed {
		t.Errorf("after %d attempts the step should fail, got %q", DefaultMaxAttempts, got.Status)
	}
	if got.Attempt != DefaultMaxAttempts {
		t.Errorf("attempt = %d, want the ceiling %d", got.Attempt, DefaultMaxAttempts)
	}
}

// TestRunItemNotFoundFailsStep: if the item vanished between claim and read, the orphan step is
// failed so it leaves the running set.
func TestRunItemNotFoundFailsStep(t *testing.T) {
	s := newFakeStore()
	// Step exists but no item registered.
	s.addStep(Step{ItemID: 1, Seq: 3, Capability: capT, AssignedProvider: prov})
	var seen []string

	if err := Run(context.Background(), Config{Capability: capT, Provider: prov, Store: s}, okHandler("x", &seen)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := s.getStep(1, 3)
	if got.Status != StatusFailed || got.errMsg != "item not found" {
		t.Errorf("orphan step = %+v, want failed 'item not found'", got)
	}
	if len(seen) != 0 {
		t.Error("handler must not run for a vanished item")
	}
}

// TestStoreErrorPropagates: a Claim error aborts the drain and surfaces from Run.
func TestStoreErrorPropagates(t *testing.T) {
	s := newFakeStore()
	s.claimErr = errors.New("db down")
	var seen []string
	err := Run(context.Background(), Config{Capability: capT, Provider: prov, Store: s}, okHandler("x", &seen))
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("Run should surface the store error, got %v", err)
	}
}

// --- lifecycle: on_demand vs resident --------------------------------------

// TestOnDemandDrainsAndReturns: with no PollInterval and no poke listener, Run drains once and
// returns (the woken Cloud Run job pattern).
func TestOnDemandDrainsAndReturns(t *testing.T) {
	s := newFakeStore()
	for i, ref := range []string{"a", "b", "c"} {
		seedOneStep(s, i+1, 3, capT, prov, ref)
	}
	var seen []string
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{Capability: capT, Provider: prov, Store: s}, okHandler("x", &seen))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("on_demand Run should return after draining, but it blocked")
	}
	if len(seen) != 3 {
		t.Errorf("drained %d steps, want 3", len(seen))
	}
}

// TestResidentStopsOnContextCancel: a resident Run (PollInterval set) blocks until ctx is cancelled,
// then returns context.Canceled.
func TestResidentStopsOnContextCancel(t *testing.T) {
	s := newFakeStore()
	seedOneStep(s, 1, 3, capT, prov, "vid1")
	var seen []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{Capability: capT, Provider: prov, Store: s, PollInterval: 20 * time.Millisecond}
	go func() { done <- Run(ctx, cfg, okHandler("x", &seen)) }()

	// The up-front drain processes the pending step.
	waitFor(t, time.Second, func() bool { return s.getStep(1, 3).Status == StatusDone })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("resident Run should stop with context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resident Run did not stop after cancel")
	}
}

// TestPollFallbackDrains: a resident worker with no poke still drains work that arrives AFTER start,
// via the poll safety net.
func TestPollFallbackDrains(t *testing.T) {
	s := newFakeStore()
	var seen []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Capability: capT, Provider: prov, Store: s, PollInterval: 15 * time.Millisecond}
	go func() { _ = Run(ctx, cfg, okHandler("x", &seen)) }()

	// Work appears only after Run has started and done its (empty) up-front drain.
	time.Sleep(30 * time.Millisecond)
	seedOneStep(s, 1, 3, capT, prov, "late")
	waitFor(t, time.Second, func() bool { return s.getStep(1, 3).Status == StatusDone })
}

// TestPeriodicHeartbeat: a resident worker stamps the provider heartbeat on its cadence even with no
// work to do (so the router's health gate keeps it eligible).
func TestPeriodicHeartbeat(t *testing.T) {
	s := newFakeStore()
	var seen []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Capability: capT, Provider: prov, Store: s, PollInterval: time.Hour, HeartbeatInterval: 10 * time.Millisecond}
	go func() { _ = Run(ctx, cfg, okHandler("x", &seen)) }()

	waitFor(t, time.Second, func() bool { return s.hbCount(prov) >= 2 })
}

// --- poke listener ---------------------------------------------------------

// TestPokeListenerAuthAndSignal: /poke requires the bearer token (fail-closed) and POSTs a drain
// signal; /healthz is open. Tests the listener directly.
func TestPokeListenerAuthAndSignal(t *testing.T) {
	ch := make(chan struct{}, 1)
	srv, addr, err := startPokeListener("127.0.0.1:0", "secret", ch)
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	defer srv.Close()
	base := "http://" + addr

	// /healthz is open.
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d, want 200", resp.StatusCode)
	}

	// No token -> 401, no signal.
	if code := postPoke(t, base, ""); code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", code)
	}
	// Wrong token -> 401.
	if code := postPoke(t, base, "nope"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", code)
	}
	select {
	case <-ch:
		t.Fatal("unauthorized poke must not signal a drain")
	default:
	}

	// Right token -> 202 + signal.
	if code := postPoke(t, base, "secret"); code != http.StatusAccepted {
		t.Errorf("good token: status %d, want 202", code)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("authorized poke should signal a drain")
	}
}

// TestPokeListenerFailsClosedWithEmptyToken: an empty configured token rejects even an empty bearer.
func TestPokeListenerFailsClosedWithEmptyToken(t *testing.T) {
	ch := make(chan struct{}, 1)
	srv, addr, err := startPokeListener("127.0.0.1:0", "", ch)
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	defer srv.Close()
	if code := postPoke(t, "http://"+addr, ""); code != http.StatusUnauthorized {
		t.Errorf("empty token must fail closed: status %d, want 401", code)
	}
}

// TestPokeTriggersDrain: a resident worker with a poke listener drains immediately on POST /poke
// (the symmetric-activation path), not only on the slow poll.
func TestPokeTriggersDrain(t *testing.T) {
	s := newFakeStore()
	var seen []string
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{
		Capability: capT, Provider: prov, Store: s,
		PollInterval: time.Hour, // poll effectively disabled; the poke must do the work
		PokeAddr:     addr, PokeToken: "tok",
	}
	go func() { _ = Run(ctx, cfg, okHandler("x", &seen)) }()

	// Wait for the listener, then add work and poke.
	waitForListener(t, addr)
	seedOneStep(s, 1, 3, capT, prov, "poked")
	if code := postPoke(t, "http://"+addr, "tok"); code != http.StatusAccepted {
		t.Fatalf("poke status %d", code)
	}
	waitFor(t, time.Second, func() bool { return s.getStep(1, 3).Status == StatusDone })
}

// --- config validation -----------------------------------------------------

func TestConfigValidation(t *testing.T) {
	s := newFakeStore()
	h := func(context.Context, Item, Step) (Result, error) { return Result{}, nil }
	cases := []struct {
		name string
		cfg  Config
		h    Handler
	}{
		{"no capability", Config{Provider: prov, Store: s}, h},
		{"no provider", Config{Capability: capT, Store: s}, h},
		{"no store", Config{Capability: capT, Provider: prov}, h},
		{"no handler", Config{Capability: capT, Provider: prov, Store: s}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Run(context.Background(), tc.cfg, tc.h); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

// --- helpers ---------------------------------------------------------------

func withDefaults(c Config) Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	return c
}

func postPoke(t *testing.T, base, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/poke", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("poke request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// freePort returns a loopback address whose port is currently free (best-effort: closed before use).
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitForListener blocks until the poke listener at addr accepts a connection.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener at %s never came up", addr)
}

// waitFor polls cond until true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
