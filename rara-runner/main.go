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
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const usage = `rara-runner — activation arm of the 2.0 control plane

usage: rara-runner <command>

commands:
  agent     resident per-host daemon: POST /run (tailnet, Bearer) -> docker run
  dispatch  perennial VPC service: reads Neon assigned steps -> wakes providers
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "agent":
		runAgent()
	case "dispatch":
		runDispatch()
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

// runDispatch reads desired state from Neon and wakes providers in a loop.
// Required env: DATABASE_URL (Neon pgx DSN).
// Optional: DISPATCH_INTERVAL_SECONDS (default 5).
func runDispatch() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("dispatch: db connect: %v", err)
	}
	defer pool.Close()

	interval := 5 * time.Second
	if v := os.Getenv("DISPATCH_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}

	db := &pgxDispatchDB{pool: pool}
	runner := newDispatchRunnerFromEnv()
	d := &Dispatcher{db: db, runner: runner}

	log.Printf("rara-runner dispatch: starting (interval=%s)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := d.DispatchOnce(ctx); err != nil {
			log.Printf("dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Printf("rara-runner dispatch: shutting down")
			return
		case <-ticker.C:
		}
	}
}
