package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// MockDatabase — Agent methods (mirror the SQL contract, zero I/O)
// ---------------------------------------------------------------------------

func (m *MockDatabase) UpsertAgent(_ context.Context, name, description, avatarURL, visibility, instructions, model string) (int, error) {
	for i, a := range m.agents {
		if a.Name == name && a.DeletedAt == nil {
			m.agents[i].Description = description
			m.agents[i].AvatarURL = avatarURL
			m.agents[i].Visibility = visibility
			m.agents[i].Instructions = instructions
			m.agents[i].Model = model
			return a.ID, nil
		}
	}
	id := m.nextAgentID
	m.nextAgentID++
	m.agents = append(m.agents, mockAgent{
		ID: id, Name: name, Description: description, AvatarURL: avatarURL,
		Visibility: visibility, Instructions: instructions, Model: model,
	})
	return id, nil
}

func (m *MockDatabase) ListAgents(_ context.Context) ([]AgentRow, error) {
	var out []AgentRow
	for _, a := range m.agents {
		if a.DeletedAt != nil {
			continue
		}
		out = append(out, AgentRow{
			ID: a.ID, Name: a.Name, Description: a.Description, AvatarURL: a.AvatarURL,
			Visibility: a.Visibility, Instructions: a.Instructions, Model: a.Model,
		})
	}
	return out, nil
}

// agentActive mirrors the deleted_at IS NULL guard.
func (m *MockDatabase) agentActive(id int) bool {
	for _, a := range m.agents {
		if a.ID == id {
			return a.DeletedAt == nil
		}
	}
	return false
}

// skillActiveExists mirrors the SET path's validation: a skill can be linked only if it exists
// and is not soft-deleted (the pgx INSERT … SELECT filters on deleted_at IS NULL).
func (m *MockDatabase) skillActiveExists(id int) bool {
	for _, s := range m.skills {
		if s.ID == id {
			return s.DeletedAt == nil
		}
	}
	return false
}

func (m *MockDatabase) GetAgent(_ context.Context, id int) (AgentRow, error) {
	for _, a := range m.agents {
		if a.ID != id || a.DeletedAt != nil {
			continue
		}
		row := AgentRow{
			ID: a.ID, Name: a.Name, Description: a.Description, AvatarURL: a.AvatarURL,
			Visibility: a.Visibility, Instructions: a.Instructions, Model: a.Model,
		}
		// Attached skills, excluding soft-deleted skills (the link survives a soft delete).
		var ids []int
		for _, sid := range m.agentSkills[id] {
			for _, s := range m.skills {
				if s.ID == sid && s.DeletedAt == nil {
					ids = append(ids, sid)
				}
			}
		}
		sort.Ints(ids)
		row.SkillIDs = ids
		return row, nil
	}
	return AgentRow{}, errNotFound
}

func (m *MockDatabase) DeleteAgent(_ context.Context, id int) error {
	tt := true
	for i, a := range m.agents {
		if a.ID == id {
			m.agents[i].DeletedAt = &tt
			return nil
		}
	}
	return nil
}

func (m *MockDatabase) SetAgentSkills(_ context.Context, agentID int, skillIDs []int) error {
	if !m.agentActive(agentID) {
		return errNotFound
	}
	for _, sid := range skillIDs {
		if !m.skillActiveExists(sid) {
			return badInput("skill %d does not exist or is deleted", sid)
		}
	}
	m.agentSkills[agentID] = append([]int(nil), skillIDs...)
	return nil
}

// ---------------------------------------------------------------------------
// mustAgent helpers
// ---------------------------------------------------------------------------

func mustAgent(t *testing.T, core *Core, ctx context.Context, in AgentInput) int {
	t.Helper()
	id, err := core.UpsertAgent(ctx, in)
	if err != nil {
		t.Fatalf("UpsertAgent(%q): %v", in.Name, err)
	}
	return id
}

func mustAgents(t *testing.T, core *Core, ctx context.Context) []AgentRow {
	t.Helper()
	got, err := core.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	return got
}

// ---------------------------------------------------------------------------
// Core.UpsertAgent / ListAgents / GetAgent / DeleteAgent
// ---------------------------------------------------------------------------

func TestUpsertAgentCreatesAndLists(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	mustAgent(t, core, ctx, AgentInput{Name: "Docslide Writer", Instructions: "write slides", Model: "groq/llama-3.3-70b"})
	got := mustAgents(t, core, ctx)
	if len(got) != 1 || got[0].Name != "Docslide Writer" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Instructions != "write slides" || got[0].Model != "groq/llama-3.3-70b" {
		t.Errorf("instructions/model not persisted: %+v", got[0])
	}
	if got[0].Visibility != "workspace" {
		t.Errorf("visibility = %q, want default workspace", got[0].Visibility)
	}
}

func TestUpsertAgentRejectsEmptyName(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	_, err := core.UpsertAgent(ctx, AgentInput{Name: "  "})
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Fatalf("want badInput, got %v", err)
	}
}

func TestUpsertAgentRejectsBadVisibility(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	_, err := core.UpsertAgent(ctx, AgentInput{Name: "x", Visibility: "public"})
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Fatalf("want badInput for visibility=public, got %v", err)
	}
}

func TestUpsertAgentEditsInPlace(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	id := mustAgent(t, core, ctx, AgentInput{Name: "x", Description: "first"})
	id2 := mustAgent(t, core, ctx, AgentInput{Name: "x", Description: "second"})
	if id != id2 {
		t.Fatalf("upsert created a new row (%d != %d) instead of editing", id, id2)
	}
	got := mustAgents(t, core, ctx)
	if len(got) != 1 || got[0].Description != "second" {
		t.Fatalf("edit not applied: %+v", got)
	}
}

func TestDeleteAgentSoftDeletes(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	id := mustAgent(t, core, ctx, AgentInput{Name: "x"})
	if err := core.DeleteAgent(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := mustAgents(t, core, ctx); len(got) != 0 {
		t.Fatalf("soft-deleted agent still listed: %+v", got)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.GetAgent(ctx, 999); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core.SetAgentSkills + GetAgent skill_ids
// ---------------------------------------------------------------------------

func TestSetAgentSkillsAttachesAndReplaces(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	s1 := mustSkill(t, core, ctx, SkillInput{Name: "docslide"})
	s2 := mustSkill(t, core, ctx, SkillInput{Name: "research"})

	// Attach with a duplicate — dedup must collapse it.
	if err := core.SetAgentSkills(ctx, aid, []int{s1, s2, s1}); err != nil {
		t.Fatalf("set skills: %v", err)
	}
	got, err := core.GetAgent(ctx, aid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.SkillIDs) != 2 {
		t.Fatalf("skill_ids = %v, want 2 unique", got.SkillIDs)
	}

	// Replace with just s2 — the set is the new truth, not a merge.
	if err := core.SetAgentSkills(ctx, aid, []int{s2}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = core.GetAgent(ctx, aid)
	if len(got.SkillIDs) != 1 || got.SkillIDs[0] != s2 {
		t.Fatalf("skill_ids = %v, want [%d]", got.SkillIDs, s2)
	}
}

func TestSetAgentSkillsClearsWithEmptySet(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	s1 := mustSkill(t, core, ctx, SkillInput{Name: "docslide"})
	if err := core.SetAgentSkills(ctx, aid, []int{s1}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := core.SetAgentSkills(ctx, aid, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ := core.GetAgent(ctx, aid)
	if len(got.SkillIDs) != 0 {
		t.Fatalf("skills not cleared: %v", got.SkillIDs)
	}
}

func TestSetAgentSkillsRejectsSoftDeletedAgent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	if err := core.DeleteAgent(ctx, aid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := core.SetAgentSkills(ctx, aid, nil); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound on soft-deleted agent, got %v", err)
	}
}

func TestSetAgentSkillsRejectsNonPositiveID(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	var bad badInputError
	if err := core.SetAgentSkills(ctx, aid, []int{0}); !errors.As(err, &bad) {
		t.Fatalf("want badInput for skill id 0, got %v", err)
	}
}

func TestSetAgentSkillsRejectsDeletedSkill(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	s1 := mustSkill(t, core, ctx, SkillInput{Name: "docslide"})
	if err := core.DeleteSkill(ctx, s1); err != nil {
		t.Fatalf("delete skill: %v", err)
	}
	// Linking an already-deleted skill must be rejected (badInput → 400), not silently dropped.
	var bad badInputError
	if err := core.SetAgentSkills(ctx, aid, []int{s1}); !errors.As(err, &bad) {
		t.Fatalf("want badInput for deleted skill, got %v", err)
	}
}

func TestGetAgentExcludesSoftDeletedSkill(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	s1 := mustSkill(t, core, ctx, SkillInput{Name: "docslide"})
	if err := core.SetAgentSkills(ctx, aid, []int{s1}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := core.DeleteSkill(ctx, s1); err != nil {
		t.Fatalf("delete skill: %v", err)
	}
	got, _ := core.GetAgent(ctx, aid)
	if len(got.SkillIDs) != 0 {
		t.Fatalf("soft-deleted skill still attached: %v", got.SkillIDs)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func TestAgentsHTTPRoundTrip(t *testing.T) {
	core, _, _ := newTestCore(t)
	mux := NewSurfaceMux(core, "tok")

	// Create.
	if rec := authedDo(t, mux, "PUT", "/v1/agents", `{"name":"Docslide Writer","instructions":"slides","model":"groq/llama"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT /v1/agents: %d %s", rec.Code, rec.Body)
	}
	// List shows it.
	if rec := authedDo(t, mux, "GET", "/v1/agents", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Docslide Writer") {
		t.Fatalf("GET /v1/agents: %d %s", rec.Code, rec.Body)
	}
}

func TestAgentSkillsHTTP(t *testing.T) {
	core, _, _ := newTestCore(t)
	ctx := context.Background()
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	sid := mustSkill(t, core, ctx, SkillInput{Name: "docslide"})
	mux := NewSurfaceMux(core, "tok")

	// Attach a skill via PUT /v1/agents/{id}/skills.
	body := fmt.Sprintf(`{"skill_ids":[%d]}`, sid)
	if rec := authedDo(t, mux, "PUT", fmt.Sprintf("/v1/agents/%d/skills", aid), body); rec.Code != http.StatusOK {
		t.Fatalf("PUT skills: %d %s", rec.Code, rec.Body)
	}
	// GET /v1/agents/{id} returns the attached skill id.
	rec := authedDo(t, mux, "GET", fmt.Sprintf("/v1/agents/%d", aid), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"skill_ids":[`) {
		t.Fatalf("GET /v1/agents/{id}: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf("[%d]", sid)) {
		t.Errorf("attached skill not reflected: %s", rec.Body)
	}
}

func TestAgentHTTPRejectsBadID(t *testing.T) {
	core, _, _ := newTestCore(t)
	mux := NewSurfaceMux(core, "tok")
	if rec := authedDo(t, mux, "GET", "/v1/agents/abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("GET /v1/agents/abc: status=%d, want 400", rec.Code)
	}
}

func TestAgentDeleteHTTP(t *testing.T) {
	core, _, _ := newTestCore(t)
	ctx := context.Background()
	aid := mustAgent(t, core, ctx, AgentInput{Name: "writer"})
	mux := NewSurfaceMux(core, "tok")
	if rec := authedDo(t, mux, "DELETE", fmt.Sprintf("/v1/agents/%d", aid), ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/agents/{id}: %d %s", rec.Code, rec.Body)
	}
	if got := mustAgents(t, core, ctx); len(got) != 0 {
		t.Errorf("agent not deleted over HTTP: %+v", got)
	}
}
