package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
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
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionDefer, DecidedBy: decidedByRules})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionKeep, DecidedBy: decidedByLLM})

	decs, err := core.ItemDecisions(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 || decs[0].Decision != decisionDefer || decs[1].Decision != decisionKeep {
		t.Errorf("decisions trail = %+v, want [defer, keep] in order", decs)
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
	prof, found, err := core.InterestProfile(ctx)
	if err != nil || !found || prof.Version != 1 {
		t.Errorf("read back v%d (found=%v): %v", prof.Version, found, err)
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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz should be open, got %d", rec.Code)
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
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionDefer, DecidedBy: decidedByLLM})

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

// --- small helpers --------------------------------------------------------

func mustItem(t *testing.T, db *MockDatabase, lane, ref string, flowID int, status string) int {
	t.Helper()
	id, err := db.UpsertItem(context.Background(), Item{Lane: lane, SourceRef: ref, FlowID: flowID, FlowVersion: 1, Status: status})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return id
}
