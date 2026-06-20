// rara-console — the operator/curator UI for the rara 2.0 control plane.
//
// One Go binary: it serves the SvelteKit SPA (embedded via embed.FS, so production is a single
// artifact, no Node at runtime) and acts as a thin BFF in front of the rara-core surface. The
// console holds the surface bearer token SERVER-SIDE and proxies/aggregates reads — the SPA never
// sees the token, and the console never touches Neon directly (rara-core is the single source of
// truth). It binds to the tailnet interface only (CONSOLE_ADDR), never the public Oracle IP.
//
// C0 scope: the shell + the "Visão geral" screen, which calls /api/overview — a real aggregate of
// the live surface (flows + providers) that proves the BFF end to end.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed all:web/build
var embedded embed.FS

// maxCoreBytes caps a single core-surface response. Far above any seeded config, but a backstop
// against an unbounded body — exceeding it is an error, never a silent truncation served as 200.
// A var (not const) so tests can shrink it.
var maxCoreBytes int64 = 4 << 20

// server is the BFF: it talks to the rara-core surface at coreURL, authenticating with the
// server-side token. client is injected so handlers are unit-testable against an httptest core.
type server struct {
	coreURL string
	token   string
	client  *http.Client
}

// fetchCoreWithStatus does an authenticated GET and returns the raw status + body without a 2xx
// check. Only transport failures are errors; a 4xx/5xx from core is a (status, body, nil) triple
// so the caller can decide whether to pass it through or convert it to 502.
func (s *server) fetchCoreWithStatus(ctx context.Context, path string) (int, json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.coreURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCoreBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(body)) > maxCoreBytes {
		return 0, nil, fmt.Errorf("core surface response exceeds %d-byte limit", maxCoreBytes)
	}
	return resp.StatusCode, body, nil
}

// fetchCore does an authenticated GET against the surface and returns the raw JSON body. A
// transport failure or any non-2xx status is an error — the caller maps it to 502 (bad gateway).
func (s *server) fetchCore(ctx context.Context, path string) (json.RawMessage, error) {
	status, body, err := s.fetchCoreWithStatus(ctx, path)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &gatewayError{status: status}
	}
	return body, nil
}

type gatewayError struct{ status int }

func (e *gatewayError) Error() string {
	return fmt.Sprintf("core surface returned status %d %s", e.status, http.StatusText(e.status))
}

// handleOverview aggregates the two seeded reads the Visão geral needs into one response, so the
// SPA makes a single request and never learns the surface URL or token.
func (s *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	flows, err := s.fetchCore(r.Context(), "/v1/flows")
	if err != nil {
		badGateway(w, err)
		return
	}
	providers, err := s.fetchCore(r.Context(), "/v1/providers")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Flows     json.RawMessage `json:"flows"`
		Providers json.RawMessage `json:"providers"`
	}{flows, providers})
}

// pipelineStatuses is the exhaustive ordered set of item lifecycle statuses. Must match
// isValidItemStatus in rara-core; do not invent values outside this list.
var pipelineStatuses = [7]string{"discovered", "to_text", "distilled", "done", "filtered", "quarantine", "failed"}

// handlePipeline fetches all 7 lifecycle statuses in parallel and returns counts + items so the
// SPA can render the full pipeline view in a single round-trip. Any per-status failure is a 502.
func (s *server) handlePipeline(w http.ResponseWriter, r *http.Request) {
	type result struct {
		status string
		body   json.RawMessage
		err    error
	}
	ch := make(chan result, len(pipelineStatuses))
	for _, st := range pipelineStatuses {
		go func(st string) {
			body, err := s.fetchCore(r.Context(), "/v1/items?status="+st)
			ch <- result{st, body, err}
		}(st)
	}

	// Drain all goroutines before processing: guarantees every goroutine has
	// finished (and stopped reading shared state) before this handler returns.
	all := make([]result, len(pipelineStatuses))
	for i := range all {
		all[i] = <-ch
	}

	counts := make(map[string]int, len(pipelineStatuses))
	items := make(map[string]json.RawMessage, len(pipelineStatuses))
	for _, res := range all {
		if res.err != nil {
			badGateway(w, res.err)
			return
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(res.body, &arr); err != nil {
			badGateway(w, err)
			return
		}
		counts[res.status] = len(arr)
		items[res.status] = res.body
	}
	writeJSON(w, http.StatusOK, struct {
		Counts map[string]int             `json:"counts"`
		Items  map[string]json.RawMessage `json:"items"`
	}{counts, items})
}

// handleItemSteps proxies /v1/items/{id}/steps from the core surface. Rejects non-numeric ids
// before touching the core so a path-traversal payload never reaches the upstream.
func (s *server) handleItemSteps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/items/"+id+"/steps")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

func isNumericID(s string) bool {
	if s == "" || len(s) > 19 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// doCore sends an authenticated request with a body (POST, PUT, …) against the surface. It returns
// the HTTP status and raw body from core — 4xx responses propagate to the caller so the SPA can
// surface core validation errors. Only transport failures are returned as errors (caller → 502).
// The request body is read into a buffer first so an oversized payload is rejected before it
// reaches core, rather than silently truncated (which would cause a confusing 4xx from core).
func (s *server) doCore(ctx context.Context, method, path string, reqBody io.Reader) (status int, body []byte, err error) {
	buf, err := io.ReadAll(io.LimitReader(reqBody, maxCoreBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(buf)) > maxCoreBytes {
		return 0, nil, fmt.Errorf("request body exceeds %d-byte limit", maxCoreBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.coreURL+path, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxCoreBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(b)) > maxCoreBytes {
		return 0, nil, fmt.Errorf("core surface response exceeds %d-byte limit", maxCoreBytes)
	}
	return resp.StatusCode, b, nil
}

func (s *server) postCore(ctx context.Context, path string, reqBody io.Reader) (int, []byte, error) {
	return s.doCore(ctx, http.MethodPost, path, reqBody)
}

func (s *server) putCore(ctx context.Context, path string, reqBody io.Reader) (int, []byte, error) {
	return s.doCore(ctx, http.MethodPut, path, reqBody)
}

// handleQuarantine proxies GET /v1/quarantine — the cold-start quarantine review queue.
func (s *server) handleQuarantine(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/quarantine")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleDistillationList proxies GET /v1/distillations, forwarding the optional ?limit= param.
// The limit value is re-encoded via url.Values to prevent query-parameter injection.
func (s *server) handleDistillationList(w http.ResponseWriter, r *http.Request) {
	path := "/v1/distillations"
	if limit := r.URL.Query().Get("limit"); limit != "" {
		q := url.Values{}
		q.Set("limit", limit)
		path += "?" + q.Encode()
	}
	body, err := s.fetchCore(r.Context(), path)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleDistillationDetail proxies GET /v1/distillations/{id}. Rejects non-numeric ids before
// touching the core so a path-traversal payload never reaches the upstream.
func (s *server) handleDistillationDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid distillation id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/distillations/"+id)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleQuarantineReview proxies POST /v1/quarantine/review with the bearer injected server-side.
// Core 4xx (e.g. item not in quarantine) propagates; only transport errors become 502.
func (s *server) handleQuarantineReview(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/quarantine/review", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleFeedbackDistillation proxies POST /v1/feedback/distillation with bearer injected.
// Core 4xx propagates; transport errors become 502.
func (s *server) handleFeedbackDistillation(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/feedback/distillation", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleInterestProfile proxies GET /v1/interest-profile. Unlike most GET proxies, a 404 from
// core (no active profile) is passed through rather than mapped to 502, so the SPA can render an
// "empty" state instead of an error on cold start.
func (s *server) handleInterestProfile(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.fetchCoreWithStatus(r.Context(), "/v1/interest-profile")
	if err != nil {
		badGateway(w, err)
		return
	}
	if (status >= 200 && status < 300) || status == http.StatusNotFound {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	badGateway(w, &gatewayError{status: status})
}

// handleInterestProfileVersions proxies GET /v1/interest-profile/versions — all versions
// (active + proposed + superseded) so the operator can see pending proposals and approve them.
func (s *server) handleInterestProfileVersions(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/interest-profile/versions")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleProposeInterestProfile proxies POST /v1/interest-profile (propose a new version).
// Core 4xx (duplicate version, bad fields) propagates; transport errors become 502.
func (s *server) handleProposeInterestProfile(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/interest-profile", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleApproveInterestProfile proxies POST /v1/interest-profile/approve (human approval).
// Core 4xx (version not proposed) propagates; transport errors become 502.
func (s *server) handleApproveInterestProfile(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/interest-profile/approve", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleGateRules proxies GET /v1/gate-rules — all allow/deny rules (enabled and disabled).
func (s *server) handleGateRules(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/gate-rules")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertGateRule proxies PUT /v1/gate-rules (full-record upsert of one rule).
// Core 4xx (invalid action/match_type) propagates; transport errors become 502.
func (s *server) handleUpsertGateRule(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/gate-rules", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleProviders proxies GET /v1/providers — list of all providers (config-as-data).
func (s *server) handleProviders(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/providers")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertProvider proxies PUT /v1/providers with bearer injected server-side.
// Core 4xx (e.g. invalid enum) propagates; only transport errors become 502.
func (s *server) handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/providers", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleRoutingPolicies proxies GET /v1/routing-policies — list of all policies.
func (s *server) handleRoutingPolicies(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/routing-policies")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertRoutingPolicy proxies PUT /v1/routing-policies with bearer injected.
// Core 4xx propagates; transport errors become 502.
func (s *server) handleUpsertRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/routing-policies", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleDecisionsFeed proxies GET /v1/decisions, forwarding the optional ?limit= param.
// Validates limit is numeric (non-numeric → 400); core clamps the value to 1-200.
func (s *server) handleDecisionsFeed(w http.ResponseWriter, r *http.Request) {
	path := "/v1/decisions"
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if _, err := strconv.Atoi(raw); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a number"})
			return
		}
		q := url.Values{}
		q.Set("limit", raw)
		path += "?" + q.Encode()
	}
	body, err := s.fetchCore(r.Context(), path)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleItemDecisions proxies GET /v1/items/{id}/decisions. Rejects non-numeric ids.
func (s *server) handleItemDecisions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/items/"+id+"/decisions")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// --- Fontes & Flows --------------------------------------------------------
// Fontes (podcast feeds) and Flows are both control-plane config the core surface owns. The BFF
// proxies them with the bearer injected server-side; reads map any core failure to 502, writes
// propagate core 4xx so the SPA can show validation errors.

// handlePodcastSources proxies GET /v1/sources/podcast — the operator-curated podcast feed list.
func (s *server) handlePodcastSources(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/sources/podcast")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleAddPodcastSource proxies POST /v1/sources/podcast (add a feed). Core 4xx (empty feed_url)
// propagates; transport errors become 502.
func (s *server) handleAddPodcastSource(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/sources/podcast", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleTogglePodcastSource proxies PUT /v1/sources/podcast (toggle active). Core 4xx propagates;
// transport errors become 502.
func (s *server) handleTogglePodcastSource(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/sources/podcast", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleFlows proxies GET /v1/flows — every flow (lane) as config-as-data.
func (s *server) handleFlows(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/flows")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleFlowSteps proxies GET /v1/flows/{id}/steps. Rejects non-numeric ids before touching core.
func (s *server) handleFlowSteps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid flow id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/flows/"+id+"/steps")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertFlow proxies PUT /v1/flows (toggle enabled / edit a lane). Core 4xx propagates.
func (s *server) handleUpsertFlow(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/flows", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleUpsertFlowStep proxies PUT /v1/flow-steps (edit one step). Core 4xx propagates.
func (s *server) handleUpsertFlowStep(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/flow-steps", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// parseFlowStepIDs extracts and validates the flow_id and seq path values.
// Writes 400 and returns ok=false when either value is non-numeric.
func parseFlowStepIDs(w http.ResponseWriter, r *http.Request) (flowID, seq string, ok bool) {
	flowID = r.PathValue("flow_id")
	seq = r.PathValue("seq")
	if !isNumericID(flowID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid flow_id"})
		return "", "", false
	}
	if !isNumericID(seq) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid seq"})
		return "", "", false
	}
	return flowID, seq, true
}

// handleStepHosts proxies GET /v1/flows/{flow_id}/steps/{seq}/hosts — per-step host priority.
// Core 4xx/404 responses are propagated as-is; transport failures become 502.
func (s *server) handleStepHosts(w http.ResponseWriter, r *http.Request) {
	flowID, seq, ok := parseFlowStepIDs(w, r)
	if !ok {
		return
	}
	status, body, err := s.fetchCoreWithStatus(r.Context(), "/v1/flows/"+flowID+"/steps/"+seq+"/hosts")
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleSetStepHosts proxies PUT /v1/flows/{flow_id}/steps/{seq}/hosts. Core 4xx propagates.
func (s *server) handleSetStepHosts(w http.ResponseWriter, r *http.Request) {
	flowID, seq, ok := parseFlowStepIDs(w, r)
	if !ok {
		return
	}
	status, body, err := s.putCore(r.Context(), "/v1/flows/"+flowID+"/steps/"+seq+"/hosts", r.Body)
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("console: write step hosts response: %v", err)
	}
}

// handleRoutePreview proxies GET /v1/route/preview — dry-run of the router without dispatching a
// job. Requires capability; optionally forwards lane, sensitivity, and exclude (multi-value).
func (s *server) handleRoutePreview(w http.ResponseWriter, r *http.Request) {
	capability := strings.TrimSpace(r.URL.Query().Get("capability"))
	if capability == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "capability is required"})
		return
	}
	q := url.Values{}
	q.Set("capability", capability)
	if v := r.URL.Query().Get("lane"); v != "" {
		q.Set("lane", v)
	}
	if v := r.URL.Query().Get("sensitivity"); v != "" {
		q.Set("sensitivity", v)
	}
	for _, ex := range r.URL.Query()["exclude"] {
		q.Add("exclude", ex)
	}
	body, err := s.fetchCore(r.Context(), "/v1/route/preview?"+q.Encode())
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleWorkerMetrics proxies GET /v1/workers/metrics — per-worker step rollup for the dashboard
// cards. Optionally forwards days=N (1–365); absent means all-time.
func (s *server) handleWorkerMetrics(w http.ResponseWriter, r *http.Request) {
	path := "/v1/workers/metrics"
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 365 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be an integer between 1 and 365"})
			return
		}
		q := url.Values{}
		q.Set("days", strconv.Itoa(n))
		path += "?" + q.Encode()
	}
	body, err := s.fetchCore(r.Context(), path)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleCoreHealth proxies GET /v1/health — the system health aggregate (db_ok, last reconcile,
// provider staleness). Always 200 from core; transport failures become 502.
func (s *server) handleCoreHealth(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/health")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleCoreUsage proxies GET /v1/usage — exact COUNT(*) GROUP BY aggregates for items,
// item_steps, and distillations. Transport failures become 502.
func (s *server) handleCoreUsage(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/usage")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleHealthz is the console's own liveness probe; it also reports whether the core surface is
// reachable so a deploy can confirm the BFF link is live. It is always 200 (the console is up).
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.fetchCore(ctx, "/live")
	if err != nil {
		log.Printf("console: core /live unreachable: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"console": true, "core": err == nil})
}

func badGateway(w http.ResponseWriter, err error) {
	log.Printf("console: core surface unreachable: %v", err) // NOSONAR — err is from internal HTTP client, not user-supplied data
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "core surface unreachable"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("console: encode response: %v", err)
	}
}

func main() {
	s := &server{
		coreURL: mustEnv("CORE_SURFACE_URL"), // e.g. http://100.x.x.x:8080
		token:   mustEnv("SURFACE_TOKEN"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	addr := mustEnv("CONSOLE_ADDR") // tailnet IP only, e.g. 100.x.x.x:8081 — never 0.0.0.0

	dist, err := fs.Sub(embedded, "web/build")
	if err != nil {
		log.Fatalf("console: embedded SPA: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/route/preview", s.handleRoutePreview)
	mux.HandleFunc("GET /api/workers/metrics", s.handleWorkerMetrics)
	mux.HandleFunc("GET /api/health", s.handleCoreHealth)
	mux.HandleFunc("GET /api/usage", s.handleCoreUsage)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/pipeline", s.handlePipeline)
	mux.HandleFunc("GET /api/items/{id}/steps", s.handleItemSteps)
	mux.HandleFunc("GET /api/quarantine", s.handleQuarantine)
	mux.HandleFunc("GET /api/distillations", s.handleDistillationList)
	mux.HandleFunc("GET /api/distillations/{id}", s.handleDistillationDetail)
	mux.HandleFunc("POST /api/quarantine/review", s.handleQuarantineReview)
	mux.HandleFunc("POST /api/feedback/distillation", s.handleFeedbackDistillation)
	mux.HandleFunc("GET /api/interest-profile", s.handleInterestProfile)
	mux.HandleFunc("GET /api/interest-profile/versions", s.handleInterestProfileVersions)
	mux.HandleFunc("POST /api/interest-profile", s.handleProposeInterestProfile)
	mux.HandleFunc("POST /api/interest-profile/approve", s.handleApproveInterestProfile)
	mux.HandleFunc("GET /api/gate-rules", s.handleGateRules)
	mux.HandleFunc("PUT /api/gate-rules", s.handleUpsertGateRule)
	mux.HandleFunc("GET /api/providers", s.handleProviders)
	mux.HandleFunc("PUT /api/providers", s.handleUpsertProvider)
	mux.HandleFunc("GET /api/routing-policies", s.handleRoutingPolicies)
	mux.HandleFunc("PUT /api/routing-policies", s.handleUpsertRoutingPolicy)
	mux.HandleFunc("GET /api/decisions", s.handleDecisionsFeed)
	mux.HandleFunc("GET /api/items/{id}/decisions", s.handleItemDecisions)
	mux.HandleFunc("GET /api/sources/podcast", s.handlePodcastSources)
	mux.HandleFunc("POST /api/sources/podcast", s.handleAddPodcastSource)
	mux.HandleFunc("PUT /api/sources/podcast", s.handleTogglePodcastSource)
	mux.HandleFunc("GET /api/flows", s.handleFlows)
	mux.HandleFunc("GET /api/flows/{id}/steps", s.handleFlowSteps)
	mux.HandleFunc("GET /api/flows/{flow_id}/steps/{seq}/hosts", s.handleStepHosts)
	mux.HandleFunc("PUT /api/flows/{flow_id}/steps/{seq}/hosts", s.handleSetStepHosts)
	mux.HandleFunc("PUT /api/flows", s.handleUpsertFlow)
	mux.HandleFunc("PUT /api/flow-steps", s.handleUpsertFlowStep)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /", spaHandler(dist))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("rara-console: listening on %s (core=%s)", addr, s.coreURL)
	log.Fatal(srv.ListenAndServe())
}

// spaHandler serves the embedded static build, falling back to index.html for any unknown path so
// client-side routing works (adapter-static SPA fallback).
func spaHandler(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(dist, trimSlash(r.URL.Path)); err != nil {
			r.URL.Path = "/" // unknown route -> index.html
		}
		files.ServeHTTP(w, r)
	})
}

func trimSlash(p string) string {
	if p == "/" || p == "" {
		return "index.html"
	}
	return p[1:]
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("console: %s is required", k)
	}
	return v
}
