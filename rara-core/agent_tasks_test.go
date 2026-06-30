package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// MockDatabase — agent_tasks methods (mirror the SQL contract, zero I/O)
// ---------------------------------------------------------------------------

type mockAgentTask struct {
	ID           int
	AgentID      int
	Instruction  string
	ContextRefs  json.RawMessage
	Status       string
	Priority     int
	DispatchedAt *time.Time
}

func (m *MockDatabase) EnqueueAgentTask(_ context.Context, t AgentTaskRecord) (int, error) {
	// Mirror the pgx INSERT … SELECT … WHERE active-agent: enqueuing onto a missing or
	// soft-deleted agent is a 404, not an orphan row.
	if !m.agentActive(t.AgentID) {
		return 0, errNotFound
	}
	id := m.nextAgentTaskID
	m.nextAgentTaskID++
	// Copy context_refs so the stored row doesn't alias the caller's slice — mirrors the
	// isolation a real jsonb round-trip gives (a later mutation of the input can't reach state).
	refs := append(json.RawMessage(nil), t.ContextRefs...)
	m.agentTasks = append(m.agentTasks, mockAgentTask{
		ID: id, AgentID: t.AgentID, Instruction: t.Instruction,
		ContextRefs: refs, Status: taskQueued, Priority: t.Priority,
	})
	return id, nil
}

func (m *MockDatabase) ListAgentTasks(_ context.Context, agentID int) ([]AgentTaskRow, error) {
	var out []AgentTaskRow
	// Newest first (mirrors id DESC).
	for i := len(m.agentTasks) - 1; i >= 0; i-- {
		if m.agentTasks[i].AgentID == agentID {
			out = append(out, m.agentTasks[i].row())
		}
	}
	return out, nil
}

func (m *MockDatabase) ListAllAgentTasks(_ context.Context, status string) ([]AgentTaskRow, error) {
	var out []AgentTaskRow
	for i := len(m.agentTasks) - 1; i >= 0; i-- {
		if status != "" && m.agentTasks[i].Status != status {
			continue
		}
		out = append(out, m.agentTasks[i].row())
	}
	return out, nil
}

func (m *MockDatabase) ClaimAgentTask(_ context.Context) (*AgentTaskRow, error) {
	best := -1
	for i := range m.agentTasks {
		if m.agentTasks[i].Status != taskQueued {
			continue
		}
		// Highest priority wins; ties break by lowest id (= insertion order = created_at, id).
		if best == -1 || m.agentTasks[i].Priority > m.agentTasks[best].Priority {
			best = i
		}
	}
	if best == -1 {
		return nil, nil // queue empty: idempotent no-op
	}
	now := m.nowFn()
	m.agentTasks[best].Status = taskDispatched
	m.agentTasks[best].DispatchedAt = &now
	row := m.agentTasks[best].row()
	return &row, nil
}

func (t mockAgentTask) row() AgentTaskRow {
	refs := t.ContextRefs
	if len(refs) == 0 {
		refs = json.RawMessage("[]")
	}
	return AgentTaskRow{
		ID: t.ID, AgentID: t.AgentID, Instruction: t.Instruction, ContextRefs: refs,
		Status: t.Status, Priority: t.Priority, DispatchedAt: t.DispatchedAt,
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustEnqueueTask(t *testing.T, core *Core, ctx context.Context, agentID int, in AgentTaskInput) int {
	t.Helper()
	id, err := core.EnqueueAgentTask(ctx, agentID, in)
	if err != nil {
		t.Fatalf("EnqueueAgentTask: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Core.EnqueueAgentTask / ListAgentTasks / ListAllAgentTasks
// ---------------------------------------------------------------------------

func TestEnqueueAgentTaskQueuesAndLists(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})

	mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "summarize the corpus"})

	got, err := core.ListAgentTasks(ctx, aid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Instruction != "summarize the corpus" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Status != taskQueued {
		t.Errorf("status = %q, want queued", got[0].Status)
	}
	if string(got[0].ContextRefs) != "[]" {
		t.Errorf("context_refs = %s, want default []", got[0].ContextRefs)
	}
}

func TestEnqueueAgentTaskRejectsEmptyInstruction(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	var bad badInputError
	if _, err := core.EnqueueAgentTask(ctx, aid, AgentTaskInput{Instruction: "  "}); !errors.As(err, &bad) {
		t.Fatalf("want badInput for empty instruction, got %v", err)
	}
}

func TestEnqueueAgentTaskRejectsMissingAgent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.EnqueueAgentTask(ctx, 999, AgentTaskInput{Instruction: "x"}); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound for missing agent, got %v", err)
	}
}

func TestEnqueueAgentTaskRejectsSoftDeletedAgent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	if err := core.DeleteAgent(ctx, aid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := core.EnqueueAgentTask(ctx, aid, AgentTaskInput{Instruction: "x"}); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound for soft-deleted agent, got %v", err)
	}
}

func TestEnqueueAgentTaskRejectsNonArrayContextRefs(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	var bad badInputError
	for _, refs := range []string{`{"id":1}`, `5`, `"x"`, `not json`} {
		_, err := core.EnqueueAgentTask(ctx, aid, AgentTaskInput{
			Instruction: "x", ContextRefs: json.RawMessage(refs)})
		if !errors.As(err, &bad) {
			t.Errorf("context_refs %q accepted, want badInput", refs)
		}
	}
	// A well-formed array is accepted and stored verbatim.
	id, err := core.EnqueueAgentTask(ctx, aid, AgentTaskInput{
		Instruction: "x", ContextRefs: json.RawMessage(`[{"type":"distillation","id":5}]`)})
	if err != nil || id == 0 {
		t.Fatalf("valid context_refs rejected: %v", err)
	}
}

func TestListAllAgentTasksFiltersByStatus(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "a"})
	mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "b"})

	// Claim one → it leaves the queued set.
	if _, err := core.ClaimAgentTask(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	queued, err := core.ListAllAgentTasks(ctx, taskQueued)
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued = %d, want 1 (one was claimed)", len(queued))
	}
	all, err := core.ListAllAgentTasks(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %d, want 2", len(all))
	}
}

func TestListAllAgentTasksRejectsBadStatus(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	var bad badInputError
	if _, err := core.ListAllAgentTasks(ctx, "bogus"); !errors.As(err, &bad) {
		t.Fatalf("want badInput for unknown status, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core.ClaimAgentTask — the FOR UPDATE SKIP LOCKED frontier
// ---------------------------------------------------------------------------

func TestClaimAgentTaskMovesToDispatched(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	id := mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "go"})

	claimed, err := core.ClaimAgentTask(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != id {
		t.Fatalf("claimed = %+v, want task %d", claimed, id)
	}
	if claimed.Status != taskDispatched {
		t.Errorf("status = %q, want dispatched", claimed.Status)
	}
	if claimed.DispatchedAt == nil {
		t.Error("dispatched_at not stamped")
	}
}

// A claimed (dispatched) task is never handed to a second claimer — the SKIP LOCKED contract.
func TestClaimAgentTaskNotHandedTwice(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "only one"})

	first, err := core.ClaimAgentTask(ctx)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := core.ClaimAgentTask(ctx)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if first == nil {
		t.Fatal("first claim returned nothing")
	}
	if second != nil {
		t.Fatalf("second claim re-handed task %d (double dispatch)", second.ID)
	}
}

func TestClaimAgentTaskHonoursPriority(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "low"})
	hi := mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "high", Priority: 10})

	claimed, err := core.ClaimAgentTask(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != hi {
		t.Fatalf("claimed = %+v, want high-priority task %d first", claimed, hi)
	}
}

func TestClaimAgentTaskEmptyQueue(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	claimed, err := core.ClaimAgentTask(ctx)
	if err != nil {
		t.Fatalf("claim on empty queue: %v", err)
	}
	if claimed != nil {
		t.Fatalf("want nil on empty queue, got %+v", claimed)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func TestAgentTasksHTTPRoundTrip(t *testing.T) {
	core, _, _ := newTestCore(t)
	ctx := context.Background()
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	mux := NewSurfaceMux(core, "tok")

	// Enqueue via POST /v1/agents/{id}/tasks.
	body := `{"instruction":"summarize","context_refs":[1,2]}`
	if rec := authedDo(t, mux, "POST", fmt.Sprintf("/v1/agents/%d/tasks", aid), body); rec.Code != http.StatusOK {
		t.Fatalf("POST tasks: %d %s", rec.Code, rec.Body)
	}
	// Per-agent history.
	rec := authedDo(t, mux, "GET", fmt.Sprintf("/v1/agents/%d/tasks", aid), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "summarize") {
		t.Fatalf("GET agent tasks: %d %s", rec.Code, rec.Body)
	}
	// Global board feed.
	rec = authedDo(t, mux, "GET", "/v1/agent-tasks", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "summarize") {
		t.Fatalf("GET /v1/agent-tasks: %d %s", rec.Code, rec.Body)
	}
}

func TestAgentTasksHTTPRejectsBadID(t *testing.T) {
	core, _, _ := newTestCore(t)
	mux := NewSurfaceMux(core, "tok")
	if rec := authedDo(t, mux, "POST", "/v1/agents/abc/tasks", `{"instruction":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("POST /v1/agents/abc/tasks: status=%d, want 400", rec.Code)
	}
}

func TestAgentTasksHTTPGlobalFeedFiltersStatus(t *testing.T) {
	core, _, _ := newTestCore(t)
	ctx := context.Background()
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	mustEnqueueTask(t, core, ctx, aid, AgentTaskInput{Instruction: "queued one"})
	mux := NewSurfaceMux(core, "tok")

	rec := authedDo(t, mux, "GET", "/v1/agent-tasks?status=queued", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "queued one") {
		t.Fatalf("filtered feed: %d %s", rec.Code, rec.Body)
	}
	// An unknown status is a 400 (allowlist), not a silent empty list.
	if rec := authedDo(t, mux, "GET", "/v1/agent-tasks?status=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("status=bogus: %d, want 400", rec.Code)
	}
}
