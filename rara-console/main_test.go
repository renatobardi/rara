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
		_, _ = w.Write([]byte(`[{"name":"distill-local","capability":"destilar","runtime":"vpc","activation":"resident","enabled":true}]`))
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
	if len(got.Providers) != 1 || got.Providers[0]["name"] != "distill-local" {
		t.Errorf("providers = %+v, want one distill-local provider", got.Providers)
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
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}
	old := maxCoreBytes
	maxCoreBytes = 8 // tiny: the fake flows body is larger than this
	defer func() { maxCoreBytes = old }()

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
		_, _ = w.Write([]byte(`[{"id":1,"capability":"destilar","provider":"distill-local","status":"done","attempts":1}]`))
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
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}
	old := maxCoreBytes
	maxCoreBytes = 1 // even "[]" (2 bytes) exceeds this
	defer func() { maxCoreBytes = old }()

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
	s := &server{coreURL: core.URL, token: "secret", client: core.Client()}
	old := maxCoreBytes
	maxCoreBytes = 1
	defer func() { maxCoreBytes = old }()

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
