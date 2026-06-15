// surface.go — Phase 5 deliverables #1, #2, #4: the control surface (HTTP core + auth).
//
// rara-core is always-on in the VPC (the reconciler loop). Phase 5 mounts an HTTP control
// surface ALONGSIDE that ticker in the same process, so a person or an agent can OBSERVE the
// running system (items by status, an item's steps, the quarantine, an item's gate decisions)
// and EDIT its config as data (flows/flow_steps, providers, routing_policies, gate_rules,
// interest_profile) — plus drive the two human-in-the-loop signals (thumbs on a distillation,
// quarantine review) by reusing the Phase 3 functions verbatim.
//
// The surface is two thin front-ends over ONE núcleo:
//
//	Core  — the operations layer (this file): every read/edit/action, validated once, over the
//	        Database seam (+ the LinkedIn store). It holds NO transport concern.
//	HTTP  — a REST adapter (this file): parse request -> Core -> JSON.
//	MCP   — a JSON-RPC adapter (mcp.go): tool call -> Core -> result. Same Core, same ops.
//
// Both adapters are pure over the seam, so the whole surface is unit-tested with the
// MockDatabase + httptest — zero real I/O. Auth is a single service token (it is personal, but
// not left open): a bearer-token middleware that fails CLOSED (an unset token refuses to serve).
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Core — the operations layer (the "núcleo" both adapters drive).
// ---------------------------------------------------------------------------

// Core is the surface's operation layer: it validates inputs once and delegates to the
// persistence seam (reads + idempotent config upserts) and the reused Phase 3 / LinkedIn
// orchestration. Transport-agnostic — the HTTP and MCP adapters both call these methods.
type Core struct {
	db    Database
	inbox LinkedInPostStore
}

// NewCore wires the operations layer over the seam and the LinkedIn store.
func NewCore(db Database, inbox LinkedInPostStore) *Core { return &Core{db: db, inbox: inbox} }

// badInputError marks a caller error (bad id, unknown status, invalid enum) so the adapters
// answer 400, not 500. Genuine seam failures stay unwrapped and answer 500.
type badInputError struct{ msg string }

func (e badInputError) Error() string { return e.msg }
func badInput(format string, a ...any) error {
	return badInputError{msg: fmt.Sprintf(format, a...)}
}

// --- State reads ----------------------------------------------------------

// ListItems returns the items in a given lifecycle status (validated).
func (c *Core) ListItems(ctx context.Context, status string) ([]Item, error) {
	if !isValidItemStatus(status) {
		return nil, badInput("unknown item status %q", status)
	}
	return c.db.ListItemsByStatus(ctx, status)
}

// ItemSteps returns an item's runtime steps (the item_steps view).
func (c *Core) ItemSteps(ctx context.Context, itemID int) ([]ItemStep, error) {
	if itemID <= 0 {
		return nil, badInput("item id must be positive, got %d", itemID)
	}
	return c.db.ListItemSteps(ctx, itemID)
}

// ItemDecisions returns an item's full gate_decisions audit trail.
func (c *Core) ItemDecisions(ctx context.Context, itemID int) ([]GateDecision, error) {
	if itemID <= 0 {
		return nil, badInput("item id must be positive, got %d", itemID)
	}
	return c.db.ListGateDecisions(ctx, itemID)
}

// Quarantine lists the deferred (quarantine) items — the cold-start review sample.
func (c *Core) Quarantine(ctx context.Context) ([]Item, error) {
	return c.db.ListQuarantinedItems(ctx)
}

// --- Config reads ---------------------------------------------------------

func (c *Core) Flows(ctx context.Context) ([]Flow, error) { return c.db.ListFlows(ctx) }
func (c *Core) FlowSteps(ctx context.Context, flowID int) ([]FlowStep, error) {
	if flowID <= 0 {
		return nil, badInput("flow id must be positive, got %d", flowID)
	}
	return c.db.ListFlowSteps(ctx, flowID)
}
func (c *Core) Providers(ctx context.Context) ([]Provider, error) { return c.db.ListProviders(ctx) }
func (c *Core) RoutingPolicies(ctx context.Context) ([]RoutingPolicy, error) {
	return c.db.ListRoutingPolicies(ctx)
}
func (c *Core) GateRules(ctx context.Context) ([]GateRule, error) { return c.db.ListAllGateRules(ctx) }

// InterestProfile returns the ACTIVE preferences document (the version in force the gate reads),
// not merely the latest — a `proposed` revision is invisible here until approved.
func (c *Core) InterestProfile(ctx context.Context) (InterestProfile, bool, error) {
	return c.db.GetActiveInterestProfile(ctx)
}

// InterestProfiles returns every version (active + proposed + superseded), so an operator can see
// a pending proposal and decide whether to approve it.
func (c *Core) InterestProfiles(ctx context.Context) ([]InterestProfile, error) {
	return c.db.ListInterestProfiles(ctx)
}

// --- Config edits (idempotent upserts; a new profile version is append-only) ----

func (c *Core) UpsertFlow(ctx context.Context, f Flow) (int, error) { return c.db.UpsertFlow(ctx, f) }
func (c *Core) UpsertFlowStep(ctx context.Context, s FlowStep) error {
	return c.db.UpsertFlowStep(ctx, s)
}
func (c *Core) UpsertProvider(ctx context.Context, p Provider) error {
	// Validate the enums here so a bad value is a 400 (caller input), not a 500 (the db CHECK
	// would reject it deeper, but the surface should name it as a client error).
	if !isValidRuntime(p.Runtime) {
		return badInput("invalid runtime %q (want local|cloudrun|vpc)", p.Runtime)
	}
	if !isValidActivation(p.Activation) {
		return badInput("invalid activation %q (want resident|on_demand)", p.Activation)
	}
	// heartbeat_at is RUNTIME liveness (owned by TouchProviderHeartbeat), not config. A
	// full-record config upsert would clobber it — so PRESERVE the live value across an edit
	// (and leave it nil for a brand-new provider, which then gets the router's bootstrap grace).
	// This is why a `heartbeat_at` in the request body is ignored.
	if existing, found, err := c.db.GetProvider(ctx, p.Name); err != nil {
		return err
	} else if found {
		p.HeartbeatAt = existing.HeartbeatAt
	} else {
		p.HeartbeatAt = nil
	}
	return c.db.UpsertProvider(ctx, p)
}
func (c *Core) UpsertRoutingPolicy(ctx context.Context, p RoutingPolicy) error {
	if p.CostWeight < 0 || p.CostWeight > 1 || p.QualityWeight < 0 || p.QualityWeight > 1 {
		return badInput("cost_weight and quality_weight must be in [0,1]")
	}
	return c.db.UpsertRoutingPolicy(ctx, p)
}
func (c *Core) UpsertGateRule(ctx context.Context, r GateRule) error {
	if !isValidRuleAction(r.Action) {
		return badInput("invalid action %q (want allow|deny)", r.Action)
	}
	if !isValidMatchType(r.MatchType) {
		return badInput("invalid match_type %q (want channel|title_contains)", r.MatchType)
	}
	return c.db.UpsertGateRule(ctx, r)
}
func (c *Core) AddInterestProfile(ctx context.Context, p InterestProfile) error {
	if p.Version <= 0 {
		return badInput("interest_profile version must be positive, got %d", p.Version)
	}
	// A manually added version is a PROPOSAL — it never takes effect until approved, exactly like
	// a reviser-generated one. (The bootstrap v1 active row is seeded directly, not via here.)
	p.Status = profileProposed
	return c.db.InsertInterestProfile(ctx, p)
}

// ApproveProfile activates a proposed interest_profile version (human approval), demoting the
// prior active. A non-positive or non-proposed version is a caller error (400).
func (c *Core) ApproveProfile(ctx context.Context, version int) error {
	if version <= 0 {
		return badInput("interest_profile version must be positive, got %d", version)
	}
	if err := c.db.ActivateInterestProfile(ctx, version); err != nil {
		if errors.Is(err, errProfileNotProposed) {
			return badInput("interest_profile v%d is not a proposed version (already active, superseded, or absent)", version)
		}
		return err
	}
	return nil
}

// --- Human-in-the-loop (reuse the Phase 3 functions verbatim) --------------

// CaptureFeedback records explicit thumbs on a distillation (deliverable #4 of Phase 3).
func (c *Core) CaptureFeedback(ctx context.Context, distillationID, signal string) error {
	if err := CaptureDistillationFeedback(ctx, c.db, distillationID, signal); err != nil {
		return badInput("%v", err) // its errors are all caller-input (bad signal / empty id)
	}
	return nil
}

// ReviewQuarantineItem resolves a quarantined item (up rescues, down confirms the drop).
func (c *Core) ReviewQuarantineItem(ctx context.Context, itemID int, signal string) error {
	if signal != signalUp && signal != signalDown {
		return badInput("signal must be %q or %q, got %q", signalUp, signalDown, signal)
	}
	if itemID <= 0 {
		return badInput("item id must be positive, got %d", itemID)
	}
	// A missing item or one not actually in quarantine is a caller error (400), not a 500.
	// Pre-check it here so the surface names it clearly; ReviewQuarantine re-checks (harmless).
	if it, found, err := c.db.GetItem(ctx, itemID); err != nil {
		return err
	} else if !found || it.Status != itemQuarantine {
		return badInput("item %d is not in quarantine", itemID)
	}
	return ReviewQuarantine(ctx, c.db, itemID, signal)
}

// SubmitLinkedIn is the manual-inbox collector (deliverable #3): upsert the post + discover the
// spine item. Returns the item id.
func (c *Core) SubmitLinkedIn(ctx context.Context, p LinkedInPost) (int, error) {
	id, err := SubmitLinkedInPost(ctx, c.db, c.inbox, p)
	if err != nil {
		return 0, badInput("%v", err) // url/text validation are caller-input
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// HTTP adapter
// ---------------------------------------------------------------------------

// NewSurfaceMux builds the authenticated REST router over Core, with the MCP adapter mounted at
// /mcp. /healthz is exempt from auth (a liveness probe). The token must be non-empty (checked by
// the caller, ServeSurface); the middleware fails closed regardless.
func NewSurfaceMux(core *Core, token string) http.Handler {
	mux := http.NewServeMux()
	h := &httpSurface{core: core}

	// State reads.
	mux.HandleFunc("GET /v1/items", h.listItems)
	mux.HandleFunc("GET /v1/items/{id}/steps", h.itemSteps)
	mux.HandleFunc("GET /v1/items/{id}/decisions", h.itemDecisions)
	mux.HandleFunc("GET /v1/quarantine", h.quarantine)

	// Config reads.
	mux.HandleFunc("GET /v1/flows", h.listFlows)
	mux.HandleFunc("GET /v1/flows/{id}/steps", h.flowSteps)
	mux.HandleFunc("GET /v1/providers", h.listProviders)
	mux.HandleFunc("GET /v1/routing-policies", h.listRoutingPolicies)
	mux.HandleFunc("GET /v1/gate-rules", h.listGateRules)
	mux.HandleFunc("GET /v1/interest-profile", h.getInterestProfile)
	mux.HandleFunc("GET /v1/interest-profile/versions", h.listInterestProfiles)

	// Config edits (idempotent upserts; a new profile version is append-only).
	mux.HandleFunc("PUT /v1/flows", h.upsertFlow)
	mux.HandleFunc("PUT /v1/flow-steps", h.upsertFlowStep)
	mux.HandleFunc("PUT /v1/providers", h.upsertProvider)
	mux.HandleFunc("PUT /v1/routing-policies", h.upsertRoutingPolicy)
	mux.HandleFunc("PUT /v1/gate-rules", h.upsertGateRule)
	mux.HandleFunc("POST /v1/interest-profile", h.addInterestProfile)
	mux.HandleFunc("POST /v1/interest-profile/approve", h.approveInterestProfile)

	// Human-in-the-loop.
	mux.HandleFunc("POST /v1/feedback/distillation", h.feedbackDistillation)
	mux.HandleFunc("POST /v1/quarantine/review", h.reviewQuarantine)

	// LinkedIn manual inbox.
	mux.HandleFunc("POST /v1/linkedin/inbox", h.linkedinInbox)

	// MCP adapter (thin JSON-RPC front-end over the SAME Core).
	mux.Handle("POST /mcp", newMCPServer(core))

	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return authMiddleware(token, mux)
}

// authMiddleware enforces a single service token via `Authorization: Bearer <token>`, in
// constant time. It fails CLOSED — an empty configured token rejects everything — so the
// surface is never accidentally open. /live is exempt (an unauthenticated liveness probe).
func authMiddleware(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live" {
			next.ServeHTTP(w, r)
			return
		}
		got, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type httpSurface struct{ core *Core }

// --- read handlers --------------------------------------------------------

func (h *httpSurface) listItems(w http.ResponseWriter, r *http.Request) {
	items, err := h.core.ListItems(r.Context(), r.URL.Query().Get("status"))
	writeResult(w, items, err)
}

func (h *httpSurface) itemSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	steps, err := h.core.ItemSteps(r.Context(), id)
	writeResult(w, steps, err)
}

func (h *httpSurface) itemDecisions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	decs, err := h.core.ItemDecisions(r.Context(), id)
	writeResult(w, decs, err)
}

func (h *httpSurface) quarantine(w http.ResponseWriter, r *http.Request) {
	items, err := h.core.Quarantine(r.Context())
	writeResult(w, items, err)
}

func (h *httpSurface) listFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := h.core.Flows(r.Context())
	writeResult(w, flows, err)
}

func (h *httpSurface) flowSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	steps, err := h.core.FlowSteps(r.Context(), id)
	writeResult(w, steps, err)
}

func (h *httpSurface) listProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.core.Providers(r.Context())
	writeResult(w, providers, err)
}

func (h *httpSurface) listRoutingPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.core.RoutingPolicies(r.Context())
	writeResult(w, policies, err)
}

func (h *httpSurface) listGateRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.core.GateRules(r.Context())
	writeResult(w, rules, err)
}

func (h *httpSurface) getInterestProfile(w http.ResponseWriter, r *http.Request) {
	prof, found, err := h.core.InterestProfile(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found {
		http.Error(w, `{"error":"no active interest_profile"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, prof)
}

func (h *httpSurface) listInterestProfiles(w http.ResponseWriter, r *http.Request) {
	profs, err := h.core.InterestProfiles(r.Context())
	writeResult(w, profs, err)
}

func (h *httpSurface) approveInterestProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.ApproveProfile(r.Context(), req.Version))
}

// --- edit handlers --------------------------------------------------------

func (h *httpSurface) upsertFlow(w http.ResponseWriter, r *http.Request) {
	var f Flow
	if !decodeJSON(w, r, &f) {
		return
	}
	id, err := h.core.UpsertFlow(r.Context(), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": id})
}

func (h *httpSurface) upsertFlowStep(w http.ResponseWriter, r *http.Request) {
	var s FlowStep
	if !decodeJSON(w, r, &s) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertFlowStep(r.Context(), s))
}

func (h *httpSurface) upsertProvider(w http.ResponseWriter, r *http.Request) {
	var p Provider
	if !decodeJSON(w, r, &p) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertProvider(r.Context(), p))
}

func (h *httpSurface) upsertRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	var p RoutingPolicy
	if !decodeJSON(w, r, &p) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertRoutingPolicy(r.Context(), p))
}

func (h *httpSurface) upsertGateRule(w http.ResponseWriter, r *http.Request) {
	var rule GateRule
	if !decodeJSON(w, r, &rule) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertGateRule(r.Context(), rule))
}

func (h *httpSurface) addInterestProfile(w http.ResponseWriter, r *http.Request) {
	var p InterestProfile
	if !decodeJSON(w, r, &p) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.AddInterestProfile(r.Context(), p))
}

// --- action handlers ------------------------------------------------------

func (h *httpSurface) feedbackDistillation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DistillationID string `json:"distillation_id"`
		Signal         string `json:"signal"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.CaptureFeedback(r.Context(), req.DistillationID, req.Signal))
}

func (h *httpSurface) reviewQuarantine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ItemID int    `json:"item_id"`
		Signal string `json:"signal"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.ReviewQuarantineItem(r.Context(), req.ItemID, req.Signal))
}

func (h *httpSurface) linkedinInbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL    string `json:"url"`
		Author string `json:"author"`
		Text   string `json:"text"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := h.core.SubmitLinkedIn(r.Context(), LinkedInPost{URL: req.URL, Author: req.Author, Text: req.Text})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"item_id": id})
}

// --- http helpers ---------------------------------------------------------

type okResult struct {
	OK bool `json:"ok"`
}

// pathID parses the {id} path wildcard as a positive int, answering 400 on a bad value.
func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.Error(w, `{"error":"invalid id in path"}`, http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// maxBodyBytes caps a request body (1 MiB) — far above any config row or pasted post, but a
// backstop against an unbounded body exhausting memory. Exceeding it fails the decode -> 400.
const maxBodyBytes = 1 << 20

// decodeJSON decodes a (size-capped) JSON request body, answering 400 on a malformed/oversized
// body. DisallowUnknownFields is deliberate: this is a config-edit API, so a mistyped field
// should be a visible 400, not silently dropped.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return false
	}
	return true
}

// writeResult writes data as 200 JSON, or maps err to a status (400 badInput, else 500).
func writeResult(w http.ResponseWriter, data any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// writeErr maps an error to its HTTP status: a badInputError is 400, anything else 500.
func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var bad badInputError
	if errors.As(err, &bad) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("surface: encode response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Server lifecycle
// ---------------------------------------------------------------------------

// ServeSurface runs the control surface until ctx is cancelled, then shuts it down gracefully.
// It fails closed: an empty token is refused (the surface is personal, but never left open).
// Called both standalone (`core-job surface`) and from the reconciler loop (same process,
// alongside the ticker — the always-on VPC deployment).
func ServeSurface(ctx context.Context, core *Core, addr, token string) error {
	if token == "" {
		return fmt.Errorf("surface: SURFACE_TOKEN is required (refusing to serve without auth)")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           NewSurfaceMux(core, token),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("rara-core surface: listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
