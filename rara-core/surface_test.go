package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "s3cr3t-service-token"

// newTestCore wires a Core over a fresh MockDatabase + fake LinkedIn store.
func newTestCore(t *testing.T) (*Core, *MockDatabase, *fakeLinkedInStore) {
	t.Helper()
	db := newMockDatabase()
	store := newFakeLinkedInStore()
	return NewCore(db, store), db, store
}

// ---------------------------------------------------------------------------
// Core operations (against the MockDatabase, zero I/O)
// ---------------------------------------------------------------------------

func TestCoreListItemsValidatesStatus(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.ListItems(ctx, "bogus"); err == nil {
		t.Fatal("unknown status should be a bad-input error")
	} else {
		var bad badInputError
		if !errors.As(err, &bad) {
			t.Errorf("want badInputError, got %T", err)
		}
	}
}

func TestCoreListItemsByStatus(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	mustItem(t, db, "youtube", "a", fid, itemDiscovered)
	mustItem(t, db, "youtube", "b", fid, itemDone)
	mustItem(t, db, "youtube", "c", fid, itemDiscovered)

	got, err := core.ListItems(ctx, itemDiscovered)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 discovered items, got %d", len(got))
	}
}

func TestCoreItemDecisionsFullTrail(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "x", FlowID: fid, FlowVersion: 1, Status: itemToText})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionDefer, DecidedBy: "rules"})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "llm"})

	decs, err := core.ItemDecisions(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 || decs[0].Decision != decisionDefer || decs[1].Decision != decisionKeep {
		t.Errorf("decisions trail = %+v, want [defer, keep] in order", decs)
	}
}

func TestCoreRecentDecisionsReturnsNewestFirst(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "x", FlowID: fid, FlowVersion: 1, Status: itemToText})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionDefer, DecidedBy: "rules"})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "llm"})

	decs, err := core.RecentDecisions(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(decs))
	}
	// Newest first: keep (inserted second) must be decs[0]
	if decs[0].Decision != decisionKeep {
		t.Errorf("want keep first (newest), got %s", decs[0].Decision)
	}
}

func TestCoreRecentDecisionsDefaultLimit(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "x", FlowID: fid, FlowVersion: 1, Status: itemToText})
	for i := 0; i < 60; i++ {
		_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "rules"})
	}

	decs, err := core.RecentDecisions(ctx, 0) // 0 → default 50
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 50 {
		t.Errorf("want 50 (default limit), got %d", len(decs))
	}
}

func TestCoreRecentDecisionsCapsAt200(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "x", FlowID: fid, FlowVersion: 1, Status: itemToText})
	for i := 0; i < 210; i++ {
		_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "rules"})
	}

	decs, err := core.RecentDecisions(ctx, 999) // over cap → clamped to 200
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 200 {
		t.Errorf("want 200 (cap), got %d", len(decs))
	}
}

func TestHTTPListDecisionsFeed(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "x", FlowID: fid, FlowVersion: 1, Status: itemToText})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "rules"})

	mux := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest("GET", "/v1/decisions", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["gate"] != gateBarato {
		t.Errorf("decisions = %+v, want 1 gate_barato decision", got)
	}
}

func TestHTTPListDecisionsFeedForwardsLimit(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "x", FlowID: fid, FlowVersion: 1, Status: itemToText})
	for i := 0; i < 5; i++ {
		_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "rules"})
	}

	mux := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest("GET", "/v1/decisions?limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	var got []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Errorf("want 2 (limit=2), got %d", len(got))
	}
}

func TestCoreConfigReadsAndEdits(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	flows, err := core.Flows(ctx)
	if err != nil || len(flows) == 0 {
		t.Fatalf("flows: %v (%d)", err, len(flows))
	}
	providers, err := core.Providers(ctx)
	if err != nil || len(providers) == 0 {
		t.Fatalf("providers: %v (%d)", err, len(providers))
	}
	// Edit a provider through Core (disable it), then read it back.
	p := providers[0]
	p.Enabled = false
	if err := core.UpsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	after, _ := core.Providers(ctx)
	for _, q := range after {
		if q.Name == p.Name && q.Enabled {
			t.Errorf("provider %q should be disabled after edit", p.Name)
		}
	}
}

func TestCoreAddInterestProfileValidatesVersion(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if err := core.AddInterestProfile(ctx, InterestProfile{Version: 0}); err == nil {
		t.Error("version 0 should be rejected as bad input")
	}
	if err := core.AddInterestProfile(ctx, InterestProfile{Version: 1}); err != nil {
		t.Fatalf("valid version: %v", err)
	}
	// A manually added version is a PROPOSAL — it is NOT active until approved, so the active read
	// finds nothing yet, while the versions list shows the proposed v1.
	if _, found, err := core.InterestProfile(ctx); err != nil || found {
		t.Errorf("a freshly added version must not be active (found=%v, err=%v)", found, err)
	}
	profs, err := core.InterestProfiles(ctx)
	if err != nil || len(profs) != 1 || profs[0].Status != profileProposed {
		t.Fatalf("versions = %+v (err=%v), want one proposed v1", profs, err)
	}
}

// Approval activates a proposed version and makes it the one the active read returns; approving a
// non-proposed (or absent) version is a 400-class caller error.
func TestCoreApproveProfile(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if err := core.AddInterestProfile(ctx, InterestProfile{Version: 1, Topics: json.RawMessage(`["ai"]`)}); err != nil {
		t.Fatal(err)
	}
	// Approving an absent version is bad input.
	if err := core.ApproveProfile(ctx, 99); err == nil {
		t.Error("approving an absent version should be bad input")
	}
	if err := core.ApproveProfile(ctx, 0); err == nil {
		t.Error("approving version 0 should be bad input")
	}
	if err := core.ApproveProfile(ctx, 1); err != nil {
		t.Fatalf("approve v1: %v", err)
	}
	prof, found, err := core.InterestProfile(ctx)
	if err != nil || !found || prof.Version != 1 || prof.Status != profileActive {
		t.Errorf("after approval, active = v%d/%s (found=%v): %v", prof.Version, prof.Status, found, err)
	}
	// Re-approving an already-active version is rejected (it is no longer proposed).
	if err := core.ApproveProfile(ctx, 1); err == nil {
		t.Error("re-approving an active version should be bad input")
	}
}

// TestCoreUpsertProviderPreservesHeartbeat: a config edit (e.g. toggling enabled) that omits
// heartbeat_at must NOT clear the provider's runtime liveness — heartbeat is owned by the
// worker, not config.
func TestCoreUpsertProviderPreservesHeartbeat(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	markProviderAlive(t, db, provASRYouTube) // a live resident worker stamped its heartbeat
	before, _, _ := db.GetProvider(ctx, provASRYouTube)
	if before.HeartbeatAt == nil {
		t.Fatal("precondition: heartbeat should be set")
	}
	edit := before
	edit.HeartbeatAt = nil // the request body omits it (the common case)
	edit.Enabled = false   // ...while toggling a real config field
	if err := core.UpsertProvider(ctx, edit); err != nil {
		t.Fatal(err)
	}
	after, _, _ := db.GetProvider(ctx, provASRYouTube)
	if after.HeartbeatAt == nil {
		t.Error("config edit cleared heartbeat_at; runtime liveness must be preserved")
	}
	if after.Enabled {
		t.Error("the enabled toggle should still apply")
	}
}

// ---------------------------------------------------------------------------
// Auth middleware (fail-closed)
// ---------------------------------------------------------------------------

func TestAuthRejectsMissingAndWrongToken(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	for _, tc := range []struct {
		name, auth string
	}{
		{"no header", ""},
		{"wrong token", "Bearer nope"},
		{"no bearer prefix", testToken},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/flows", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, rec.Code)
		}
	}
}

func TestAuthFailsClosedOnEmptyToken(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, "") // misconfigured: no token
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/flows", nil)
	req.Header.Set("Authorization", "Bearer ") // even a matching-empty bearer must be refused
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty configured token must reject everything, got %d", rec.Code)
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/live should be open, got %d", rec.Code)
	}
}

func TestServeSurfaceRefusesWithoutToken(t *testing.T) {
	core, _, _ := newTestCore(t)
	if err := ServeSurface(context.Background(), core, ":0", ""); err == nil {
		t.Error("ServeSurface must refuse to serve without a token")
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers (httptest + auth)
// ---------------------------------------------------------------------------

// do issues an authenticated request against the surface and returns the recorder.
func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestHTTPListItemsByStatus(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	mustItem(t, db, "youtube", "v1", fid, itemDiscovered)
	mustItem(t, db, "youtube", "v2", fid, itemDone)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/items?status=discovered", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var items []Item
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SourceRef != "v1" {
		t.Errorf("items = %+v, want [v1]", items)
	}
	_ = ctx
}

func TestHTTPBadStatusIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/items?status=nonsense", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad status should be 400, got %d", rec.Code)
	}
}

func TestHTTPInvalidJSONIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPut, "/v1/providers", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body should be 400, got %d", rec.Code)
	}
}

func TestHTTPUpsertProviderInvalidEnumIs400(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	h := NewSurfaceMux(core, testToken)
	// A bad runtime is caller input -> 400, not 500.
	body := `{"name":"x","capability":"destilar","runtime":"gpu","activation":"on_demand","enabled":true}`
	rec := do(t, h, http.MethodPut, "/v1/providers", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid runtime should be 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := db.providers["x"]; ok {
		t.Error("an invalid provider must not be written")
	}
}

func TestHTTPUpsertProviderRoundTrips(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil { // capability FK target
		t.Fatal(err)
	}
	h := NewSurfaceMux(core, testToken)
	body := `{"name":"distill-x","capability":"destilar","runtime":"cloudrun","activation":"on_demand","cost":1.5,"quality":0.9,"latency_ms":1000,"enabled":true}`
	rec := do(t, h, http.MethodPut, "/v1/providers", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if p, ok := db.providers["distill-x"]; !ok || p.Capability != capDestilar || !p.Enabled {
		t.Errorf("provider not upserted via HTTP: %+v", p)
	}
}

func TestHTTPLinkedinInboxDiscoversItem(t *testing.T) {
	ctx := context.Background()
	core, db, store := newTestCore(t)
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	h := NewSurfaceMux(core, testToken)
	body := `{"url":"https://lnkd.in/abc","author":"Renato","text":"on control planes"}`
	rec := do(t, h, http.MethodPost, "/v1/linkedin/inbox", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ItemID int `json:"item_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ItemID == 0 {
		t.Fatal("no item_id returned")
	}
	if it := db.itemByID[resp.ItemID]; it.Lane != laneLinkedIn || it.Sensitivity != sensitivityPublic {
		t.Errorf("item = %+v, want linkedin/public", it)
	}
	if _, ok := store.posts["https://lnkd.in/abc"]; !ok {
		t.Error("post not written to the inbox store")
	}
}

func TestHTTPReviewQuarantineRescues(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// An item parked in quarantine by a gate_barato defer.
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "q1", FlowID: db.flows[youtubeFlowName].ID, FlowVersion: 1, Status: itemQuarantine})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionDefer, DecidedBy: "llm"})

	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPost, "/v1/quarantine/review", `{"item_id":`+strconv.Itoa(id)+`,"signal":"up"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if got := db.itemByID[id].Status; got == itemQuarantine {
		t.Errorf("item should be rescued out of quarantine, still %q", got)
	}
	if len(db.feedback) != 1 || db.feedback[0].Source != sourceQuarantineReview {
		t.Errorf("review should record quarantine_review feedback: %+v", db.feedback)
	}
}

func TestHTTPReviewNonQuarantinedIs400(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// A discovered (not quarantined) item: reviewing it is a caller error, not a 500.
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "d", FlowID: db.flows[youtubeFlowName].ID, FlowVersion: 1, Status: itemDiscovered})
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPost, "/v1/quarantine/review", `{"item_id":`+strconv.Itoa(id)+`,"signal":"up"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reviewing a non-quarantined item should be 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPOversizedBodyIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	big := `{"name":"` + strings.Repeat("a", (1<<20)+16) + `"}` // > maxBodyBytes
	rec := do(t, h, http.MethodPut, "/v1/providers", big)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an oversized body should be 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Distillation reads — Core + HTTP
// ---------------------------------------------------------------------------

func TestCoreRecentDistillationsRespectLimitAndOrder(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	// Seed 3 distillations in insertion order (older → newer).
	seedDistillation(t, db, 1, "content-a")
	seedDistillation(t, db, 2, "content-b")
	seedDistillation(t, db, 3, "content-c")

	got, err := core.RecentDistillations(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2: want 2 summaries, got %d", len(got))
	}
	// Newest-first: id=3 then id=2.
	if got[0].ID != 3 || got[1].ID != 2 {
		t.Errorf("want [3,2], got [%d,%d]", got[0].ID, got[1].ID)
	}
}

func TestCoreRecentDistillationsDefaultsAndCap(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	// limit=0 → default 50 (no panic, just returns what's there — empty).
	got, err := core.RecentDistillations(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("empty list should be non-nil slice, got nil")
	}

	// limit above cap → capped at 200 (verify Core accepts it without error).
	got, err = core.RecentDistillations(ctx, 999)
	if err != nil {
		t.Fatal(err)
	}
	_ = got
}

func TestCoreRecentDistillationsReturnsSummaries(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	seedDistillation(t, db, 1, "should-not-appear")

	summaries, err := core.RecentDistillations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("want 1 summary, got %d", len(summaries))
	}
	if summaries[0].ID != 1 || summaries[0].Engine == "" {
		t.Errorf("summary fields wrong: %+v", summaries[0])
	}
}

func TestCoreGetDistillationReturnsContent(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	seedDistillation(t, db, 7, "# Wisdom\nsome insights")

	d, err := core.GetDistillation(ctx, 7)
	if err != nil {
		t.Fatalf("get distillation 7: %v", err)
	}
	if d.ID != 7 {
		t.Errorf("want id=7, got %d", d.ID)
	}
	if d.Content == nil || *d.Content != "# Wisdom\nsome insights" {
		t.Errorf("want content %q, got %v", "# Wisdom\nsome insights", d.Content)
	}
}

func TestCoreGetDistillationInvalidIDIsBadInput(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	_, err := core.GetDistillation(ctx, 0)
	if err == nil {
		t.Fatal("id=0 should be an error")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestCoreGetDistillationNotFoundIsBadInput(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	_, err := core.GetDistillation(ctx, 99)
	if err == nil {
		t.Fatal("missing distillation should be an error")
	}
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Errorf("want badInputError, got %T: %v", err, err)
	}
}

func TestHTTPListDistillationsReturnsJSON(t *testing.T) {
	core, db, _ := newTestCore(t)
	seedDistillation(t, db, 1, "content")
	seedDistillation(t, db, 2, "content2")
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/distillations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var summaries []DistillationSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("want 2 summaries, got %d", len(summaries))
	}
	// newest-first: id=2, id=1
	if summaries[0].ID != 2 || summaries[1].ID != 1 {
		t.Errorf("want [2,1], got [%d,%d]", summaries[0].ID, summaries[1].ID)
	}
}

func TestHTTPListDistillationsHasNoContentKey(t *testing.T) {
	core, db, _ := newTestCore(t)
	seedDistillation(t, db, 1, "secret markdown")
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/distillations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one row")
	}
	if _, hasContent := rows[0]["content"]; hasContent {
		t.Error("list response must not include the content field")
	}
}

func TestHTTPListDistillationsRequiresAuth(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	req := httptest.NewRequest(http.MethodGet, "/v1/distillations", nil) // no auth
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rec.Code)
	}
}

func TestHTTPGetDistillationReturnsContent(t *testing.T) {
	core, db, _ := newTestCore(t)
	seedDistillation(t, db, 5, "the content")
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/distillations/5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var d Distillation
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != 5 {
		t.Errorf("want id=5, got %d", d.ID)
	}
	if d.Content == nil || *d.Content != "the content" {
		t.Errorf("want content %q, got %v", "the content", d.Content)
	}
}

func TestHTTPGetDistillationInvalidIDIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/distillations/abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad path id should be 400, got %d", rec.Code)
	}
}

func TestHTTPGetDistillationNotFoundIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/distillations/999", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing distillation should be 400, got %d", rec.Code)
	}
}

// TestCoreListItemsPassesThroughDisplayFields: Title/Channel/Summary/PublishedAt stored on an
// item are returned by ListItems. Also verifies that lanes without a date (linkedin) return nil.
func TestCoreListItemsPassesThroughDisplayFields(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	ts := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	_, err := db.UpsertItem(ctx, Item{
		Lane: "podcast", SourceRef: "ep-01", FlowID: fid, FlowVersion: 1,
		Status: itemDiscovered,
		Title:  "My Episode", Channel: "My Channel", Summary: "Short summary",
		PublishedAt: &ts,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.UpsertItem(ctx, Item{
		Lane: "linkedin", SourceRef: "https://li/p1", FlowID: fid, FlowVersion: 1,
		Status: itemDiscovered, Title: "Post",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := core.ListItems(ctx, itemDiscovered)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		switch it.Lane {
		case "podcast":
			if it.Title != "My Episode" {
				t.Errorf("Title = %q, want My Episode", it.Title)
			}
			if it.Channel != "My Channel" {
				t.Errorf("Channel = %q, want My Channel", it.Channel)
			}
			if it.Summary != "Short summary" {
				t.Errorf("Summary = %q, want Short summary", it.Summary)
			}
			if it.PublishedAt == nil || !it.PublishedAt.Equal(ts) {
				t.Errorf("podcast PublishedAt = %v, want %v", it.PublishedAt, ts)
			}
		case "linkedin":
			if it.PublishedAt != nil {
				t.Errorf("linkedin PublishedAt = %v, want nil", it.PublishedAt)
			}
		}
	}
}

// TestCoreQuarantinePassesThroughDisplayFields: same contract for Quarantine().
func TestCoreQuarantinePassesThroughDisplayFields(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	_, err := db.UpsertItem(ctx, Item{
		Lane: "podcast", SourceRef: "ep-q", FlowID: fid, FlowVersion: 1,
		Status: itemQuarantine,
		Title:  "Quarantine Episode", Channel: "Some Cast",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := core.Quarantine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "Quarantine Episode" {
		t.Errorf("quarantine item title = %q", items[0].Title)
	}
}

// TestHTTPItemsResponseIncludesDisplayFields: the JSON response from GET /v1/items includes
// title/channel/summary/published_at when available.
func TestHTTPItemsResponseIncludesDisplayFields(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.UpsertItem(ctx, Item{
		Lane: "podcast", SourceRef: "ep-http", FlowID: fid, FlowVersion: 1,
		Status: itemDiscovered,
		Title:  "HTTP Episode", Channel: "HTTP Cast", Summary: "A summary",
		PublishedAt: &ts,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/items?status=discovered", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var items []Item
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "HTTP Episode" || items[0].Channel != "HTTP Cast" {
		t.Errorf("items = %+v", items)
	}
	if items[0].PublishedAt == nil || !items[0].PublishedAt.Equal(ts) {
		t.Errorf("PublishedAt = %v, want %v", items[0].PublishedAt, ts)
	}
	_ = ctx
}

// --- health + usage -------------------------------------------------------

func TestCoreHealthDBOK(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	report := core.Health(ctx)
	if !report.DBOk {
		t.Error("db_ok should be true for mock database")
	}
}

func TestCoreHealthNoReconcileYet(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	// Reset so this test doesn't depend on a prior stamp from another test.
	prev := atomic.SwapInt64(&lastReconcileNano, 0)
	t.Cleanup(func() { atomic.StoreInt64(&lastReconcileNano, prev) })
	report := core.Health(ctx)
	if report.LastReconcileAt != nil {
		t.Errorf("last_reconcile_at should be nil when never stamped, got %v", report.LastReconcileAt)
	}
}

func TestCoreHealthStaleProvider(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := db.UpsertCapability(ctx, Capability{Name: "transcrever"}); err != nil {
		t.Fatalf("seed capability: %v", err)
	}

	fresh := time.Now().Add(-1 * time.Minute)
	stale := time.Now().Add(-10 * time.Minute) // older than defaultHealthTTL (5m)
	if err := db.UpsertProvider(ctx, Provider{
		Name: "asr-fresh", Capability: "transcrever",
		Runtime: runtimeLocal, Activation: activationResident,
		Enabled: true, HeartbeatAt: &fresh,
	}); err != nil {
		t.Fatalf("seed provider asr-fresh: %v", err)
	}
	if err := db.UpsertProvider(ctx, Provider{
		Name: "asr-stale", Capability: "transcrever",
		Runtime: runtimeLocal, Activation: activationResident,
		Enabled: true, HeartbeatAt: &stale,
	}); err != nil {
		t.Fatalf("seed provider asr-stale: %v", err)
	}
	if err := db.UpsertProvider(ctx, Provider{
		Name: "asr-on-demand", Capability: "transcrever",
		Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Enabled: true, HeartbeatAt: &stale, // on_demand: exempt from staleness
	}); err != nil {
		t.Fatalf("seed provider asr-on-demand: %v", err)
	}
	if err := db.UpsertProvider(ctx, Provider{
		Name: "asr-disabled", Capability: "transcrever",
		Runtime: runtimeLocal, Activation: activationResident,
		Enabled: false, HeartbeatAt: &stale,
	}); err != nil {
		t.Fatalf("seed provider asr-disabled: %v", err)
	}

	report := core.Health(ctx)
	if report.Providers.Total != 4 {
		t.Errorf("total = %d, want 4", report.Providers.Total)
	}
	if report.Providers.Enabled != 3 {
		t.Errorf("enabled = %d, want 3", report.Providers.Enabled)
	}
	// Only asr-stale is a stale RESIDENT; asr-on-demand is exempt; asr-disabled is stale
	// but also disabled — the health count covers enabled+disabled (it's a connectivity concern).
	if report.Providers.Stale != 2 { // asr-stale + asr-disabled (both resident, both old)
		t.Errorf("stale = %d, want 2 (resident stale regardless of enabled)", report.Providers.Stale)
	}
}

func TestHTTPHealth(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	prev := atomic.SwapInt64(&lastReconcileNano, 0)
	t.Cleanup(func() { atomic.StoreInt64(&lastReconcileNano, prev) })
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var report HealthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.DBOk {
		t.Error("db_ok should be true")
	}
	_ = ctx
}

func TestCoreUsageCounts(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	itemID := mustItem(t, db, "youtube", "v1", fid, itemDiscovered)
	mustItem(t, db, "youtube", "v2", fid, itemDiscovered)
	mustItem(t, db, "youtube", "v3", fid, itemDone)
	mustItem(t, db, "podcast", "ep1", fid, itemQuarantine)
	if err := db.UpsertCapability(ctx, Capability{Name: capTranscrever}); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
	if err := db.UpsertItemStep(ctx, ItemStep{
		ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepPending,
	}); err != nil {
		t.Fatalf("seed item step: %v", err)
	}
	seedDistillation(t, db, 1, "content")
	seedDistillation(t, db, 2, "content2")

	report, err := core.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ytDisc int
	for _, ic := range report.Items {
		if ic.Lane == "youtube" && ic.Status == itemDiscovered {
			ytDisc = ic.Count
		}
	}
	if ytDisc != 2 {
		t.Errorf("youtube/discovered = %d, want 2", ytDisc)
	}
	if report.Distillations != 2 {
		t.Errorf("distillations = %d, want 2", report.Distillations)
	}
	if report.Quarantine != 1 {
		t.Errorf("quarantine = %d, want 1", report.Quarantine)
	}
	var pending int
	for _, sc := range report.ItemSteps {
		if sc.Capability == capTranscrever && sc.Status == stepPending {
			pending = sc.Count
		}
	}
	if pending != 1 {
		t.Errorf("item_steps %s/pending = %d, want 1", capTranscrever, pending)
	}
}

func TestHTTPUsage(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	mustItem(t, db, "youtube", "v1", fid, itemDiscovered)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodGet, "/v1/usage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var report UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Items) == 0 {
		t.Error("items should not be empty")
	}
	_ = ctx
}

// --- small helpers --------------------------------------------------------

func mustItem(t *testing.T, db *MockDatabase, lane, ref string, flowID int, status string) int {
	t.Helper()
	id, err := db.UpsertItem(context.Background(), Item{Lane: lane, SourceRef: ref, FlowID: flowID, FlowVersion: 1, Status: status})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return id
}
