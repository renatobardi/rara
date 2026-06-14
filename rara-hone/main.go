// rara-hone is the interest_profile reviser — the 2.0 learning loop, extracted from rara-core into
// its own PERIODIC job. It is NOT a claim-worker (no rara-addon SDK, no per-item routing): it is a
// run-once-and-exit batch fired by a systemd timer on the VPC. Each invocation:
//
//  1. reads the ACTIVE interest_profile (the base) + the feedback accumulated since the last
//     revision over the shared Neon tables;
//  2. asks shouldRevise whether the cadence/threshold/debounce say to revise now (and never while
//     a proposal already awaits approval — no stacking);
//  3. if due, runs the deterministic engine for the structured change + the LiteLLM narrator for
//     the prose, and APPENDS the result as a NEW `proposed` version.
//
// hone PROPOSES (append-only, status `proposed`); it NEVER activates. The human APPROVAL of a
// proposal (ActivateInterestProfile) stays in rara-core's surface (MCP/HTTP). The gate cascade
// reads the ACTIVE version, so a proposal is inert until a person approves it there.
//
// Deploy: a native binary on the always-on VPC under a systemd timer (weekly by default), NOT a
// Cloud Run Job — so there is no gate/activation to wake; the timer is the trigger. The trigger
// logic itself still no-ops a run with nothing new to learn from, so the timer can fire freely.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	fs := flag.NewFlagSet("hone", flag.ExitOnError)
	force := fs.Bool("force", false, "bypass the cadence/threshold/debounce gate (still no-ops if there is no new feedback)")
	_ = fs.Parse(os.Args[1:])

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	// Signal-aware context so a SIGTERM during the run (the systemd timer's stop) cancels cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := pgx.Connect(connectCtx, dbURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(ctx)

	cfg := configFromEnv(*force)

	// The narrator is optional: without a LiteLLM gateway the proposal still gets a deterministic
	// template narrative (the structured revision is the part that must always work).
	var narrator ProfileNarrator
	if n, err := newLiteLLMNarrator(); err != nil {
		log.Printf("hone: narrator unavailable (%v); a proposal will carry a template narrative", err)
	} else {
		narrator = n
	}

	version, revised, err := ReviseProfile(ctx, newPgxStore(conn), newPgxDistillationResolver(conn), narrator, time.Now(), cfg)
	switch {
	case errors.Is(err, errVersionExists):
		// A concurrent proposal (a human surface add, or an overlapping run) claimed the version
		// number first. Benign: skip this run cleanly; the next timer fire renumbers off the new
		// max. NOT fatal — exiting non-zero here would needlessly alarm the systemd timer.
		log.Printf("rara-hone: profile version already taken by a concurrent proposal; skipping this run")
		return
	case err != nil:
		log.Fatalf("hone: %v", err)
	}
	if !revised {
		log.Printf("rara-hone: no revision proposed (trigger not met or no new feedback)")
		return
	}
	log.Printf("rara-hone: proposed interest_profile v%d — approve it via the rara-core surface to take effect", version)
}

// configFromEnv builds the reviser config from the conservative defaults, overriding the trigger
// knobs (cadence/threshold/debounce) from the environment so the systemd timer's cadence can be
// tuned without a rebuild. --force collapses the gate (an operator forcing a revision now), but
// the engine still no-ops when there is genuinely no new feedback to learn from.
func configFromEnv(force bool) reviseConfig {
	cfg := defaultReviseConfig()
	if v := os.Getenv("REVISE_CADENCE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Cadence = time.Duration(n) * time.Hour
		}
	}
	if v := os.Getenv("REVISE_FEEDBACK_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FeedbackThreshold = n
		}
	}
	if v := os.Getenv("REVISE_DEBOUNCE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Debounce = time.Duration(n) * time.Hour
		}
	}
	if force {
		cfg.Cadence, cfg.Debounce, cfg.FeedbackThreshold = 0, 0, 1
	}
	return cfg
}
