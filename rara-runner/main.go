// rara-runner is the activation arm of the 2.0 control plane (ATIVACAO-UNIFICADA.pt-BR.md). Like
// rara-core's `core-job reconcile|surface|...`, it is one binary with role subcommands:
//
//   - agent    — resident per-host daemon (VPC + Mac) that wakes workers on demand: POST /run over
//     the tailnet -> `docker run`. The "portable Cloud Run". (this file ships F1)
//   - dispatch — the perennial VPC service that reads desired state from Neon and wakes via Runner.
//     Lands in F3; not built yet.
//
// rara-runner is the piece that RUNS; rara-addon stays the contract workers import.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const usage = `rara-runner — activation arm of the 2.0 control plane

usage: rara-runner <command>

commands:
  agent    resident per-host daemon: POST /run (tailnet, Bearer) -> docker run
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "agent":
		runAgent()
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

// runAgent wires the agent from env (config is env-only; required vars fail fast — repo convention)
// and serves until killed. Required: RUNNER_ADDR (tailnet bind), RUNNER_TOKEN (Bearer), and
// RUNNER_ALLOWED_IMAGES (app=image,...). RUNNER_DOCKER_BIN overrides the launcher (default "docker").
func runAgent() {
	addr := os.Getenv("RUNNER_ADDR")
	if addr == "" {
		log.Fatalf("RUNNER_ADDR is required (tailnet host:port, never 0.0.0.0)")
	}
	if err := validateListenAddr(addr); err != nil {
		log.Fatalf("invalid RUNNER_ADDR: %v", err)
	}
	token := os.Getenv("RUNNER_TOKEN")
	if token == "" {
		log.Fatalf("RUNNER_TOKEN is required (Bearer auth fails closed)")
	}
	allowed, err := parseAllowlist(os.Getenv("RUNNER_ALLOWED_IMAGES"))
	if err != nil {
		log.Fatalf("invalid RUNNER_ALLOWED_IMAGES: %v", err)
	}
	bin := os.Getenv("RUNNER_DOCKER_BIN")
	if bin == "" {
		bin = "docker"
	}
	if err := validateDockerBin(bin); err != nil {
		log.Fatalf("invalid RUNNER_DOCKER_BIN: %v", err)
	}

	// Signal-aware context: SIGINT/SIGTERM (systemd/launchd lifecycle) cancel it so the listener
	// drains in-flight requests and exits cleanly — mirrors rara-core's main.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := newAgentServer(token, allowed, dockerRunner{bin: bin})
	if err := serveAgent(ctx, addr, h); err != nil {
		log.Fatalf("agent: %v", err)
	}
}
