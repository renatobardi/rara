// runner.go — the dispatcher's transport layer. Reads assigned item_steps from Neon and wakes the
// right provider by shape: runtime=cloudrun fires a Cloud Run Jobs `run`; a resident provider with
// a runner_url gets a POST to the rara-runner agent on the tailnet. The reconciler only persists
// assignments; this file closes the gap by sending the actual wake.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Provider shape constants, mirroring rara-core — no import, separate modules.
const (
	runtimeCloudRun = "cloudrun"
	runtimeLocal    = "local"
	runtimeVPC      = "vpc"
)

// RunRequest is the unit of work the dispatcher passes to Runner.Run: wake App for Capability,
// routing via Runtime/Activation/RunnerURL, with per-run Env overrides and tracing via ItemStepID.
type RunRequest struct {
	App        string
	Runtime    string
	Activation string
	RunnerURL  string            // tailnet URL of rara-runner agent for resident providers
	Env        map[string]string // per-run config passed as Cloud Run overrides or docker -e flags
	ItemStepID int
}

// Runner wakes a provider so its worker can start pulling. dispatchRunner routes by provider shape;
// individual transports (cloudRunTransport, hostRunnerTransport) handle the wire protocol. Best-effort:
// the worker's own poll is the safety net — a failed wake is logged, never fatal.
type Runner interface {
	Run(ctx context.Context, req RunRequest) error
}

// tokenSource yields a fresh OAuth2 bearer on every call. It is the injectable seam: production
// uses cloudRunTokenSource (ADC-backed, auto-renewing); tests inject a fake to avoid I/O.
type tokenSource func(ctx context.Context) (string, error)

// httpDoer is the HTTP client seam, injectable for tests.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// dispatchRunner routes a RunRequest to the right transport by provider shape.
//   - runtime=cloudrun  → cloudRun.Run  (Cloud Run Jobs `jobs:run`)
//   - non-cloudrun with a runner_url → host.Run (rara-runner agent on tailnet)
//   - neither configured → log no-op (poll is the safety net)
type dispatchRunner struct {
	cloudRun Runner
	host     Runner
}

func (d dispatchRunner) Run(ctx context.Context, req RunRequest) error {
	switch {
	case req.Runtime == runtimeCloudRun:
		if d.cloudRun == nil {
			log.Printf("dispatch %s: cloud run transport not configured; relying on poll", req.App)
			return nil
		}
		return d.cloudRun.Run(ctx, req)
	case req.RunnerURL != "":
		if d.host == nil {
			log.Printf("dispatch %s: host transport not configured; relying on poll", req.App)
			return nil
		}
		return d.host.Run(ctx, req)
	default:
		log.Printf("dispatch %s: no transport path (runtime=%s, runner_url empty); relying on poll", req.App, req.Runtime)
		return nil
	}
}

// cloudRunTransport wakes an on_demand Cloud Run provider by firing a Cloud Run Jobs v2 `run`. The
// job name is jobPrefix + RunRequest.App. Env overrides are sent in the request body so the worker
// can read per-run config without a separate DB round-trip.
type cloudRunTransport struct {
	project   string
	region    string
	jobPrefix string
	http      httpDoer
	token     tokenSource
}

func (tr *cloudRunTransport) Run(ctx context.Context, req RunRequest) error {
	job := tr.jobPrefix + req.App
	endpoint := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s:run",
		tr.project, tr.region, url.PathEscape(job))

	tok, err := tr.token(ctx)
	if err != nil {
		return fmt.Errorf("cloud run token for %s: %w", job, err)
	}

	body := dispatchRunBody(req.Env)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("cloud run request for %s: %w", job, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := tr.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("cloud run run %s: %w", job, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("cloud run run %s: status %d: %s", job, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// dispatchRunBody returns the Cloud Run Jobs v2 `run` body. Empty env → `{}` (use job defaults).
// Non-empty → containerOverrides so the worker reads per-run config from its own env.
func dispatchRunBody(env map[string]string) string {
	if len(env) == 0 {
		return "{}"
	}
	type envVar struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	vars := make([]envVar, 0, len(env))
	for k, v := range env {
		vars = append(vars, envVar{Name: k, Value: v})
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	b, _ := json.Marshal(map[string]any{
		"overrides": map[string]any{
			"containerOverrides": []map[string]any{{"env": vars}},
		},
	})
	return string(b)
}

// hostRunnerTransport wakes a resident provider by POSTing to its rara-runner agent endpoint over
// the tailnet. The body is a JSON-encoded runRequest (app + env) — the same wire the agent expects
// from POST /run (agent.go). The Bearer token is the shared tailnet secret.
type hostRunnerTransport struct {
	token string
	http  httpDoer
}

func (tr *hostRunnerTransport) Run(ctx context.Context, req RunRequest) error {
	endpoint := strings.TrimRight(req.RunnerURL, "/") + "/run"
	// runner_url is operator config, but it carries the shared RUNNER_TOKEN bearer — so guard the
	// trust boundary: only POST to a well-formed http(s) endpoint with a host. A scheme-confused
	// (file://, gopher://) or hostless url would leak the token, so reject it before sending.
	// (A host allowlist — e.g. restricting to the tailnet CIDR — is the upgrade path if runner_url
	// ever stops being operator-curated data entered into the providers table by an admin.)
	if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("host runner %s: invalid runner_url %q", req.App, req.RunnerURL)
	}

	type wireReq struct {
		App string            `json:"app"`
		Env map[string]string `json:"env,omitempty"`
	}
	body, err := json.Marshal(wireReq{App: req.App, Env: req.Env})
	if err != nil {
		return fmt.Errorf("host runner marshal for %s: %w", req.App, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("host runner request for %s: %w", req.App, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+tr.token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := tr.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("host runner run %s: %w", req.App, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		return fmt.Errorf("host runner run %s: status %d", req.App, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	return nil
}

// cloudRunTokenSource returns a tokenSource backed by ADC for long-lived services.
//
// Prefers CLOUD_RUN_OAUTH_TOKEN (static override for dev/manual testing). Otherwise uses adc: when
// nil, google.DefaultTokenSource resolves Application Default Credentials — on Cloud Run / GCE this
// is the metadata server of the attached SA. The oauth2.TokenSource caches and auto-renews so each
// Run call gets a fresh bearer without a metadata round-trip every time. Tests inject a fake
// oauth2.TokenSource via adc to avoid I/O.
func cloudRunTokenSource(adc oauth2.TokenSource) tokenSource {
	if tok := os.Getenv("CLOUD_RUN_OAUTH_TOKEN"); tok != "" {
		return func(_ context.Context) (string, error) { return tok, nil }
	}
	if adc == nil {
		var err error
		adc, err = google.DefaultTokenSource(context.Background(),
			"https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			log.Printf("dispatch: ADC unavailable (set CLOUD_RUN_OAUTH_TOKEN or attach a SA); cloud run wakes will fail: %v", err)
			return func(_ context.Context) (string, error) {
				return "", fmt.Errorf("cloud run token: no credential (set CLOUD_RUN_OAUTH_TOKEN or attach a SA): %w", err)
			}
		}
	}
	return func(ctx context.Context) (string, error) {
		// oauth2.TokenSource.Token() takes no context, so honour caller cancellation here.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		tok, err := adc.Token()
		if err != nil {
			return "", fmt.Errorf("cloud run ADC token: %w", err)
		}
		return tok.AccessToken, nil
	}
}

// newDispatchRunnerFromEnv builds the production dispatchRunner from env config.
// CLOUD_RUN_PROJECT + CLOUD_RUN_REGION → cloudRunTransport with ADC (CLOUD_RUN_OAUTH_TOKEN overrides for dev).
// RUNNER_TOKEN → hostRunnerTransport (shared tailnet bearer, same as agent).
func newDispatchRunnerFromEnv() Runner {
	client := &http.Client{Timeout: 30 * time.Second}
	d := dispatchRunner{}

	project, region := os.Getenv("CLOUD_RUN_PROJECT"), os.Getenv("CLOUD_RUN_REGION")
	if (project == "") != (region == "") {
		// Partial config silently disables the cloud run transport and masks an operator mistake;
		// the pair is all-or-nothing, so fail fast (config is env-only, per CLAUDE.md).
		log.Fatalf("dispatch: CLOUD_RUN_PROJECT and CLOUD_RUN_REGION must be set together")
	}
	if project != "" {
		d.cloudRun = &cloudRunTransport{
			project:   project,
			region:    region,
			jobPrefix: os.Getenv("CLOUD_RUN_JOB_PREFIX"),
			http:      client,
			token:     cloudRunTokenSource(nil),
		}
	}
	if tok := os.Getenv("RUNNER_TOKEN"); tok != "" {
		d.host = &hostRunnerTransport{token: tok, http: client}
	} else {
		log.Printf("dispatch: RUNNER_TOKEN unset; host runner transport disabled")
	}

	if d.cloudRun == nil && d.host == nil {
		log.Fatalf("dispatch: no transport configured (set CLOUD_RUN_PROJECT/REGION or RUNNER_TOKEN)")
	}
	return d
}
