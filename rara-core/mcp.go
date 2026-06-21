// mcp.go — Phase 5 deliverable #2: the thin MCP adapter in front of the HTTP núcleo.
//
// MCP (Model Context Protocol) is the open, vendor-neutral standard (Linux Foundation / AAIF)
// the architecture builds the surface on — an anti-lock-in move (ARCHITECTURE-2.0, "Surface:
// MCP over HTTP"). This adapter exposes the SAME Core operations the REST surface does, as MCP
// TOOLS, so Cowork/agents can drive rara: list pending work, read an item's state, read/edit
// config, submit feedback, push a LinkedIn post.
//
// It is deliberately THIN: a JSON-RPC 2.0 dispatcher (initialize / tools/list / tools/call) and
// a tool registry where each tool's handler parses its arguments and calls one Core method. No
// business logic lives here — the mapping tool->operation is the whole adapter, and that mapping
// is exactly what mcp_test.go pins (zero I/O, against the MockDatabase).
package main

import (
	"context"
	"encoding/json"
	"net/http"
)

// mcpProtocolVersion is the MCP revision this adapter speaks (echoed in `initialize`).
const mcpProtocolVersion = "2025-06-18"

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelope
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent/null => a notification (no response)
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC standard error codes used here.
const (
	rpcErrParse          = -32700
	rpcErrInvalidRequest = -32600
	rpcErrMethodNotFound = -32601
	rpcErrInvalidParams  = -32602
)

// ---------------------------------------------------------------------------
// Tool registry
// ---------------------------------------------------------------------------

// mcpTool is one exposed operation: its name/description/inputSchema (the MCP tool contract)
// and a handler that parses arguments and calls Core. The handler's returned value is JSON-
// encoded into the tool result's text content; a handler error becomes an isError tool result.
type mcpTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

// toolResult is the MCP tools/call result shape: content blocks + an isError flag (a tool's
// domain error is reported HERE, not as a JSON-RPC error, per the MCP spec).
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpServer is the JSON-RPC handler over the tool registry. It is also an http.Handler mounted
// at POST /mcp by the REST surface (behind the same auth middleware).
type mcpServer struct {
	core   *Core
	tools  []mcpTool
	byName map[string]mcpTool
}

func newMCPServer(core *Core) *mcpServer {
	tools := buildTools(core)
	byName := make(map[string]mcpTool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return &mcpServer{core: core, tools: tools, byName: byName}
}

// buildTools is the whole adapter: the explicit map of MCP tool -> Core operation. The upsert
// tools unmarshal their arguments straight into the json-tagged domain structs (so the tool
// schema IS the config-as-data shape); the rest into small request structs.
func buildTools(core *Core) []mcpTool {
	return []mcpTool{
		// --- state reads ---
		{
			Name: "rara_list_items", Description: "List spine items in a given lifecycle status (discovered, to_text, distilled, done, filtered, quarantine, failed).",
			InputSchema: schemaObject(`{"status":{"type":"string"}}`, "status"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				return core.ListItems(ctx, a.Status)
			},
		},
		{
			Name: "rara_item_steps", Description: "List the runtime item_steps of one item.",
			InputSchema: schemaObject(`{"item_id":{"type":"integer"}}`, "item_id"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decodeItemID(raw)
				if err != nil {
					return nil, err
				}
				return core.ItemSteps(ctx, a)
			},
		},
		{
			Name: "rara_item_decisions", Description: "List the full gate_decisions audit trail of one item.",
			InputSchema: schemaObject(`{"item_id":{"type":"integer"}}`, "item_id"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decodeItemID(raw)
				if err != nil {
					return nil, err
				}
				return core.ItemDecisions(ctx, a)
			},
		},
		{
			Name: "rara_list_quarantine", Description: "List items deferred to quarantine (the cold-start review sample).",
			InputSchema: schemaObject(`{}`),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return core.Quarantine(ctx)
			},
		},
		// --- config reads ---
		{
			Name: "rara_list_flows", Description: "List all flows (lanes) as config-as-data.",
			InputSchema: schemaObject(`{}`),
			Handler:     func(ctx context.Context, _ json.RawMessage) (any, error) { return core.Flows(ctx) },
		},
		{
			Name: "rara_flow_steps", Description: "List the ordered steps of one flow.",
			InputSchema: schemaObject(`{"flow_id":{"type":"integer"}}`, "flow_id"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					FlowID int `json:"flow_id"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				return core.FlowSteps(ctx, a.FlowID)
			},
		},
		{
			Name: "rara_list_providers", Description: "List all providers (workers) as config-as-data.",
			InputSchema: schemaObject(`{}`),
			Handler:     func(ctx context.Context, _ json.RawMessage) (any, error) { return core.Providers(ctx) },
		},
		{
			Name: "rara_list_routing_policies", Description: "List all routing policies as config-as-data.",
			InputSchema: schemaObject(`{}`),
			Handler:     func(ctx context.Context, _ json.RawMessage) (any, error) { return core.RoutingPolicies(ctx) },
		},
		{
			Name: "rara_list_gate_rules", Description: "List all gate rules (allow/deny) as config-as-data.",
			InputSchema: schemaObject(`{}`),
			Handler:     func(ctx context.Context, _ json.RawMessage) (any, error) { return core.GateRules(ctx) },
		},
		{
			Name: "rara_get_interest_profile", Description: "Get the live (highest-version) interest_profile document.",
			InputSchema: schemaObject(`{}`),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				prof, found, err := core.InterestProfile(ctx)
				if err != nil {
					return nil, err
				}
				if !found {
					return map[string]any{"found": false}, nil
				}
				return prof, nil
			},
		},
		// --- config edits (idempotent upserts; the structs ARE the schema) ---
		{
			Name: "rara_upsert_flow", Description: "Create or update a flow (idempotent on name). Returns its id.",
			InputSchema: schemaObject(`{"name":{"type":"string"},"source_type":{"type":"string"},"enabled":{"type":"boolean"},"version":{"type":"integer"}}`, "name", "source_type"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var f Flow
				if err := json.Unmarshal(raw, &f); err != nil {
					return nil, err
				}
				id, err := core.UpsertFlow(ctx, f)
				if err != nil {
					return nil, err
				}
				return map[string]int{"id": id}, nil
			},
		},
		{
			Name: "rara_upsert_flow_step", Description: "Create or update one flow step (idempotent on flow_id+seq).",
			InputSchema: schemaObject(`{"flow_id":{"type":"integer"},"seq":{"type":"integer"},"capability":{"type":"string"},"options":{"type":"object"},"enabled":{"type":"boolean"}}`, "flow_id", "seq", "capability"),
			Handler:     upsertHandler(func(ctx context.Context, s FlowStep) error { return core.UpsertFlowStep(ctx, s) }),
		},
		{
			Name: "rara_upsert_provider", Description: "Create or update a provider (idempotent on name).",
			InputSchema: schemaObject(`{"name":{"type":"string"},"capability":{"type":"string"},"runtime":{"type":"string"},"activation":{"type":"string"},"constraints":{"type":"object"},"enabled":{"type":"boolean"}}`, "name", "capability", "runtime", "activation"),
			Handler:     upsertHandler(func(ctx context.Context, p Provider) error { return core.UpsertProvider(ctx, p) }),
		},
		{
			Name: "rara_upsert_routing_policy", Description: "Create or update a routing policy (idempotent on scope).",
			InputSchema: schemaObject(`{"scope":{"type":"string"},"fallback":{"type":"array"}}`, "scope"),
			Handler:     upsertHandler(func(ctx context.Context, p RoutingPolicy) error { return core.UpsertRoutingPolicy(ctx, p) }),
		},
		{
			Name: "rara_upsert_gate_rule", Description: "Create or update a gate rule (idempotent on action+match_type+value).",
			InputSchema: schemaObject(`{"action":{"type":"string"},"match_type":{"type":"string"},"value":{"type":"string"},"enabled":{"type":"boolean"}}`, "action", "match_type", "value"),
			Handler:     upsertHandler(func(ctx context.Context, r GateRule) error { return core.UpsertGateRule(ctx, r) }),
		},
		{
			Name: "rara_add_interest_profile", Description: "Append a new interest_profile version as a PROPOSAL (append-only; takes effect only after rara_approve_profile).",
			InputSchema: schemaObject(`{"version":{"type":"integer"},"topics":{"type":"array"},"authors":{"type":"array"},"anti_topics":{"type":"array"},"weights":{"type":"object"},"narrative":{"type":"string"}}`, "version"),
			Handler:     upsertHandler(func(ctx context.Context, p InterestProfile) error { return core.AddInterestProfile(ctx, p) }),
		},
		{
			Name: "rara_list_interest_profiles", Description: "List every interest_profile version with its status (active | proposed | superseded) — to review a pending proposal.",
			InputSchema: schemaObject(`{}`),
			Handler:     func(ctx context.Context, _ json.RawMessage) (any, error) { return core.InterestProfiles(ctx) },
		},
		{
			Name: "rara_approve_profile", Description: "Approve (activate) a proposed interest_profile version; the prior active version is superseded. Human approval before a revision takes effect.",
			InputSchema: schemaObject(`{"version":{"type":"integer"}}`, "version"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					Version int `json:"version"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				if err := core.ApproveProfile(ctx, a.Version); err != nil {
					return nil, err
				}
				return okResult{OK: true}, nil
			},
		},
		// --- human-in-the-loop ---
		{
			Name: "rara_feedback_distillation", Description: "Record explicit thumbs (up|down) on a distillation.",
			InputSchema: schemaObject(`{"distillation_id":{"type":"string"},"signal":{"type":"string"}}`, "distillation_id", "signal"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					DistillationID string `json:"distillation_id"`
					Signal         string `json:"signal"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				if err := core.CaptureFeedback(ctx, a.DistillationID, a.Signal); err != nil {
					return nil, err
				}
				return okResult{OK: true}, nil
			},
		},
		{
			Name: "rara_review_quarantine", Description: "Review a quarantined item: up rescues it back into the pipeline, down confirms the drop.",
			InputSchema: schemaObject(`{"item_id":{"type":"integer"},"signal":{"type":"string"}}`, "item_id", "signal"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					ItemID int    `json:"item_id"`
					Signal string `json:"signal"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				if err := core.ReviewQuarantineItem(ctx, a.ItemID, a.Signal); err != nil {
					return nil, err
				}
				return okResult{OK: true}, nil
			},
		},
		// --- LinkedIn manual inbox ---
		{
			Name: "rara_submit_linkedin_post", Description: "Submit a LinkedIn post (url + text, optional author) into the manual inbox; upserts it and discovers the spine item. Returns the item id.",
			InputSchema: schemaObject(`{"url":{"type":"string"},"author":{"type":"string"},"text":{"type":"string"}}`, "url", "text"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					URL    string `json:"url"`
					Author string `json:"author"`
					Text   string `json:"text"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				id, err := core.SubmitLinkedIn(ctx, LinkedInPost{URL: a.URL, Author: a.Author, Text: a.Text})
				if err != nil {
					return nil, err
				}
				return map[string]int{"item_id": id}, nil
			},
		},
	}
}

// upsertHandler builds a tool handler that unmarshals the arguments into a domain struct T and
// calls an idempotent Core upsert — the shared shape behind every config-edit tool.
func upsertHandler[T any](fn func(context.Context, T) error) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if err := fn(ctx, v); err != nil {
			return nil, err
		}
		return okResult{OK: true}, nil
	}
}

// decodeItemID parses the common {"item_id": N} argument shape.
func decodeItemID(raw json.RawMessage) (int, error) {
	var a struct {
		ItemID int `json:"item_id"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return 0, err
	}
	return a.ItemID, nil
}

// schemaObject builds a JSON-Schema object literal from a properties body and the required keys
// — concise enough to keep the registry readable while still giving MCP clients a real schema.
func schemaObject(properties string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if len(required) == 0 {
		req = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","properties":` + properties + `,"required":` + string(req) + `}`)
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// callTool runs a registered tool by name. A missing tool or unparseable arguments is a
// PROTOCOL error (returned as *rpcError); a tool handler's own error is a tool result with
// isError=true (the MCP convention — the model sees the failure, the call still "succeeds").
func (s *mcpServer) callTool(ctx context.Context, name string, args json.RawMessage) (toolResult, *rpcError) {
	t, ok := s.byName[name]
	if !ok {
		return toolResult{}, &rpcError{Code: rpcErrInvalidParams, Message: "unknown tool: " + name}
	}
	data, err := t.Handler(ctx, args)
	if err != nil {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}
	encoded, mErr := json.Marshal(data)
	if mErr != nil {
		return toolResult{}, &rpcError{Code: rpcErrInvalidParams, Message: "encode result: " + mErr.Error()}
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: string(encoded)}}}, nil
}

// toolList is the tools/list payload: the registry minus the handlers.
func (s *mcpServer) toolList() any {
	type listed struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	out := make([]listed, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, listed{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return map[string]any{"tools": out}
}

// dispatch handles one JSON-RPC request, returning the response and whether to write it (false
// for notifications, which take no response). It is the pure dispatch seam ServeHTTP wraps.
func (s *mcpServer) dispatch(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	// A request with no id is a notification (e.g. notifications/initialized): act, answer nothing.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "rara-core", "version": "phase6"},
		}
	case "notifications/initialized":
		return rpcResponse{}, false // notification: no response
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = s.toolList()
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: rpcErrInvalidParams, Message: "invalid tools/call params"}
			break
		}
		result, rerr := s.callTool(ctx, p.Name, p.Arguments)
		if rerr != nil {
			resp.Error = rerr
			break
		}
		resp.Result = result
	default:
		resp.Error = &rpcError{Code: rpcErrMethodNotFound, Message: "method not found: " + req.Method}
	}

	if isNotification {
		return rpcResponse{}, false
	}
	return resp, true
}

// ServeHTTP is the POST /mcp endpoint: decode the JSON-RPC request, dispatch, write the response.
func (s *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: rpcErrParse, Message: "parse error"},
		})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: rpcErrInvalidRequest, Message: "invalid request"},
		})
		return
	}
	resp, write := s.dispatch(r.Context(), req)
	if !write {
		w.WriteHeader(http.StatusAccepted) // notification acknowledged, no body
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
