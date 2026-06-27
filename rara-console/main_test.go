package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCore stands in for the rara-core surface: it demands the bearer token and serves
// the two read endpoints the overview aggregates. A request without the token gets 401,
// so a successful aggregate proves the console injected the token server-side.
func fakeCore(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/flows", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"youtube","source_type":"youtube","enabled":true,"version":1}]`))
	})
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"distill-vpc","capability":"destilar","runtime":"vpc","activation":"resident","enabled":true}]`))
	})
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOverviewAggregatesFlowsAndProviders(t *testing.T) {
	core := fakeCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Flows     []map[string]any `json:"flows"`
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Flows) != 1 || got.Flows[0]["name"] != "youtube" {
		t.Errorf("flows = %+v, want one youtube flow", got.Flows)
	}
	if len(got.Providers) != 1 || got.Providers[0]["name"] != "distill-vpc" {
		t.Errorf("providers = %+v, want one distill-vpc provider", got.Providers)
	}
}

// The bearer token is a server-side secret; it must never appear in a response served to the SPA.
func TestOverviewNeverLeaksToken(t *testing.T) {
	core := fakeCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

// An oversized core response must fail as a bad gateway, never pass through truncated (which would
// be invalid JSON served to the SPA as a 200).
func TestOverviewRejectsOversizedCoreResponse(t *testing.T) {
	core := fakeCore(t, "secret")
	shrinkMaxBytes(t, 8) // tiny: the fake flows body is larger than this
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass as success)", rec.Code)
	}
}

func TestOverviewReturns502WhenCoreUnreachable(t *testing.T) {
	// A closed server URL: the dial fails, so the aggregate is a bad gateway, not a 200.
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHealthzReportsCoreReachability(t *testing.T) {
	core := fakeCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest("GET", "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["core"] != true {
		t.Errorf("core reachability = %v, want true", got["core"])
	}
}

// fakePipelineCore serves the items endpoints the pipeline and item-steps handlers consume.
func fakePipelineCore(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		st := r.URL.Query().Get("status")
		switch st {
		case "discovered":
			_, _ = w.Write([]byte(`[{"id":1,"title":"Video A","status":"discovered"}]`))
		case "to_text":
			_, _ = w.Write([]byte(`[{"id":2,"title":"Video B","status":"to_text"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})
	mux.HandleFunc("GET /v1/items/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"capability":"destilar","provider":"distill-vpc","status":"done","attempts":1}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPipelineAggregatesAllStatuses(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Counts map[string]int              `json:"counts"`
		Items  map[string][]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.Counts["discovered"] != 1 {
		t.Errorf("counts[discovered] = %d, want 1", got.Counts["discovered"])
	}
	if got.Counts["to_text"] != 1 {
		t.Errorf("counts[to_text] = %d, want 1", got.Counts["to_text"])
	}
	if got.Counts["done"] != 0 {
		t.Errorf("counts[done] = %d, want 0", got.Counts["done"])
	}
	if len(got.Items["discovered"]) != 1 {
		t.Errorf("items[discovered] len = %d, want 1", len(got.Items["discovered"]))
	}
}

func TestPipelineNeverLeaksToken(t *testing.T) {
	core := fakePipelineCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestPipelineRejectsOversizedCoreResponse(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	shrinkMaxBytes(t, 1) // even "[]" (2 bytes) exceeds this
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass as success)", rec.Code)
	}
}

func TestPipelineReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handlePipeline(rec, httptest.NewRequest("GET", "/api/pipeline", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestItemStepsProxiesCorrectly(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0]["capability"] != "destilar" {
		t.Errorf("steps = %+v, want one destilar step", got)
	}
}

func TestItemStepsNeverLeaksToken(t *testing.T) {
	core := fakePipelineCore(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestItemStepsRejectsOversizedCoreResponse(t *testing.T) {
	core := fakePipelineCore(t, "secret")
	shrinkMaxBytes(t, 1)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized response must not pass as success)", rec.Code)
	}
}

func TestItemStepsReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/42/steps", nil)
	req.SetPathValue("id", "42")
	s.handleItemSteps(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestItemStepsRejectsBadID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "", "12x", "../etc", "9999999999999999999999"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/items/"+bad+"/steps", nil)
		req.SetPathValue("id", bad)
		s.handleItemSteps(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- C2: quarantine + distillations + writes ----------------------------------

// fakeC2Core serves the 5 endpoints added in C2 (quarantine reads, distillation
// reads, and the two write endpoints). Every read requires the bearer token.
func fakeC2Core(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	requireBearer := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/quarantine", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"id":7,"status":"quarantine","source_ref":"yt123","lane":"youtube"}]`))
	})
	mux.HandleFunc("GET /v1/distillations", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"source_type":"youtube","source_ref":"yt123","engine":"claude","status":"done"}]`))
	})
	mux.HandleFunc("GET /v1/distillations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"source_type":"youtube","source_ref":"yt123","engine":"claude","status":"done","content":"# Hello"}`))
	})
	mux.HandleFunc("POST /v1/quarantine/review", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		// echo body back so tests can verify forwarding
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST /v1/feedback/distillation", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeC2CoreReturns4xx is a minimal fake that always returns 400 for write endpoints,
// so tests can verify that 4xx from core propagates rather than becoming 502.
func fakeC2CoreReturns4xx(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/quarantine/review", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"item not in quarantine"}`, http.StatusBadRequest)
	})
	mux.HandleFunc("POST /v1/feedback/distillation", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"distillation not found"}`, http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// --- quarantine reads ---

func TestQuarantineProxiesItemsWithBearer(t *testing.T) {
	core := fakeC2Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleQuarantine(rec, httptest.NewRequest("GET", "/api/quarantine", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0]["id"] != float64(7) {
		t.Errorf("quarantine = %+v, want one item id=7", got)
	}
}

func TestQuarantineNeverLeaksToken(t *testing.T) {
	core := fakeC2Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleQuarantine(rec, httptest.NewRequest("GET", "/api/quarantine", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestQuarantineReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleQuarantine(rec, httptest.NewRequest("GET", "/api/quarantine", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// --- distillation list ---

func TestDistillationListProxiesWithBearer(t *testing.T) {
	core := fakeC2Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleDistillationList(rec, httptest.NewRequest("GET", "/api/distillations?limit=10", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0]["source_ref"] != "yt123" {
		t.Errorf("distillations = %+v, want one yt123 item", got)
	}
}

func TestDistillationListForwardsLimitParam(t *testing.T) {
	var gotLimit string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleDistillationList(rec, httptest.NewRequest("GET", "/api/distillations?limit=25", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotLimit != "25" {
		t.Errorf("core received limit=%q, want 25", gotLimit)
	}
}

func TestDistillationListNeverLeaksToken(t *testing.T) {
	core := fakeC2Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleDistillationList(rec, httptest.NewRequest("GET", "/api/distillations", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestDistillationListReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleDistillationList(rec, httptest.NewRequest("GET", "/api/distillations", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// --- distillation detail ---

func TestDistillationDetailProxiesWithBearer(t *testing.T) {
	core := fakeC2Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/distillations/1", nil)
	req.SetPathValue("id", "1")
	s.handleDistillationDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["content"] != "# Hello" {
		t.Errorf("content = %v, want '# Hello'", got["content"])
	}
}

func TestDistillationDetailNeverLeaksToken(t *testing.T) {
	core := fakeC2Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/distillations/1", nil)
	req.SetPathValue("id", "1")
	s.handleDistillationDetail(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestDistillationDetailReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/distillations/1", nil)
	req.SetPathValue("id", "1")
	s.handleDistillationDetail(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestDistillationDetailRejectsBadID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	for _, bad := range []string{"abc", "", "12x", "../etc", "9999999999999999999999"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/distillations/"+bad, nil)
		req.SetPathValue("id", bad)
		s.handleDistillationDetail(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

// --- quarantine review (write) ---

func TestQuarantineReviewForwardsBodyAndBearer(t *testing.T) {
	core := fakeC2Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	payload := `{"item_id":7,"signal":"up"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/quarantine/review", strings.NewReader(payload))
	s.handleQuarantineReview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// fakeC2Core echoes the body back; confirm the payload made it to core
	if body := rec.Body.String(); !contains(body, "item_id") {
		t.Errorf("body = %q; forwarded payload not reflected", body)
	}
}

func TestQuarantineReviewNeverLeaksToken(t *testing.T) {
	core := fakeC2Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/quarantine/review", strings.NewReader(`{"item_id":7,"signal":"up"}`))
	s.handleQuarantineReview(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestQuarantineReviewReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/quarantine/review", strings.NewReader(`{}`))
	s.handleQuarantineReview(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestQuarantineReviewPropagatesCoreError(t *testing.T) {
	core := fakeC2CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/quarantine/review", strings.NewReader(`{"item_id":9,"signal":"up"}`))
	s.handleQuarantineReview(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core error must propagate, not become 502)", rec.Code)
	}
}

// --- feedback distillation (write) ---

func TestFeedbackDistillationForwardsBodyAndBearer(t *testing.T) {
	core := fakeC2Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	payload := `{"distillation_id":"1","signal":"up"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/feedback/distillation", strings.NewReader(payload))
	s.handleFeedbackDistillation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !contains(body, "distillation_id") {
		t.Errorf("body = %q; forwarded payload not reflected", body)
	}
}

func TestFeedbackDistillationNeverLeaksToken(t *testing.T) {
	core := fakeC2Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/feedback/distillation", strings.NewReader(`{"distillation_id":"1","signal":"up"}`))
	s.handleFeedbackDistillation(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestFeedbackDistillationReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/feedback/distillation", strings.NewReader(`{}`))
	s.handleFeedbackDistillation(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestFeedbackDistillationPropagatesCoreError(t *testing.T) {
	core := fakeC2CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/feedback/distillation", strings.NewReader(`{"distillation_id":"99","signal":"up"}`))
	s.handleFeedbackDistillation(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core error must propagate, not become 502)", rec.Code)
	}
}

// --- B1: interest-profile + gate-rules ---

// fakeB1Core serves all 6 B1 endpoints. Reads require the bearer token; writes echo the body back
// so tests can confirm the payload was forwarded.
func fakeB1Core(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	requireBearer := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/interest-profile", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"version":1,"status":"active","narrative":"Tech content"}`))
	})
	mux.HandleFunc("GET /v1/interest-profile/versions", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"version":1,"status":"active"},{"version":2,"status":"proposed"}]`))
	})
	mux.HandleFunc("POST /v1/interest-profile", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST /v1/interest-profile/approve", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /v1/gate-rules", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"action":"allow","match_type":"channel","value":"@tech","enabled":true}]`))
	})
	mux.HandleFunc("PUT /v1/gate-rules", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeB1CoreReturns4xx returns 400 for all B1 write endpoints.
func fakeB1CoreReturns4xx(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/interest-profile", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"version already exists"}`, http.StatusBadRequest)
	})
	mux.HandleFunc("POST /v1/interest-profile/approve", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"not a proposed version"}`, http.StatusBadRequest)
	})
	mux.HandleFunc("PUT /v1/gate-rules", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid action"}`, http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeB1CoreNoProfile serves GET /v1/interest-profile as 404 (cold start, no active profile).
func fakeB1CoreNoProfile(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/interest-profile", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"no active interest_profile"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// shrinkMaxBytes sets maxCoreBytes to n for the duration of the test.
func shrinkMaxBytes(t *testing.T, n int64) {
	t.Helper()
	old := maxCoreBytes
	maxCoreBytes = n
	t.Cleanup(func() { maxCoreBytes = old })
}

// fatCoreServer returns a fake core that always writes 100 x-bytes, closed via t.Cleanup.
func fatCoreServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- interest-profile (active) ---

func TestInterestProfileProxiesActiveWithBearer(t *testing.T) {
	core := fakeB1Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfile(rec, httptest.NewRequest("GET", "/api/interest-profile", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "active" {
		t.Errorf("status = %v, want active", got["status"])
	}
}

func TestInterestProfilePropagates404WhenNoneConfigured(t *testing.T) {
	core := fakeB1CoreNoProfile(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfile(rec, httptest.NewRequest("GET", "/api/interest-profile", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no profile must propagate as 404, not 502)", rec.Code)
	}
}

func TestInterestProfileNeverLeaksToken(t *testing.T) {
	core := fakeB1Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfile(rec, httptest.NewRequest("GET", "/api/interest-profile", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestInterestProfileReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleInterestProfile(rec, httptest.NewRequest("GET", "/api/interest-profile", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestInterestProfileRejectsOversizedCoreResponse(t *testing.T) {
	core := fakeB1Core(t, "secret")
	shrinkMaxBytes(t, 1)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfile(rec, httptest.NewRequest("GET", "/api/interest-profile", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass through)", rec.Code)
	}
}

func TestInterestProfileReturns502WhenCoreReturns5xx(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfile(rec, httptest.NewRequest("GET", "/api/interest-profile", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (5xx from core must not pass through as 200)", rec.Code)
	}
}

// --- interest-profile/versions ---

func TestInterestProfileVersionsProxiesAllWithBearer(t *testing.T) {
	core := fakeB1Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfileVersions(rec, httptest.NewRequest("GET", "/api/interest-profile/versions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("versions len = %d, want 2", len(got))
	}
}

func TestInterestProfileVersionsNeverLeaksToken(t *testing.T) {
	core := fakeB1Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfileVersions(rec, httptest.NewRequest("GET", "/api/interest-profile/versions", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestInterestProfileVersionsReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleInterestProfileVersions(rec, httptest.NewRequest("GET", "/api/interest-profile/versions", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestInterestProfileVersionsReturns502WhenCoreReturnsNon2xx(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(core.Close)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfileVersions(rec, httptest.NewRequest("GET", "/api/interest-profile/versions", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (non-2xx from core must become 502)", rec.Code)
	}
}

func TestInterestProfileVersionsRejectsOversizedCoreResponse(t *testing.T) {
	core := fakeB1Core(t, "secret")
	shrinkMaxBytes(t, 1)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleInterestProfileVersions(rec, httptest.NewRequest("GET", "/api/interest-profile/versions", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass through)", rec.Code)
	}
}

// --- POST /api/interest-profile (propose) ---

func TestProposeInterestProfileForwardsBodyAndBearer(t *testing.T) {
	core := fakeB1Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	payload := `{"version":3,"narrative":"New topics"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile", strings.NewReader(payload))
	s.handleProposeInterestProfile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !contains(body, "narrative") {
		t.Errorf("body = %q; forwarded payload not reflected", body)
	}
}

func TestProposeInterestProfileNeverLeaksToken(t *testing.T) {
	core := fakeB1Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile", strings.NewReader(`{"version":3}`))
	s.handleProposeInterestProfile(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestProposeInterestProfileReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile", strings.NewReader(`{}`))
	s.handleProposeInterestProfile(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestProposeInterestProfilePropagatesCoreError(t *testing.T) {
	core := fakeB1CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile", strings.NewReader(`{"version":1}`))
	s.handleProposeInterestProfile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core error must propagate, not become 502)", rec.Code)
	}
}

func TestProposeInterestProfileRejectsOversizedRequestBody(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}
	shrinkMaxBytes(t, 1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile", strings.NewReader("{}")) // 2 bytes > maxCoreBytes=1
	s.handleProposeInterestProfile(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized request body must be rejected before reaching core)", rec.Code)
	}
}

func TestProposeInterestProfileRejectsOversizedCoreResponse(t *testing.T) {
	core := fatCoreServer(t)
	shrinkMaxBytes(t, 2)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile", strings.NewReader("{}")) // 2 bytes = ok
	s.handleProposeInterestProfile(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass through)", rec.Code)
	}
}

// --- POST /api/interest-profile/approve ---

func TestApproveInterestProfileForwardsBodyAndBearer(t *testing.T) {
	core := fakeB1Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	payload := `{"version":2}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile/approve", strings.NewReader(payload))
	s.handleApproveInterestProfile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !contains(body, "version") {
		t.Errorf("body = %q; forwarded payload not reflected", body)
	}
}

func TestApproveInterestProfileNeverLeaksToken(t *testing.T) {
	core := fakeB1Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile/approve", strings.NewReader(`{"version":2}`))
	s.handleApproveInterestProfile(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestApproveInterestProfileReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile/approve", strings.NewReader(`{"version":2}`))
	s.handleApproveInterestProfile(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestApproveInterestProfilePropagatesCoreError(t *testing.T) {
	core := fakeB1CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile/approve", strings.NewReader(`{"version":99}`))
	s.handleApproveInterestProfile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core error must propagate, not become 502)", rec.Code)
	}
}

func TestApproveInterestProfileRejectsOversizedRequestBody(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}
	shrinkMaxBytes(t, 1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile/approve", strings.NewReader("{}"))
	s.handleApproveInterestProfile(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized request body must be rejected before reaching core)", rec.Code)
	}
}

func TestApproveInterestProfileRejectsOversizedCoreResponse(t *testing.T) {
	core := fatCoreServer(t)
	shrinkMaxBytes(t, 2)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/interest-profile/approve", strings.NewReader("{}"))
	s.handleApproveInterestProfile(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass through)", rec.Code)
	}
}

// --- GET /api/gate-rules ---

func TestGateRulesProxiesWithBearer(t *testing.T) {
	core := fakeB1Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleGateRules(rec, httptest.NewRequest("GET", "/api/gate-rules", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0]["action"] != "allow" {
		t.Errorf("gate-rules = %+v, want one allow rule", got)
	}
}

func TestGateRulesNeverLeaksToken(t *testing.T) {
	core := fakeB1Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleGateRules(rec, httptest.NewRequest("GET", "/api/gate-rules", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestGateRulesReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleGateRules(rec, httptest.NewRequest("GET", "/api/gate-rules", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestGateRulesRejectsOversizedCoreResponse(t *testing.T) {
	core := fakeB1Core(t, "secret")
	shrinkMaxBytes(t, 1)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleGateRules(rec, httptest.NewRequest("GET", "/api/gate-rules", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass through)", rec.Code)
	}
}

// --- PUT /api/gate-rules ---

func TestUpsertGateRuleForwardsBodyAndBearer(t *testing.T) {
	core := fakeB1Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	payload := `{"action":"deny","match_type":"title_contains","value":"spam","enabled":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/gate-rules", strings.NewReader(payload))
	s.handleUpsertGateRule(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !contains(body, "action") {
		t.Errorf("body = %q; forwarded payload not reflected", body)
	}
}

func TestUpsertGateRuleNeverLeaksToken(t *testing.T) {
	core := fakeB1Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/gate-rules", strings.NewReader(`{"action":"allow","match_type":"channel","value":"@tech","enabled":true}`))
	s.handleUpsertGateRule(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestUpsertGateRuleReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/gate-rules", strings.NewReader(`{}`))
	s.handleUpsertGateRule(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestUpsertGateRulePropagatesCoreError(t *testing.T) {
	core := fakeB1CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/gate-rules", strings.NewReader(`{"action":"invalid"}`))
	s.handleUpsertGateRule(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (core error must propagate, not become 502)", rec.Code)
	}
}

func TestUpsertGateRuleRejectsOversizedRequestBody(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}
	shrinkMaxBytes(t, 1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/gate-rules", strings.NewReader("{}"))
	s.handleUpsertGateRule(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized request body must be rejected before reaching core)", rec.Code)
	}
}

func TestUpsertGateRuleRejectsOversizedCoreResponse(t *testing.T) {
	core := fatCoreServer(t)
	shrinkMaxBytes(t, 2)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/gate-rules", strings.NewReader("{}"))
	s.handleUpsertGateRule(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (oversized core response must not pass through)", rec.Code)
	}
}

// --- C4: providers, routing-policies, decisions feed, item decisions ---

func fakeC4Core(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	requireBearer := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"name":"distill-vpc","capability":"destilar","runtime":"vpc","activation":"resident","enabled":true}]`))
	})
	mux.HandleFunc("GET /v1/workers", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"name":"distill","capability":"destilar","placements":[{"name":"distill-vpc","capability":"destilar","runtime":"vpc","activation":"resident","enabled":true}]}]`))
	})
	mux.HandleFunc("PUT /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /v1/routing-policies", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"scope":"global","fallback":[]}]`))
	})
	mux.HandleFunc("PUT /v1/routing-policies", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /v1/decisions", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"item_id":7,"gate":"gate_barato","decision":"keep","decided_by":"rules","reason":"matched rule","when":"2026-01-01T00:00:00Z"}]`))
	})
	mux.HandleFunc("GET /v1/items/{id}/decisions", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"item_id":7,"gate":"gate_barato","decision":"keep","decided_by":"rules"}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fakeC4CoreReturns4xx(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/providers", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid provider"}`, http.StatusBadRequest)
	})
	mux.HandleFunc("PUT /v1/routing-policies", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid policy"}`, http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkersProxiesWithBearer(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleWorkers(rec, httptest.NewRequest("GET", "/api/workers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	// Expect grouped Worker→placements shape (from /v1/workers, not flat /v1/providers).
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["name"] != "distill" {
		t.Errorf("workers = %+v, want one 'distill' worker group", got)
	}
	placements, ok := got[0]["placements"].([]any)
	if !ok || len(placements) != 1 {
		t.Errorf("placements = %v, want one entry", got[0]["placements"])
	}
}

func TestWorkersNeverLeaksToken(t *testing.T) {
	core := fakeC4Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleWorkers(rec, httptest.NewRequest("GET", "/api/workers", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestWorkersReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleWorkers(rec, httptest.NewRequest("GET", "/api/workers", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rec.Code)
	}
}

func TestPlacementsProxiesWithBearer(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePlacementsFlat(rec, httptest.NewRequest("GET", "/api/placements", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	// Flat list from /v1/providers — individual placement names.
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["name"] != "distill-vpc" {
		t.Errorf("placements = %+v, want one distill-vpc placement", got)
	}
}

func TestPlacementsNeverLeaksToken(t *testing.T) {
	core := fakeC4Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handlePlacementsFlat(rec, httptest.NewRequest("GET", "/api/placements", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked the surface token: %s", body)
	}
}

func TestPlacementsReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handlePlacementsFlat(rec, httptest.NewRequest("GET", "/api/placements", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rec.Code)
	}
}

func TestUpsertPlacementProxiesPut(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	body := strings.NewReader(`{"name":"test","capability":"destilar","runtime":"vpc","activation":"resident","cost":1,"quality":0.9,"latency_ms":200,"enabled":true}`)
	rec := httptest.NewRecorder()
	s.handleUpsertPlacement(rec, httptest.NewRequest("PUT", "/api/placements", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
}

func TestUpsertPlacementPropagates4xx(t *testing.T) {
	core := fakeC4CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleUpsertPlacement(rec, httptest.NewRequest("PUT", "/api/placements", strings.NewReader(`{"name":"bad"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

func TestRoutingPoliciesProxiesWithBearer(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleRoutingPolicies(rec, httptest.NewRequest("GET", "/api/routing-policies", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["scope"] != "global" {
		t.Errorf("policies = %+v, want one global policy", got)
	}
}

func TestRoutingPoliciesNeverLeaksToken(t *testing.T) {
	core := fakeC4Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleRoutingPolicies(rec, httptest.NewRequest("GET", "/api/routing-policies", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked token: %s", body)
	}
}

func TestRoutingPoliciesReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleRoutingPolicies(rec, httptest.NewRequest("GET", "/api/routing-policies", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rec.Code)
	}
}

func TestUpsertRoutingPolicyProxiesPut(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	body := strings.NewReader(`{"scope":"global","fallback":[]}`)
	rec := httptest.NewRecorder()
	s.handleUpsertRoutingPolicy(rec, httptest.NewRequest("PUT", "/api/routing-policies", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
}

func TestUpsertRoutingPolicyPropagates4xx(t *testing.T) {
	core := fakeC4CoreReturns4xx(t)
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleUpsertRoutingPolicy(rec, httptest.NewRequest("PUT", "/api/routing-policies", strings.NewReader(`{"scope":"bad"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

func TestDecisionsFeedProxiesWithBearer(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleDecisionsFeed(rec, httptest.NewRequest("GET", "/api/decisions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["gate"] != "gate_barato" {
		t.Errorf("decisions = %+v, want one gate_barato", got)
	}
}

func TestDecisionsFeedForwardsLimitParam(t *testing.T) {
	var capturedLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	s := &server{coreURL: srv.URL, token: "t", client: srv.Client()}

	s.handleDecisionsFeed(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/decisions?limit=10", nil))

	if capturedLimit != "10" {
		t.Errorf("limit forwarded = %q, want 10", capturedLimit)
	}
}

func TestDecisionsFeedNeverLeaksToken(t *testing.T) {
	core := fakeC4Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	rec := httptest.NewRecorder()
	s.handleDecisionsFeed(rec, httptest.NewRequest("GET", "/api/decisions", nil))

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked token: %s", body)
	}
}

func TestDecisionsFeedReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleDecisionsFeed(rec, httptest.NewRequest("GET", "/api/decisions", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rec.Code)
	}
}

func TestDecisionsFeedRejectsNonNumericLimit(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	s.handleDecisionsFeed(rec, httptest.NewRequest("GET", "/api/decisions?limit=abc", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for non-numeric limit", rec.Code)
	}
}

func TestItemDecisionsProxiesWithBearer(t *testing.T) {
	core := fakeC4Core(t, "secret")
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}

	req := httptest.NewRequest("GET", "/api/items/7/decisions", nil)
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	s.handleItemDecisions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("item decisions = %+v, want 1", got)
	}
}

func TestItemDecisionsNeverLeaksToken(t *testing.T) {
	core := fakeC4Core(t, "supersecret")
	s := &server{coreURL: core.URL, token: "supersecret", client: core.Client()}

	req := httptest.NewRequest("GET", "/api/items/7/decisions", nil)
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	s.handleItemDecisions(rec, req)

	if body := rec.Body.String(); contains(body, "supersecret") {
		t.Errorf("response leaked token: %s", body)
	}
}

func TestItemDecisionsReturns502WhenCoreUnreachable(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	req := httptest.NewRequest("GET", "/api/items/7/decisions", nil)
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	s.handleItemDecisions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rec.Code)
	}
}

func TestItemDecisionsRejectsBadID(t *testing.T) {
	s := &server{coreURL: "http://127.0.0.1:1", token: "secret", client: http.DefaultClient}

	req := httptest.NewRequest("GET", "/api/items/not-a-number/decisions", nil)
	req.SetPathValue("id", "not-a-number")
	rec := httptest.NewRecorder()
	s.handleItemDecisions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// TestDecisionsFeedExposesDecidedByAndReason verifies the BFF passes decided_by and reason
// from core verbatim — closing the gap from #2 where these fields were added to /v1/decisions.
func TestDecisionsFeedExposesDecidedByAndReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"item_id":7,"gate":"gate_barato","decision":"keep","decided_by":"rules","reason":"matched allow-list rule","when":"2026-01-01T00:00:00Z"}]`))
	}))
	defer srv.Close()
	s := &server{coreURL: srv.URL, token: "t", client: srv.Client()}

	rec := httptest.NewRecorder()
	s.handleDecisionsFeed(rec, httptest.NewRequest("GET", "/api/decisions", nil))

	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 decision, got %d", len(got))
	}
	if got[0]["decided_by"] != "rules" {
		t.Errorf("decided_by = %v, want rules", got[0]["decided_by"])
	}
	if got[0]["reason"] != "matched allow-list rule" {
		t.Errorf("reason = %v, want matched allow-list rule", got[0]["reason"])
	}
}

// TestDecisionsFeedNullReasonPassedThrough verifies that when core returns reason:null
// the BFF passes it as JSON null, not as a string "null" or empty string.
func TestDecisionsFeedNullReasonPassedThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"item_id":7,"gate":"gate_barato","decision":"drop","decided_by":"profile","reason":null,"when":"2026-01-01T00:00:00Z"}]`))
	}))
	defer srv.Close()
	s := &server{coreURL: srv.URL, token: "t", client: srv.Client()}

	rec := httptest.NewRecorder()
	s.handleDecisionsFeed(rec, httptest.NewRequest("GET", "/api/decisions", nil))

	// The raw body must not contain the string literal "null" for reason as a JSON string.
	body := rec.Body.String()
	if contains(body, `"reason":"null"`) || contains(body, `"reason":""`) {
		t.Errorf("reason was coerced to string: %s", body)
	}
	// Verify reason is actually JSON null in the payload.
	var got []map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 decision, got %d", len(got))
	}
	v, ok := got[0]["reason"]
	if !ok {
		t.Fatal("reason field missing from response; want explicit JSON null")
	}
	if v != nil {
		t.Errorf("reason = %v, want nil", v)
	}
}

func TestCoreHealthProxiesWithBearer(t *testing.T) {
	const tok = "health-tok"
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"db_ok":true,"providers":{"total":3,"enabled":2,"stale":0}}`))
	}))
	defer core.Close()
	s := &server{coreURL: core.URL, token: tok, client: http.DefaultClient}

	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	s.handleCoreHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["db_ok"] != true {
		t.Errorf("db_ok = %v, want true", body["db_ok"])
	}
}

func TestCoreUsageProxiesWithBearer(t *testing.T) {
	const tok = "usage-tok"
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"lane":"youtube","status":"discovered","count":5}],"item_steps":[],"distillations":10,"quarantine":2}`))
	}))
	defer core.Close()
	s := &server{coreURL: core.URL, token: tok, client: http.DefaultClient}

	req := httptest.NewRequest("GET", "/api/usage", nil)
	rec := httptest.NewRecorder()
	s.handleCoreUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	gotDist, ok := body["distillations"].(float64)
	if !ok {
		t.Fatalf("distillations type unexpected: %T (value=%v)", body["distillations"], body["distillations"])
	}
	if gotDist != 10 {
		t.Errorf("distillations = %v, want 10", gotDist)
	}
}
