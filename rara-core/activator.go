// activator.go — the real Activators: the orchestrator side of symmetric activation (P1b).
//
// Trabalho = pull always; ativação = symmetric. The reconciler writes an assignment (a pending
// item_step with a chosen provider) and then ASKS that provider to start now instead of waiting for
// its next poll tick. How you "ask" depends on the provider's shape:
//
//   - runtime=cloudrun (on_demand, scale-to-zero) -> Cloud Run Jobs `run`: an authenticated POST that
//     starts a new execution of the job that serves the provider. The job is named after the provider
//     (one deployable unit per provider; an optional CLOUD_RUN_JOB_PREFIX namespaces them).
//   - activation=resident (the Mac scribe, a VPC worker) -> a tailnet poke: POST <poke_url>/poke with
//     a Bearer token, which the worker's poke listener (rara-addon/poke.go) turns into a drain.
//
// Both are BEST-EFFORT by design (the architecture's §4): a failed activation is logged, never fatal.
// The pending row stands and the worker's own poll — the safety net — drains it on the next tick. A
// poke cannot wake a sleeping Mac; the Cloud Run call can fail on a transient API error. Neither must
// stall the reconciler, so Activate errors only bubble up to a log line (reconciler.activate).
//
// Everything that touches the network is injected (httpDoer, tokenSource) so the TDD fakes assert the
// dispatch + the wire shape without a real HTTP call or a real GCP credential.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// httpDoer is the subset of *http.Client the activators use, injected so tests substitute a fake
// transport and assert the request without a live endpoint.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// tokenSource yields a fresh OAuth2 bearer for the Cloud Run Jobs API. It is a seam: the default
// (envTokenSource) reads a token from env, and a deploy can slot in a refreshing service-account
// source (run.jobs.run) without changing the call logic below.
type tokenSource func(ctx context.Context) (string, error)

// dispatchActivator routes an assignment to the right wake mechanism by provider shape. Each branch
// is best-effort: an unconfigured mechanism (nil sub-activator) logs and returns nil — not an error
// — because the worker poll is the safety net. A provider that fits no branch (e.g. an on_demand VPC
// provider that is never routed per item) is a no-op.
type dispatchActivator struct {
	cloudRun Activator // wakes runtime=cloudrun providers; nil = unconfigured
	poke     Activator // wakes activation=resident providers; nil = unconfigured
}

func (d dispatchActivator) Activate(ctx context.Context, p Provider) error {
	switch {
	case p.Runtime == runtimeCloudRun:
		if d.cloudRun == nil {
			log.Printf("activate %s: cloud run not configured (set CLOUD_RUN_PROJECT/CLOUD_RUN_REGION); relying on poll", p.Name)
			return nil
		}
		return d.cloudRun.Activate(ctx, p)
	case p.Activation == activationResident:
		if d.poke == nil {
			log.Printf("activate %s: poke not configured (set POKE_AUTH_TOKEN); relying on poll", p.Name)
			return nil
		}
		return d.poke.Activate(ctx, p)
	default:
		log.Printf("activate %s: no activation path (runtime=%s activation=%s); relying on poll", p.Name, p.Runtime, p.Activation)
		return nil
	}
}

// cloudRunActivator wakes an on_demand cloudrun provider by starting an execution of its Cloud Run
// Job. project/region/credentials are env config (shared across every job); the job NAME is derived
// per-provider (jobPrefix + provider name), so one reconciler wakes the whole fleet without per-row
// addressing. The provider IS the deployable unit (one job per provider, same image, different
// --provider arg), so naming the job after the provider is exact, not a guess.
type cloudRunActivator struct {
	project   string
	region    string
	jobPrefix string // optional namespace, e.g. "rara-"; usually empty
	http      httpDoer
	token     tokenSource
}

func (a *cloudRunActivator) Activate(ctx context.Context, p Provider) error {
	job := a.jobPrefix + p.Name
	url := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s:run",
		a.project, a.region, job)

	tok, err := a.token(ctx)
	if err != nil {
		return fmt.Errorf("cloud run token: %w", err)
	}
	// An empty body runs the job with its deployed defaults (the `--provider` arg the addon needs is
	// baked into the job at deploy, not passed per run).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloud run run %s: %w", job, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("cloud run run %s: status %d: %s", job, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) // drain so the connection can be reused
	return nil
}

// pokeActivator wakes a resident provider by POSTing to its tailnet poke endpoint (the worker side is
// rara-addon/poke.go). The endpoint is per-provider (providers.poke_url); the Bearer token is shared
// across the tailnet (POKE_AUTH_TOKEN, matched against each worker's POKE_TOKEN). A provider with no
// poke_url is skipped (the slow poll covers it) — that is a no-op, not an error.
type pokeActivator struct {
	token string
	http  httpDoer
}

func (a *pokeActivator) Activate(ctx context.Context, p Provider) error {
	if p.PokeURL == "" {
		return nil // resident relying on the slow poll alone; nothing to poke
	}
	url := strings.TrimRight(p.PokeURL, "/") + "/poke"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	// The worker's listener fails CLOSED on an empty token, so always send the Bearer.
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("poke %s: %w", p.Name, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		return fmt.Errorf("poke %s: status %d", p.Name, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	return nil
}

// newActivatorFromEnv builds the production activator from env config. Cloud Run activation needs
// CLOUD_RUN_PROJECT + CLOUD_RUN_REGION (and a token via CLOUD_RUN_OAUTH_TOKEN); poke activation needs
// POKE_AUTH_TOKEN. Whatever is configured is wired; the rest stays a logged no-op. With NOTHING
// configured it returns the pure logActivator, so the reconciler still runs (and the worker poll
// drains the queue) on a box that has not been given activation credentials yet.
func newActivatorFromEnv() Activator {
	client := &http.Client{Timeout: envDuration("ACTIVATE_TIMEOUT_SECONDS", 10*time.Second)}
	d := dispatchActivator{}

	if project, region := os.Getenv("CLOUD_RUN_PROJECT"), os.Getenv("CLOUD_RUN_REGION"); project != "" && region != "" {
		d.cloudRun = &cloudRunActivator{
			project:   project,
			region:    region,
			jobPrefix: os.Getenv("CLOUD_RUN_JOB_PREFIX"),
			http:      client,
			token:     envTokenSource("CLOUD_RUN_OAUTH_TOKEN"),
		}
	}
	if tok := os.Getenv("POKE_AUTH_TOKEN"); tok != "" {
		d.poke = &pokeActivator{token: tok, http: client}
	}

	if d.cloudRun == nil && d.poke == nil {
		log.Printf("activator: no activation configured (CLOUD_RUN_* / POKE_AUTH_TOKEN unset); pull-only (poll is the safety net)")
		return logActivator{}
	}
	return d
}

// envTokenSource is the default tokenSource: read an OAuth2 access token (with run.jobs.run) from
// env. Suitable for a short-lived/manual token; a deploy wires a refreshing service-account source
// in its place without touching cloudRunActivator.
func envTokenSource(key string) tokenSource {
	return func(_ context.Context) (string, error) {
		tok := os.Getenv(key)
		if tok == "" {
			return "", fmt.Errorf("%s is empty (need an OAuth2 access token with run.jobs.run)", key)
		}
		return tok, nil
	}
}
