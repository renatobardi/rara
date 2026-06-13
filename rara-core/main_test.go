package main

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pure validators (mirror the SQL CHECK enums)
// ---------------------------------------------------------------------------

func TestValidators(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) bool
		ok   []string
		bad  []string
	}{
		{"runtime", isValidRuntime, []string{runtimeLocal, runtimeCloudRun, runtimeVPC}, []string{"", "gpu", "edge"}},
		{"activation", isValidActivation, []string{activationResident, activationOnDemand}, []string{"", "lazy"}},
		{"itemStatus", isValidItemStatus, []string{itemDiscovered, itemToText, itemDistilled, itemDone, itemFiltered, itemQuarantine, itemFailed}, []string{"", "pending", "queued"}},
		{"stepStatus", isValidStepStatus, []string{stepPending, stepAssigned, stepRunning, stepDone, stepFailed, stepSkipped}, []string{"", "discovered", "to_text"}},
		{"gate", isValidGate, []string{gateBarato, gateRico}, []string{"", "gate_medio"}},
		{"decision", isValidDecision, []string{decisionKeep, decisionDrop, decisionDefer}, []string{"", "maybe"}},
		{"targetType", isValidTargetType, []string{targetItem, targetDistillation}, []string{"", "transcript"}},
	}
	for _, c := range cases {
		for _, v := range c.ok {
			if !c.fn(v) {
				t.Errorf("%s: %q should be valid", c.name, v)
			}
		}
		for _, v := range c.bad {
			if c.fn(v) {
				t.Errorf("%s: %q should be invalid", c.name, v)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// MockDatabase — an in-memory store that mirrors the SQL contract of
// migrations/001_initial_schema.sql: UNIQUE keys (upsert vs. duplicate-add),
// internal FK references (capability / flow / item / provider must exist), the CHECK
// enums (via the shared validators), and the append-only nature of the audit tables.
// Zero I/O — the whole persistence seam is exercised in memory.
// ---------------------------------------------------------------------------

var (
	errFKViolation     = errors.New("mock: foreign key violation")
	errUniqueViolation = errors.New("mock: unique violation")
	errCheckViolation  = errors.New("mock: check violation")
)

type itemStepKey struct {
	itemID int
	seq    int
}
type flowStepKey struct {
	flowID int
	seq    int
}

type MockDatabase struct {
	capabilities map[string]Capability // UNIQUE(name)
	providers    map[string]Provider   // UNIQUE(name)
	flows        map[string]Flow       // UNIQUE(name)
	flowByID     map[int]bool          // FK target for flow_steps.flow_id / items.flow_id
	flowSteps    map[flowStepKey]FlowStep
	policies     map[string]RoutingPolicy // UNIQUE(scope)

	items     map[string]Item          // UNIQUE(lane, source_ref) -> key "lane\x00source_ref"
	itemByID  map[int]Item             // id -> item (for GetItem / ListActiveItems)
	itemSteps map[itemStepKey]ItemStep // UNIQUE(item_id, seq)
	// stepOrder records item_steps insertion order so the claim can mirror the SQL
	// ORDER BY id (FIFO over the pending frontier).
	stepOrder    map[itemStepKey]int
	nextStepSeqN int

	gateDecisions []GateDecision          // append-only
	feedback      []Feedback              // append-only
	profiles      map[int]InterestProfile // UNIQUE(version)

	nextFlowID int
	nextItemID int
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		capabilities: make(map[string]Capability),
		providers:    make(map[string]Provider),
		flows:        make(map[string]Flow),
		flowByID:     make(map[int]bool),
		flowSteps:    make(map[flowStepKey]FlowStep),
		policies:     make(map[string]RoutingPolicy),
		items:        make(map[string]Item),
		itemByID:     make(map[int]Item),
		itemSteps:    make(map[itemStepKey]ItemStep),
		stepOrder:    make(map[itemStepKey]int),
		profiles:     make(map[int]InterestProfile),
		nextFlowID:   1,
		nextItemID:   1,
	}
}

func itemKey(lane, sourceRef string) string { return lane + "\x00" + sourceRef }

func (m *MockDatabase) UpsertCapability(_ context.Context, c Capability) error {
	m.capabilities[c.Name] = c // ON CONFLICT (name) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertProvider(_ context.Context, p Provider) error {
	if !isValidRuntime(p.Runtime) || !isValidActivation(p.Activation) {
		return errCheckViolation
	}
	if _, ok := m.capabilities[p.Capability]; !ok {
		return errFKViolation // REFERENCES capabilities(name)
	}
	m.providers[p.Name] = p // ON CONFLICT (name) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertFlow(_ context.Context, f Flow) (int, error) {
	if existing, ok := m.flows[f.Name]; ok {
		f.ID = existing.ID // ON CONFLICT (name) DO UPDATE keeps the row id
		m.flows[f.Name] = f
		return f.ID, nil
	}
	f.ID = m.nextFlowID
	m.nextFlowID++
	m.flows[f.Name] = f
	m.flowByID[f.ID] = true
	return f.ID, nil
}

func (m *MockDatabase) UpsertFlowStep(_ context.Context, s FlowStep) error {
	if !m.flowByID[s.FlowID] {
		return errFKViolation // REFERENCES flows(id)
	}
	if _, ok := m.capabilities[s.Capability]; !ok {
		return errFKViolation // REFERENCES capabilities(name)
	}
	m.flowSteps[flowStepKey{s.FlowID, s.Seq}] = s // ON CONFLICT (flow_id, seq) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertRoutingPolicy(_ context.Context, p RoutingPolicy) error {
	m.policies[p.Scope] = p // ON CONFLICT (scope) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertItem(_ context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, errCheckViolation
	}
	if !m.flowByID[it.FlowID] {
		return 0, errFKViolation // REFERENCES flows(id)
	}
	k := itemKey(it.Lane, it.SourceRef)
	if existing, ok := m.items[k]; ok {
		it.ID = existing.ID // ON CONFLICT (lane, source_ref) DO UPDATE keeps the row id
		m.items[k] = it
		m.itemByID[it.ID] = it
		return it.ID, nil
	}
	it.ID = m.nextItemID
	m.nextItemID++
	m.items[k] = it
	m.itemByID[it.ID] = it
	return it.ID, nil
}

// DiscoverItem mirrors the pgx idempotent discovery upsert: insert in the passed status,
// but on conflict PRESERVE the existing runtime status (re-stamping only flow fields).
func (m *MockDatabase) DiscoverItem(_ context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, errCheckViolation
	}
	if !m.flowByID[it.FlowID] {
		return 0, errFKViolation // REFERENCES flows(id)
	}
	k := itemKey(it.Lane, it.SourceRef)
	if existing, ok := m.items[k]; ok {
		existing.FlowID = it.FlowID
		existing.FlowVersion = it.FlowVersion // status untouched
		m.items[k] = existing
		m.itemByID[existing.ID] = existing
		return existing.ID, nil
	}
	it.ID = m.nextItemID
	m.nextItemID++
	m.items[k] = it
	m.itemByID[it.ID] = it
	return it.ID, nil
}

func (m *MockDatabase) UpsertItemStep(_ context.Context, s ItemStep) error {
	if !isValidStepStatus(s.Status) {
		return errCheckViolation
	}
	if _, ok := m.itemByID[s.ItemID]; !ok {
		return errFKViolation // REFERENCES items(id)
	}
	if _, ok := m.capabilities[s.Capability]; !ok {
		return errFKViolation // REFERENCES capabilities(name)
	}
	if s.AssignedProvider != "" {
		if _, ok := m.providers[s.AssignedProvider]; !ok {
			return errFKViolation // REFERENCES providers(name)
		}
	}
	k := itemStepKey{s.ItemID, s.Seq}
	if _, ok := m.stepOrder[k]; !ok {
		m.nextStepSeqN++
		m.stepOrder[k] = m.nextStepSeqN // record SERIAL-id insertion order for the claim
	}
	m.itemSteps[k] = s // ON CONFLICT (item_id, seq) DO UPDATE
	return nil
}

func (m *MockDatabase) InsertGateDecision(_ context.Context, d GateDecision) error {
	if !isValidGate(d.Gate) || !isValidDecision(d.Decision) {
		return errCheckViolation
	}
	if _, ok := m.itemByID[d.ItemID]; !ok {
		return errFKViolation
	}
	m.gateDecisions = append(m.gateDecisions, d) // append-only
	return nil
}

func (m *MockDatabase) InsertFeedback(_ context.Context, f Feedback) error {
	if !isValidTargetType(f.TargetType) {
		return errCheckViolation
	}
	m.feedback = append(m.feedback, f) // append-only
	return nil
}

func (m *MockDatabase) InsertInterestProfile(_ context.Context, p InterestProfile) error {
	if _, ok := m.profiles[p.Version]; ok {
		return errUniqueViolation // UNIQUE(version) — versions are immutable
	}
	m.profiles[p.Version] = p
	return nil
}

// --- Reads + claim (Phase 1) ----------------------------------------------
// These mirror the pgx implementations in store_reads.go: pure observation plus the one
// atomic claim. The claim simulates FOR UPDATE SKIP LOCKED by serving the lowest-insertion
// pending step and moving it to running, so a second claim never returns the same row.

// claimTime is the fixed heartbeat stamp the mock writes on claim (the real DB uses
// CURRENT_TIMESTAMP). Tests only assert it is non-nil.
var claimTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (m *MockDatabase) GetFlow(_ context.Context, name string) (Flow, bool, error) {
	f, ok := m.flows[name]
	return f, ok, nil
}

func (m *MockDatabase) GetItem(_ context.Context, id int) (Item, bool, error) {
	it, ok := m.itemByID[id]
	return it, ok, nil
}

func (m *MockDatabase) ListActiveItems(_ context.Context) ([]Item, error) {
	var out []Item
	for _, it := range m.items {
		switch it.Status {
		case itemDone, itemFiltered, itemFailed, itemQuarantine:
			// terminal — skip
		default:
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MockDatabase) ListFlowSteps(_ context.Context, flowID int) ([]FlowStep, error) {
	var out []FlowStep
	for k, s := range m.flowSteps {
		if k.flowID == flowID && s.Enabled {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *MockDatabase) ListItemSteps(_ context.Context, itemID int) ([]ItemStep, error) {
	var out []ItemStep
	for k, s := range m.itemSteps {
		if k.itemID == itemID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *MockDatabase) ListProvidersForCapability(_ context.Context, capability string) ([]Provider, error) {
	var out []Provider
	for _, p := range m.providers {
		if p.Capability == capability && p.Enabled {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MockDatabase) ClaimPendingStep(_ context.Context, capability string) (*ItemStep, error) {
	bestKey, bestOrder, found := itemStepKey{}, int(^uint(0)>>1), false
	for k, s := range m.itemSteps {
		if s.Capability == capability && s.Status == stepPending {
			if o := m.stepOrder[k]; o < bestOrder { // lowest insertion order = FIFO by id
				bestKey, bestOrder, found = k, o, true
			}
		}
	}
	if !found {
		return nil, nil
	}
	s := m.itemSteps[bestKey]
	s.Status = stepRunning // pending -> running, atomically leaving the pending frontier
	s.Attempt++
	hb := claimTime
	s.HeartbeatAt = &hb
	m.itemSteps[bestKey] = s
	return &s, nil
}

// compile-time guarantee the mock satisfies the seam the pgx impl does.
var _ Database = (*MockDatabase)(nil)

// seedFlow inserts a flow and returns its id, for tests that need a valid FK target
// for flow_steps.flow_id / items.flow_id.
func seedFlow(t *testing.T, db *MockDatabase) int {
	t.Helper()
	fid, err := db.UpsertFlow(context.Background(), Flow{Name: "test_flow", SourceType: "news", Enabled: true, Version: 1})
	if err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	return fid
}

// ---------------------------------------------------------------------------
// Config-table contract
// ---------------------------------------------------------------------------

func TestCapabilityUpsertIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar, Description: "v1"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar, Description: "v2"}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if len(db.capabilities) != 1 {
		t.Fatalf("UNIQUE(name) not honored: got %d rows", len(db.capabilities))
	}
	if got := db.capabilities[capDestilar].Description; got != "v2" {
		t.Errorf("upsert should replace: description = %q, want v2", got)
	}
}

func TestProviderRequiresCapability(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	p := Provider{Name: "asr-youtube", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident}
	if err := db.UpsertProvider(ctx, p); !errors.Is(err, errFKViolation) {
		t.Fatalf("provider with missing capability should fail FK, got %v", err)
	}
	// Register the capability, then it succeeds.
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("provider upsert after capability exists: %v", err)
	}
}

func TestProviderRejectsBadEnum(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	bad := Provider{Name: "x", Capability: capTranscrever, Runtime: "gpu", Activation: activationResident}
	if err := db.UpsertProvider(ctx, bad); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid runtime should fail CHECK, got %v", err)
	}
}

func TestProviderUpsertIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	p := Provider{Name: "asr-youtube", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident, Enabled: true}
	_ = db.UpsertProvider(ctx, p)
	p.Enabled = false // toggle
	_ = db.UpsertProvider(ctx, p)
	if len(db.providers) != 1 {
		t.Fatalf("UNIQUE(name) not honored: %d rows", len(db.providers))
	}
	if db.providers["asr-youtube"].Enabled {
		t.Errorf("upsert should replace enabled flag")
	}
}

func TestFlowUpsertReturnsStableID(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	id1, err := db.UpsertFlow(ctx, Flow{Name: "youtube_channels", SourceType: "youtube", Enabled: true, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.UpsertFlow(ctx, Flow{Name: "youtube_channels", SourceType: "youtube", Enabled: true, Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ON CONFLICT(name) must keep row id: %d != %d", id1, id2)
	}
	if len(db.flows) != 1 {
		t.Fatalf("UNIQUE(name) not honored: %d rows", len(db.flows))
	}
	if db.flows["youtube_channels"].Version != 2 {
		t.Errorf("version bump should persist")
	}
}

func TestFlowStepRequiresCapabilityAndUniqueSeq(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid, _ := db.UpsertFlow(ctx, Flow{Name: "news", SourceType: "news"})
	// Missing capability -> FK.
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: fid, Seq: 1, Capability: capColetar}); !errors.Is(err, errFKViolation) {
		t.Fatalf("flow_step with missing capability should fail FK, got %v", err)
	}
	_ = db.UpsertCapability(ctx, Capability{Name: capColetar})
	_ = db.UpsertCapability(ctx, Capability{Name: capExtrair})
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: fid, Seq: 1, Capability: capColetar}); err != nil {
		t.Fatal(err)
	}
	// Same (flow_id, seq) replaces, not duplicates.
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: fid, Seq: 1, Capability: capExtrair}); err != nil {
		t.Fatal(err)
	}
	if len(db.flowSteps) != 1 {
		t.Fatalf("UNIQUE(flow_id, seq) not honored: %d rows", len(db.flowSteps))
	}
	if db.flowSteps[flowStepKey{fid, 1}].Capability != capExtrair {
		t.Errorf("upsert should replace the step capability")
	}
}

func TestRoutingPolicyUniqueScope(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: "global", CostWeight: 0.5, QualityWeight: 0.5})
	_ = db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: "global", CostWeight: 0.3, QualityWeight: 0.7})
	if len(db.policies) != 1 {
		t.Fatalf("UNIQUE(scope) not honored: %d rows", len(db.policies))
	}
	if db.policies["global"].QualityWeight != 0.7 {
		t.Errorf("upsert should replace policy weights")
	}
}

// ---------------------------------------------------------------------------
// Spine contract
// ---------------------------------------------------------------------------

func TestItemDedupByLaneSourceRef(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	id1, err := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "vid123", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	if err != nil {
		t.Fatal(err)
	}
	// Same natural key re-discovered: collapses to one row, id stable.
	id2, err := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "vid123", FlowID: fid, FlowVersion: 1, Status: itemToText})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ON CONFLICT(lane, source_ref) must keep row id: %d != %d", id1, id2)
	}
	if len(db.items) != 1 {
		t.Fatalf("UNIQUE(lane, source_ref) not honored: %d rows", len(db.items))
	}
	// Same source_ref in a DIFFERENT lane is a distinct item.
	if _, err := db.UpsertItem(ctx, Item{Lane: "podcast", SourceRef: "vid123", FlowID: fid, FlowVersion: 1, Status: itemDiscovered}); err != nil {
		t.Fatal(err)
	}
	if len(db.items) != 2 {
		t.Fatalf("composite key should distinguish lanes: %d rows", len(db.items))
	}
}

func TestItemRejectsBadStatus(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	if _, err := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: fid, FlowVersion: 1, Status: "queued"}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid item status should fail CHECK, got %v", err)
	}
}

func TestItemRequiresExistingFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, err := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: 999, FlowVersion: 1, Status: itemDiscovered}); !errors.Is(err, errFKViolation) {
		t.Fatalf("item on unknown flow should fail FK, got %v", err)
	}
}

func TestFlowStepRequiresExistingFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capColetar})
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: 999, Seq: 1, Capability: capColetar}); !errors.Is(err, errFKViolation) {
		t.Fatalf("flow_step on unknown flow should fail FK, got %v", err)
	}
}

func TestItemStepUniquePerItemSeqAndFKs(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "v", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})

	// FK: unknown item.
	if err := db.UpsertItemStep(ctx, ItemStep{ItemID: 9999, Seq: 1, Capability: capTranscrever, Status: stepPending}); !errors.Is(err, errFKViolation) {
		t.Fatalf("item_step on unknown item should fail FK, got %v", err)
	}
	// FK: assigned_provider must exist when set.
	bad := ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepAssigned, AssignedProvider: "ghost"}
	if err := db.UpsertItemStep(ctx, bad); !errors.Is(err, errFKViolation) {
		t.Fatalf("item_step with unknown provider should fail FK, got %v", err)
	}
	// Happy path: pending step, no provider yet.
	if err := db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepPending}); err != nil {
		t.Fatal(err)
	}
	// Mutable in place: same (item_id, seq) advances status & bumps attempt (the
	// claim/retry pattern), one row.
	if err := db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepRunning, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if len(db.itemSteps) != 1 {
		t.Fatalf("UNIQUE(item_id, seq) not honored: %d rows", len(db.itemSteps))
	}
	got := db.itemSteps[itemStepKey{itemID, 1}]
	if got.Status != stepRunning || got.Attempt != 1 {
		t.Errorf("upsert should advance step in place: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Curation + learning contract (append-only / versioned)
// ---------------------------------------------------------------------------

func TestGateDecisionsAppendOnly(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	// Two runs of the same gate accumulate — history is the point (calibration sample).
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionDefer, DecidedBy: "rules"}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "llm-judge"}); err != nil {
		t.Fatal(err)
	}
	if len(db.gateDecisions) != 2 {
		t.Fatalf("gate_decisions must be append-only: %d rows", len(db.gateDecisions))
	}
	// Bad enum rejected.
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: "gate_medio", Decision: decisionKeep, DecidedBy: "x"}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid gate should fail CHECK, got %v", err)
	}
}

// gate_rico carries both a confidence score (0..1) and an integer rank (ordering);
// the two are distinct columns so a rank position can exceed the score's [0,1] range.
func TestGateDecisionRichScoreAndRank(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: fid, FlowVersion: 1, Status: itemToText})
	score := 0.82
	rank := 3
	if err := db.InsertGateDecision(ctx, GateDecision{
		ItemID: itemID, Gate: gateRico, Decision: decisionKeep,
		Score: &score, Rank: &rank, DecidedBy: "llm-judge",
	}); err != nil {
		t.Fatalf("rich gate decision with score+rank: %v", err)
	}
	got := db.gateDecisions[len(db.gateDecisions)-1]
	if got.Score == nil || *got.Score != score || got.Rank == nil || *got.Rank != rank {
		t.Errorf("score/rank not preserved: %+v", got)
	}
}

func TestFeedbackTargetTypeChecked(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.InsertFeedback(ctx, Feedback{TargetType: targetDistillation, TargetRef: "42", Signal: "up", Source: "explicit"}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertFeedback(ctx, Feedback{TargetType: "transcript", TargetRef: "1", Signal: "up", Source: "explicit"}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid target_type should fail CHECK, got %v", err)
	}
	if len(db.feedback) != 1 {
		t.Fatalf("only the valid row should persist: %d rows", len(db.feedback))
	}
}

func TestInterestProfileVersionImmutable(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	// Re-inserting the same version is rejected — revisions create a NEW version.
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1}); !errors.Is(err, errUniqueViolation) {
		t.Fatalf("UNIQUE(version) should reject duplicate, got %v", err)
	}
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2}); err != nil {
		t.Fatal(err)
	}
	if len(db.profiles) != 2 {
		t.Fatalf("each version is a distinct immutable row: %d rows", len(db.profiles))
	}
}
