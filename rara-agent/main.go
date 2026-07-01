package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type config struct {
	store        Store
	exec         Executor
	pollInterval time.Duration
	workBase     string
}

// runOnce claims one task, runs it, and writes the result back.
// Returns (true, nil) if a task was processed, (false, nil) if queue was empty.
func runOnce(ctx context.Context, cfg config) (bool, error) {
	task, err := cfg.store.ClaimTask(ctx)
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if task == nil {
		return false, nil
	}

	markFailed := func(reason string) {
		if uerr := cfg.store.UpdateTask(ctx, task.ID, "failed", "", "", nil, reason); uerr != nil {
			log.Printf("task %d: mark failed: %v (original error: %s)", task.ID, uerr, reason)
		}
	}

	agent, err := cfg.store.FetchAgent(ctx, task.AgentID)
	if err != nil {
		markFailed(err.Error())
		return true, fmt.Errorf("fetch agent %d: %w", task.AgentID, err)
	}

	skills, err := cfg.store.FetchSkills(ctx, agent.SkillIDs)
	if err != nil {
		markFailed(err.Error())
		return true, fmt.Errorf("fetch skills: %w", err)
	}

	tc := TaskCtx{
		TaskID:      task.ID,
		AgentID:     task.AgentID,
		Instruction: task.Instruction,
		ContextRefs: task.ContextRefs,
		Agent:       agent,
		Skills:      skills,
	}

	workDir, err := BuildWorkdir(tc, cfg.workBase)
	if err != nil {
		markFailed(err.Error())
		return true, fmt.Errorf("build workdir: %w", err)
	}
	tc.WorkDir = workDir

	if err := cfg.store.UpdateTask(ctx, task.ID, "running", "", workDir, nil, ""); err != nil {
		return true, fmt.Errorf("mark running: %w", err)
	}

	result, execErr := cfg.exec.Run(ctx, tc)
	if execErr != nil {
		markFailed(execErr.Error())
		return true, nil
	}

	out := result.Output
	if out == nil {
		out = json.RawMessage(`{}`)
	}
	if err := cfg.store.UpdateTask(ctx, task.ID, "done", result.SessionID, result.WorkDir, out, ""); err != nil {
		return true, fmt.Errorf("mark done: %w", err)
	}
	return true, nil
}

func main() {
	dbURL := mustEnv("DATABASE_URL")
	coreURL := mustEnv("CORE_URL")
	coreToken := mustEnv("CORE_TOKEN")

	pollSec := envIntOr("AGENT_POLL_INTERVAL_S", 5)
	claudeBin := envOr("CLAUDE_BIN", "claude")
	workBase := envOr("AGENT_WORK_BASE", "/tmp/rara-agent")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := newPgxStore(ctx, dbURL, coreURL, coreToken)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	cfg := config{
		store:        store,
		exec:         &CLIExecutor{bin: claudeBin},
		pollInterval: time.Duration(pollSec) * time.Second,
		workBase:     workBase,
	}

	log.Printf("rara-agent starting (poll=%ds)", pollSec)
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()
	for {
		ran, err := runOnce(ctx, cfg)
		if err != nil {
			log.Printf("runOnce error: %v", err)
		}
		if ran {
			continue // drain the queue without waiting
		}
		select {
		case <-ctx.Done():
			log.Println("rara-agent: shutting down")
			return
		case <-ticker.C:
		}
	}
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("required env %s not set", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
