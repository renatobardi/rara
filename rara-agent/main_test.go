package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FakeExecutor is the Executor test double.
type FakeExecutor struct {
	Output    json.RawMessage
	SessionID string
	Err       error
}

func (f *FakeExecutor) Run(_ context.Context, tc TaskCtx) (ExecResult, error) {
	if f.Err != nil {
		return ExecResult{}, f.Err
	}
	out := f.Output
	if out == nil {
		out = json.RawMessage(`{}`)
	}
	return ExecResult{SessionID: f.SessionID, WorkDir: tc.WorkDir, Output: out}, nil
}

// MockStore satisfies the Store interface for tests.
type MockStore struct {
	task       *Task
	agent      AgentInfo
	skills     []SkillBundle
	claimErr   error
	fetchAgErr error
	fetchSkErr error

	updated []updateCall
}

type updateCall struct {
	id        int
	status    string
	sessionID string
	workDir   string
	result    json.RawMessage
	errMsg    string
}

func (m *MockStore) ClaimTask(_ context.Context) (*Task, error) {
	return m.task, m.claimErr
}
func (m *MockStore) UpdateTask(_ context.Context, id int, status, sessionID, workDir string, result json.RawMessage, errMsg string) error {
	m.updated = append(m.updated, updateCall{id, status, sessionID, workDir, result, errMsg})
	return nil
}
func (m *MockStore) FetchAgent(_ context.Context, _ int) (AgentInfo, error) {
	return m.agent, m.fetchAgErr
}
func (m *MockStore) FetchSkills(_ context.Context, _ []int) ([]SkillBundle, error) {
	return m.skills, m.fetchSkErr
}

func TestRunOneTask(t *testing.T) {
	task := &Task{ID: 1, AgentID: 42, Instruction: "do the thing"}
	agent := AgentInfo{ID: 42, Name: "alice", SkillIDs: []int{}}
	store := &MockStore{task: task, agent: agent}
	exec := &FakeExecutor{SessionID: "sess-abc", Output: json.RawMessage(`{"ok":true}`)}

	cfg := config{store: store, exec: exec, workBase: t.TempDir()}
	ran, err := runOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !ran {
		t.Fatal("expected ran=true")
	}

	if len(store.updated) < 2 {
		t.Fatalf("expected ≥2 UpdateTask calls, got %d", len(store.updated))
	}
	// first call: running
	if store.updated[0].status != "running" {
		t.Errorf("first update status = %q, want running", store.updated[0].status)
	}
	// last call: done
	last := store.updated[len(store.updated)-1]
	if last.status != "done" {
		t.Errorf("last update status = %q, want done", last.status)
	}
	if last.sessionID != "sess-abc" {
		t.Errorf("session_id = %q, want sess-abc", last.sessionID)
	}
}

func TestRunOnce_EmptyQueue(t *testing.T) {
	store := &MockStore{task: nil}
	cfg := config{store: store, exec: &FakeExecutor{}, workBase: t.TempDir()}
	ran, err := runOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if ran {
		t.Fatal("expected ran=false on empty queue")
	}
	if len(store.updated) != 0 {
		t.Fatalf("expected no UpdateTask calls, got %d", len(store.updated))
	}
}

func TestRunOnce_ExecutorError_MarksTaskFailed(t *testing.T) {
	task := &Task{ID: 7, AgentID: 1, Instruction: "fail me"}
	store := &MockStore{task: task, agent: AgentInfo{ID: 1}}
	exec := &FakeExecutor{Err: errors.New("claude exploded")}

	cfg := config{store: store, exec: exec, workBase: t.TempDir()}
	ran, err := runOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !ran {
		t.Fatal("expected ran=true even on executor error")
	}

	var found bool
	for _, u := range store.updated {
		if u.status == "failed" && u.errMsg == "claude exploded" {
			found = true
		}
	}
	if !found {
		t.Errorf("no failed UpdateTask with error message, calls: %+v", store.updated)
	}
}

func TestBuildWorkdir_TrustedSkillWritesFiles(t *testing.T) {
	tc := TaskCtx{
		TaskID: 1,
		Agent:  AgentInfo{Name: "testbot", Instructions: "You are a test bot."},
		Skills: []SkillBundle{
			{
				ID:      10,
				Name:    "docslide",
				Content: "# SKILL: docslide\nGenerate slides.",
				Trusted: true,
				Files:   []SkillFile{{Path: "generate.py", Content: "print('hello')"}},
			},
		},
	}

	dir, err := BuildWorkdir(tc, t.TempDir())
	if err != nil {
		t.Fatalf("BuildWorkdir: %v", err)
	}
	defer os.RemoveAll(dir)

	for _, name := range []string{"CLAUDE.md", "SKILL.md", "generate.py"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not written", name)
		}
	}
}

func TestBuildWorkdir_UntrustedSkillNoFiles(t *testing.T) {
	tc := TaskCtx{
		TaskID: 2,
		Agent:  AgentInfo{Name: "bot", Instructions: "You help."},
		Skills: []SkillBundle{
			{
				ID:      20,
				Name:    "external",
				Content: "# SKILL: external\nSome content.",
				Trusted: false,
				Files:   []SkillFile{{Path: "hack.py", Content: "evil"}},
			},
		},
	}

	dir, err := BuildWorkdir(tc, t.TempDir())
	if err != nil {
		t.Fatalf("BuildWorkdir: %v", err)
	}
	defer os.RemoveAll(dir)

	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Error("SKILL.md not written for untrusted skill")
	}
	if _, err := os.Stat(filepath.Join(dir, "hack.py")); err == nil {
		t.Error("hack.py must NOT be written for untrusted skill")
	}
}

func TestBuildWorkdir_PathTraversalRejected(t *testing.T) {
	tc := TaskCtx{
		TaskID: 3,
		Agent:  AgentInfo{Name: "bot"},
		Skills: []SkillBundle{
			{
				ID:      30,
				Name:    "evil",
				Content: "evil",
				Trusted: true,
				Files:   []SkillFile{{Path: "../../../etc/passwd", Content: "pwned"}},
			},
		},
	}
	if _, err := BuildWorkdir(tc, t.TempDir()); err == nil {
		t.Error("expected error for path traversal, got nil")
	}
}

func TestBuildWorkdir_WritesMCPConfig(t *testing.T) {
	mcpCfg := []byte(`{"mcpServers":{"neon":{"command":"npx","args":["-y","@neondatabase/mcp"]}}}`)
	tc := TaskCtx{
		TaskID: 4,
		Agent:  AgentInfo{Name: "mcp-bot", MCPConfig: mcpCfg},
	}
	dir, err := BuildWorkdir(tc, t.TempDir())
	if err != nil {
		t.Fatalf("BuildWorkdir: %v", err)
	}
	defer os.RemoveAll(dir)

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf(".claude/settings.json not written: %v", err)
	}
	if string(got) != string(mcpCfg) {
		t.Errorf("settings.json = %s, want %s", got, mcpCfg)
	}
}

func TestCLIExecutor_InvokesClaude(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\necho '{\"session_id\":\"test-sess\",\"result\":{\"ok\":true}}'"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	e := &CLIExecutor{bin: bin}
	tc := TaskCtx{
		TaskID:      99,
		Instruction: "hello",
		WorkDir:     t.TempDir(),
		Agent:       AgentInfo{Name: "bot"},
	}
	result, err := e.Run(context.Background(), tc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID != "test-sess" {
		t.Errorf("session_id: got %q, want test-sess", result.SessionID)
	}
}

func TestCLIExecutor_NonZeroExitIsError(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	e := &CLIExecutor{bin: bin}
	_, err := e.Run(context.Background(), TaskCtx{WorkDir: t.TempDir(), Instruction: "fail"})
	if err == nil {
		t.Error("expected error on non-zero exit, got nil")
	}
}
