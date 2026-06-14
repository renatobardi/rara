package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callTool is a test convenience: marshal args and invoke the named tool.
func callTool(t *testing.T, s *mcpServer, name string, args any) (toolResult, *rpcError) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return s.callTool(context.Background(), name, raw)
}

// toolJSON unmarshals a (non-error) tool result's text content into dst.
func toolJSON(t *testing.T, res toolResult, dst any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool returned isError: %s", res.Content[0].Text)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("unexpected content: %+v", res.Content)
	}
	if dst != nil {
		if err := json.Unmarshal([]byte(res.Content[0].Text), dst); err != nil {
			t.Fatalf("unmarshal tool result %q: %v", res.Content[0].Text, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Registry — every tool is well-formed, and the expected set is exposed.
// ---------------------------------------------------------------------------

func TestMCPToolRegistryWellFormed(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)

	want := []string{
		"rara_list_items", "rara_item_steps", "rara_item_decisions", "rara_list_quarantine",
		"rara_list_flows", "rara_flow_steps", "rara_list_providers", "rara_list_routing_policies",
		"rara_list_gate_rules", "rara_get_interest_profile", "rara_list_interest_profiles",
		"rara_upsert_flow", "rara_upsert_flow_step", "rara_upsert_provider",
		"rara_upsert_routing_policy", "rara_upsert_gate_rule", "rara_add_interest_profile",
		"rara_approve_profile",
		"rara_feedback_distillation", "rara_review_quarantine", "rara_submit_linkedin_post",
	}
	if len(s.tools) != len(want) {
		t.Fatalf("tool count = %d, want %d", len(s.tools), len(want))
	}
	for _, name := range want {
		tl, ok := s.byName[name]
		if !ok {
			t.Errorf("missing tool %q", name)
			continue
		}
		if tl.Handler == nil {
			t.Errorf("tool %q has no handler", name)
		}
		if !json.Valid(tl.InputSchema) {
			t.Errorf("tool %q has invalid inputSchema: %s", name, tl.InputSchema)
		}
		if tl.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
	}
}

func TestMCPToolsListPayload(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	resp, write := s.dispatch(context.Background(), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if !write || resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	payload, _ := json.Marshal(resp.Result)
	var got struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != len(s.tools) {
		t.Errorf("tools/list returned %d tools, want %d", len(got.Tools), len(s.tools))
	}
	for _, tl := range got.Tools {
		if !json.Valid(tl.InputSchema) {
			t.Errorf("tool %q listed with invalid schema", tl.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// tool -> Core operation mapping (the core of the adapter)
// ---------------------------------------------------------------------------

func TestMCPSubmitLinkedInMapsToCore(t *testing.T) {
	ctx := context.Background()
	core, db, store := newTestCore(t)
	if err := SeedLinkedInLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := newMCPServer(core)

	res, rerr := callTool(t, s, "rara_submit_linkedin_post", map[string]string{
		"url": "https://lnkd.in/z", "author": "Renato", "text": "on orchestration",
	})
	if rerr != nil {
		t.Fatalf("protocol error: %+v", rerr)
	}
	var out struct {
		ItemID int `json:"item_id"`
	}
	toolJSON(t, res, &out)
	if out.ItemID == 0 {
		t.Fatal("submit tool returned no item_id")
	}
	if _, ok := store.posts["https://lnkd.in/z"]; !ok {
		t.Error("submit tool did not reach the inbox store")
	}
	if db.itemByID[out.ItemID].Lane != laneLinkedIn {
		t.Error("submit tool did not discover a linkedin item")
	}
}

func TestMCPUpsertProviderMapsToCore(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := newMCPServer(core)
	res, rerr := callTool(t, s, "rara_upsert_provider", Provider{
		Name: "gate-x", Capability: capGateRico, Runtime: runtimeVPC, Activation: activationResident,
		Quality: 0.7, Enabled: true,
	})
	if rerr != nil {
		t.Fatalf("protocol error: %+v", rerr)
	}
	toolJSON(t, res, nil)
	if _, ok := db.providers["gate-x"]; !ok {
		t.Error("upsert_provider tool did not write the provider")
	}
}

func TestMCPListItemsMapsAndFilters(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	mustItem(t, db, "youtube", "a", fid, itemQuarantine)
	s := newMCPServer(core)

	res, rerr := callTool(t, s, "rara_list_items", map[string]string{"status": itemQuarantine})
	if rerr != nil {
		t.Fatalf("protocol error: %+v", rerr)
	}
	var items []Item
	toolJSON(t, res, &items)
	if len(items) != 1 || items[0].SourceRef != "a" {
		t.Errorf("list_items via MCP = %+v, want [a]", items)
	}
	_ = ctx
}

func TestMCPReviewQuarantineMapsToCore(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	id, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "q", FlowID: db.flows[youtubeFlowName].ID, FlowVersion: 1, Status: itemQuarantine})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: id, Gate: gateBarato, Decision: decisionDefer, DecidedBy: decidedByLLM})
	s := newMCPServer(core)

	res, rerr := callTool(t, s, "rara_review_quarantine", map[string]any{"item_id": id, "signal": "down"})
	if rerr != nil {
		t.Fatalf("protocol error: %+v", rerr)
	}
	toolJSON(t, res, nil)
	if db.itemByID[id].Status != itemFiltered {
		t.Errorf("review down should confirm the drop (filtered), got %q", db.itemByID[id].Status)
	}
}

// ---------------------------------------------------------------------------
// Error semantics: domain errors are isError tool results; protocol errors are rpcError.
// ---------------------------------------------------------------------------

func TestMCPDomainErrorIsToolIsError(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	res, rerr := callTool(t, s, "rara_list_items", map[string]string{"status": "bogus"})
	if rerr != nil {
		t.Fatalf("a bad status is a tool error, not a protocol error: %+v", rerr)
	}
	if !res.IsError {
		t.Error("bad status should produce an isError tool result")
	}
}

func TestMCPUnknownToolIsProtocolError(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	_, rerr := s.callTool(context.Background(), "rara_does_not_exist", json.RawMessage(`{}`))
	if rerr == nil || rerr.Code != rpcErrInvalidParams {
		t.Errorf("unknown tool should be a protocol error, got %+v", rerr)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC envelope
// ---------------------------------------------------------------------------

func TestMCPDispatchInitialize(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	resp, write := s.dispatch(context.Background(), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"})
	if !write || resp.Error != nil {
		t.Fatalf("initialize failed: %+v", resp.Error)
	}
	payload, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(payload), mcpProtocolVersion) {
		t.Errorf("initialize result missing protocolVersion: %s", payload)
	}
}

func TestMCPDispatchNotificationHasNoResponse(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	_, write := s.dispatch(context.Background(), rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if write {
		t.Error("a notification must produce no response")
	}
}

func TestMCPDispatchUnknownMethod(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	resp, write := s.dispatch(context.Background(), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "frobnicate"})
	if !write || resp.Error == nil || resp.Error.Code != rpcErrMethodNotFound {
		t.Errorf("unknown method should be method-not-found, got %+v", resp.Error)
	}
}

func TestMCPServeHTTPToolsCall(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	fid := seedFlow(t, db)
	mustItem(t, db, "youtube", "v1", fid, itemDiscovered)
	s := newMCPServer(core)

	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"rara_list_items","arguments":{"status":"discovered"}}}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %+v", resp.Error)
	}
	// The result is a toolResult whose content text holds the items JSON.
	payload, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(payload), "v1") {
		t.Errorf("tools/call result missing the item: %s", payload)
	}
	_ = ctx
}

func TestMCPServeHTTPParseError(t *testing.T) {
	core, _, _ := newTestCore(t)
	s := newMCPServer(core)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{garbage")))
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != rpcErrParse {
		t.Errorf("malformed JSON-RPC should be a parse error, got %+v", resp.Error)
	}
}
