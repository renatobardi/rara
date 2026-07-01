package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Task is the work item claimed from agent_tasks.
type Task struct {
	ID          int
	AgentID     int
	Instruction string
	ContextRefs []byte // raw JSON array
}

// AgentInfo holds the agent's runtime config, fetched from rara-core.
type AgentInfo struct {
	ID           int
	Name         string
	Model        string            // "kind/model" upstream
	MCPConfig    []byte            // raw JSON object (mcp_config)
	CustomEnv    map[string]string // custom_env from agent row
	CustomArgs   []string          // custom_args from agent row
	Instructions string            // system prompt written to CLAUDE.md
	Executor     string            // "cli" | "gateway"
	SkillIDs     []int
}

// SkillFile is one supporting file in a skill bundle.
type SkillFile struct {
	Path    string
	Content string
}

// SkillBundle is one skill with its content and optional supporting files.
type SkillBundle struct {
	ID      int
	Name    string
	Content string // SKILL.md text
	Trusted bool
	Files   []SkillFile // only populated + written when Trusted=true
}

// TaskCtx is the full assembled context passed to an Executor.
type TaskCtx struct {
	TaskID      int
	AgentID     int
	Instruction string
	ContextRefs []byte
	Agent       AgentInfo
	Skills      []SkillBundle
	WorkDir     string // pre-assembled workdir path (set by the loop)
}

// ExecResult is what the Executor returns on success.
type ExecResult struct {
	SessionID string
	WorkDir   string
	Output    json.RawMessage
}

// Executor runs an agent task and returns its result.
type Executor interface {
	Run(ctx context.Context, tc TaskCtx) (ExecResult, error)
}

// daemonEnv returns os.Environ() minus daemon-specific secrets so they are
// never visible to the claude subprocess or any tool it invokes.
var daemonSecrets = map[string]bool{
	"DATABASE_URL": true,
	"CORE_URL":     true,
	"CORE_TOKEN":   true,
}

func daemonEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		key := kv
		if i := len(kv); i > 0 {
			for j := 0; j < i; j++ {
				if kv[j] == '=' {
					key = kv[:j]
					break
				}
			}
		}
		if !daemonSecrets[key] {
			env = append(env, kv)
		}
	}
	return env
}

// CLIExecutor invokes the Claude Code CLI (`claude -p`) in the pre-built workdir.
type CLIExecutor struct {
	bin string // path to `claude` binary
}

// Run spawns `claude -p <instruction> --output-format json` in tc.WorkDir and parses the result.
func (e *CLIExecutor) Run(ctx context.Context, tc TaskCtx) (ExecResult, error) {
	args := []string{"-p", tc.Instruction, "--output-format", "json"}
	if tc.Agent.Model != "" {
		args = append(args, "--model", tc.Agent.Model)
	}
	args = append(args, tc.Agent.CustomArgs...)

	cmd := exec.CommandContext(ctx, e.bin, args...) //nolint:gosec — bin is operator-controlled
	cmd.Dir = tc.WorkDir
	// Pass a filtered env: strip daemon secrets (DATABASE_URL, CORE_TOKEN, …) so the
	// subprocess and any tool it spawns cannot read them from the environment.
	cmd.Env = daemonEnv()
	for k, v := range tc.Agent.CustomEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.Output()
	if err != nil {
		return ExecResult{WorkDir: tc.WorkDir}, fmt.Errorf("claude exit: %w", err)
	}

	// claude --output-format json shape: {"session_id":"...","result":<any>}
	var envelope struct {
		SessionID string          `json:"session_id"`
		Result    json.RawMessage `json:"result"`
	}
	if jsonErr := json.Unmarshal(out, &envelope); jsonErr != nil {
		// Non-JSON output — store raw string.
		raw, _ := json.Marshal(string(out))
		return ExecResult{WorkDir: tc.WorkDir, Output: raw}, nil
	}
	return ExecResult{SessionID: envelope.SessionID, WorkDir: tc.WorkDir, Output: envelope.Result}, nil
}
