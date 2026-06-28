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

// handleItemContent proxies GET /v1/items/{id}/content — the mega-thumbnail content
// endpoint. Rejects non-numeric ids before forwarding to core.
func (s *server) handleItemContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/items/"+id+"/content")
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

// proxyWrite relays a doCore result to the client: a transport failure → 502, otherwise the
// upstream status + body verbatim (so core 4xx validation reaches the SPA).
func proxyWrite(w http.ResponseWriter, status int, body []byte, err error) {
	if err != nil {
		badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
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

// sourceListParams are the only query params forwarded to /v1/sources. Re-encoding through this
// allowlist means a crafted query (e.g. ?evil=1) can never reach the upstream surface.
var sourceListParams = [7]string{"kind", "status", "q", "page", "page_size", "sort_by", "sort_dir"}

// handleSources proxies GET /v1/sources (the unified sources_v read-model), forwarding only the
// whitelisted filter/pagination params. The values are re-encoded via url.Values to prevent
// query-parameter injection.
func (s *server) handleSources(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range sourceListParams {
		v := r.URL.Query().Get(k)
		if v == "" {
			continue
		}
		if k == "page" || k == "page_size" {
			// Forward pagination only when it parses as a positive int — garbage like ?page=abc
			// never reaches the upstream. The core surface owns the page_size ceiling
			// (maxSourcePageSize), so we deliberately don't duplicate that cap here.
			if n, err := strconv.Atoi(v); err != nil || n <= 0 {
				continue
			}
		}
		q.Set(k, v)
	}
	path := "/v1/sources"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	body, err := s.fetchCore(r.Context(), path)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleSourceKinds proxies GET /v1/source-kinds — the static registry that drives the wizard and
// labels/icons each kind in the list.
func (s *server) handleSourceKinds(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/source-kinds")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// --- Fontes CRUD writes (fatia #4) -----------------------------------------
// Write proxies for the unified source surface. The bearer is injected server-side; a core 4xx
// (validation) propagates so the wizard can show field errors, transport failures become 502.
// The {kind} and {source_id} path segments are validated before they're concatenated into the
// upstream URL so a crafted value can't traverse to another surface path.

// isSourceKindToken reports whether s is a safe source-kind token ([a-z_]+). The core owns the
// authoritative allow-list; this only guards the URL path against injection.
func isSourceKindToken(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

// isSourceID reports whether s is a well-formed api_id (kind:N), matching the core's composite id.
func isSourceID(s string) bool {
	k, n, ok := strings.Cut(s, ":")
	return ok && isSourceKindToken(k) && isNumericID(n)
}

// handleAddSource proxies POST /v1/sources/{kind} — the registry-driven per-kind create.
func (s *server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !isSourceKindToken(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid source kind"})
		return
	}
	status, body, err := s.postCore(r.Context(), "/v1/sources/"+kind, r.Body)
	proxyWrite(w, status, body, err)
}

// handlePatchSource proxies PATCH /v1/sources/{source_id} — edits display_name and/or tags.
func (s *server) handlePatchSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("source_id")
	if !isSourceID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid source id"})
		return
	}
	status, body, err := s.doCore(r.Context(), http.MethodPatch, "/v1/sources/"+id, r.Body)
	proxyWrite(w, status, body, err)
}

// handleSourceConfig proxies GET /v1/sources/{source_id}/config — the source's raw editable
// fields, used to pre-fill the Edit modal.
func (s *server) handleSourceConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("source_id")
	if !isSourceID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid source id"})
		return
	}
	status, body, err := s.fetchCoreWithStatus(r.Context(), "/v1/sources/"+id+"/config")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, status, json.RawMessage(body))
}

// handleDeleteSource proxies DELETE /v1/sources/{source_id} — soft-delete (source disappears).
func (s *server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("source_id")
	if !isSourceID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid source id"})
		return
	}
	status, body, err := s.doCore(r.Context(), http.MethodDelete, "/v1/sources/"+id, r.Body)
	proxyWrite(w, status, body, err)
}

// handlePauseSource proxies POST /v1/sources/{source_id}/pause.
func (s *server) handlePauseSource(w http.ResponseWriter, r *http.Request) {
	s.toggleSource(w, r, "pause")
}

// handleResumeSource proxies POST /v1/sources/{source_id}/resume.
func (s *server) handleResumeSource(w http.ResponseWriter, r *http.Request) {
	s.toggleSource(w, r, "resume")
}

func (s *server) toggleSource(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("source_id")
	if !isSourceID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid source id"})
		return
	}
	status, body, err := s.postCore(r.Context(), "/v1/sources/"+id+"/"+action, r.Body)
	proxyWrite(w, status, body, err)
}

// handleBulkSources proxies POST /v1/sources/bulk — apply one action (pause|resume|tag|untag|
// delete) to many sources. The body (action/ids/tag) is forwarded as-is; the core validates it
// and returns per-item results, so a core 4xx propagates and transport failures become 502.
func (s *server) handleBulkSources(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/sources/bulk", r.Body)
	proxyWrite(w, status, body, err)
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

// handleWorkers proxies GET /v1/workers — workers grouped by logical name, each with a
// placements slice (the flat providers that share that worker name). This is the E2 grouped
// endpoint; the SPA renders it as a worker→placements tree.
func (s *server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/workers")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handlePlacementsFlat proxies GET /v1/providers — the flat list of all placements
// (individual provider rows). Used by the SPA for health cards, fallback selectors, and
// routing scopes where a flat list is needed.
func (s *server) handlePlacementsFlat(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/providers")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertPlacement proxies PUT /v1/providers with bearer injected server-side.
// Core 4xx (e.g. invalid enum) propagates; only transport errors become 502.
func (s *server) handleUpsertPlacement(w http.ResponseWriter, r *http.Request) {
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
// Flows are control-plane config the core surface owns. The BFF proxies them with the bearer
// injected server-side; reads map any core failure to 502, writes propagate core 4xx so the SPA can
// show validation errors. Podcast feeds are no longer special-cased here (#4b) — they ride the
// generic POST /api/sources/{kind} create like every other kind.

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
	mux.HandleFunc("GET /api/workers/metrics", s.handleWorkerMetrics)
	mux.HandleFunc("GET /api/health", s.handleCoreHealth)
	mux.HandleFunc("GET /api/usage", s.handleCoreUsage)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/pipeline", s.handlePipeline)
	mux.HandleFunc("GET /api/items/{id}/steps", s.handleItemSteps)
	mux.HandleFunc("GET /api/items/{id}/content", s.handleItemContent)
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
	// /api/workers → grouped worker→placements view (proxies /v1/workers).
	// /api/placements → flat provider list + upsert (proxies /v1/providers).
	mux.HandleFunc("GET /api/workers", s.handleWorkers)
	mux.HandleFunc("GET /api/placements", s.handlePlacementsFlat)
	mux.HandleFunc("PUT /api/placements", s.handleUpsertPlacement)
	mux.HandleFunc("GET /api/routing-policies", s.handleRoutingPolicies)
	mux.HandleFunc("PUT /api/routing-policies", s.handleUpsertRoutingPolicy)
	mux.HandleFunc("GET /api/decisions", s.handleDecisionsFeed)
	mux.HandleFunc("GET /api/items/{id}/decisions", s.handleItemDecisions)
	mux.HandleFunc("GET /api/source-kinds", s.handleSourceKinds)
	// /api/sources → unified sources_v list (fatia #1).
	mux.HandleFunc("GET /api/sources", s.handleSources)
	// Fontes CRUD writes (fatia #4). Deeper /pause|/resume patterns win over the {source_id}
	// catch-all. Podcast creation rides the {kind} create like every other kind (#4b).
	mux.HandleFunc("POST /api/sources/{kind}", s.handleAddSource)
	mux.HandleFunc("GET /api/sources/{source_id}/config", s.handleSourceConfig)
	mux.HandleFunc("PATCH /api/sources/{source_id}", s.handlePatchSource)
	mux.HandleFunc("DELETE /api/sources/{source_id}", s.handleDeleteSource)
	mux.HandleFunc("POST /api/sources/{source_id}/pause", s.handlePauseSource)
	mux.HandleFunc("POST /api/sources/{source_id}/resume", s.handleResumeSource)
	mux.HandleFunc("POST /api/sources/bulk", s.handleBulkSources)
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
