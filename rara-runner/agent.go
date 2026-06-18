// agent.go — `rara-runner agent`: the "portable Cloud Run" — a resident per-host daemon (VPC + Mac)
// that wakes workers on demand. It receives an authenticated POST /run over the tailnet and starts
// the worker's container locally, the same contract the GCP `jobs:run` serves natively (see §4/§7 of
// ATIVACAO-UNIFICADA.pt-BR.md).
//
// Security is the whole point — this runs containers on a personal machine (the Mac):
//   - Listener binds tailnet-only (RUNNER_ADDR); a wildcard bind (0.0.0.0 / :port) is refused.
//   - Bearer auth fails CLOSED (constant-time, mirrors rara-addon/poke.go): an empty token rejects all.
//   - The image is resolved ONLY from the allowlist (app -> digest-pinned image); a body never names
//     an image. The runner trusts no secret from the body either — env in the body is per-run config.
//
// The container launcher is a seam (ContainerRunner) so tests assert dispatch + the resolved image
// without a real docker.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ContainerRunner starts the worker container for a resolved (already-allowlisted) image with the
// given per-run env. ctx carries the request deadline so a stalled docker binary doesn't hold the
// handler goroutine open indefinitely. Implemented by dockerRunner; faked in tests.
type ContainerRunner interface {
	Run(ctx context.Context, image string, env map[string]string) error
}

// runRequest is the POST /run body — the wire form of the control plane's RunRequest (rara-core F0).
// The agent acts on the App+Env subset: App names the app to wake (matched against the allowlist),
// never an image — the image is the runner's pinned choice, not the caller's; Env is non-secret
// per-run config, never a trusted secret. The dispatcher's reserved fields (Provider, Capability,
// ItemStepID) are accepted and ignored here (lenient decode), so a full RunRequest wakes the agent
// unchanged when F3 lands.
type runRequest struct {
	App string            `json:"app"`
	Env map[string]string `json:"env"`
}

// newAgentServer builds the HTTP handler: GET /healthz (no auth) and POST /run (Bearer + allowlist).
// token is the shared tailnet bearer (empty => everything is rejected, fail-closed). allowed maps an
// app name to its digest-pinned image.
func newAgentServer(token string, allowed map[string]string, runner ContainerRunner) http.Handler {
	want := []byte(token)
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("healthz write: %v", err)
		}
	})

	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		got, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req runRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if req.App == "" {
			http.Error(w, "app is required", http.StatusBadRequest)
			return
		}
		image, ok := allowed[req.App]
		if !ok {
			http.Error(w, "app not in allowlist", http.StatusForbidden)
			return
		}
		// Env reaches `docker run -e KEY=VAL`; reject a malformed name at the boundary so a bad
		// request is a clear 400, not an opaque docker failure after we've already answered 202.
		if err := validateEnvKeys(req.Env); err != nil {
			http.Error(w, "invalid env key", http.StatusBadRequest)
			return
		}

		if err := runner.Run(r.Context(), image, req.Env); err != nil {
			log.Printf("run %s (%s): %v", req.App, image, err)
			http.Error(w, "failed to start container", http.StatusInternalServerError)
			return
		}
		log.Printf("run %s -> %s (%d env)", req.App, image, len(req.Env))
		w.WriteHeader(http.StatusAccepted)
	})

	return mux
}

// validateEnvKeys returns an error if any key in env is not a valid POSIX env var name. Called from
// the /run handler to validate body-supplied keys before they reach `docker run -e`.
func validateEnvKeys(env map[string]string) error {
	for k := range env {
		if !isValidEnvKey(k) {
			return fmt.Errorf("invalid env key %q", k)
		}
	}
	return nil
}

// isValidEnvKey reports whether k is a POSIX-shaped env var name ([A-Za-z_][A-Za-z0-9_]*). Used to
// validate body-supplied env keys before they become `docker run -e` flags.
func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		switch {
		case c == '_':
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// validateDockerBin constrains RUNNER_DOCKER_BIN to a known launcher ("docker"/"podman") or an
// absolute path. Defense in depth: env is operator-set (not request data), but pinning the launcher
// stops a relative name resolving an attacker-planted binary off PATH.
func validateDockerBin(bin string) error {
	switch bin {
	case "docker", "podman":
		return nil
	}
	if filepath.IsAbs(bin) {
		return nil
	}
	return fmt.Errorf("RUNNER_DOCKER_BIN %q: use \"docker\", \"podman\", or an absolute path", bin)
}

// dockerRunner launches the worker via `docker run`. Detached (-d) and --rm: the agent's job is to
// START the container and answer 202, not to babysit it — the worker pulls its own work and exits.
type dockerRunner struct {
	bin string // RUNNER_DOCKER_BIN; default "docker" (e.g. "podman" for rootless on the Mac)
}

func (d dockerRunner) Run(ctx context.Context, image string, env map[string]string) error {
	args := []string{"run", "-d", "--rm"}
	for k, v := range env {
		args = append(args, "-e", k+"="+v) // exec passes argv directly — no shell, no injection
	}
	args = append(args, image)
	if out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s run: %w: %s", d.bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tailscale4 and tailscale6 are the two address ranges Tailscale uses for its mesh IPs
// (CGNAT 100.64.0.0/10 and the Tailscale ULA prefix fd7a:115c:a1e0::/48). validateListenAddr
// accepts only loopback and these two ranges so RUNNER_ADDR can never be a LAN or public IP.
var (
	tailscale4 = mustParseCIDR("100.64.0.0/10")
	tailscale6 = mustParseCIDR("fd7a:115c:a1e0::/48")
)

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// validateListenAddr refuses any bind that would expose the runner outside the tailnet. Wildcards
// (0.0.0.0/::/empty host) and non-IP hostnames are rejected outright; an explicit IP must be
// loopback or inside one of the two Tailscale address ranges (100.64.0.0/10 or fd7a:115c:a1e0::/48).
func validateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("RUNNER_ADDR %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("RUNNER_ADDR %q binds all interfaces; use an explicit tailnet IP", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("RUNNER_ADDR %q: host must be an explicit IP, not a hostname", addr)
	}
	if ip.IsLoopback() || tailscale4.Contains(ip) || tailscale6.Contains(ip) {
		return nil
	}
	return fmt.Errorf("RUNNER_ADDR %q: must be a loopback or Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48)", addr)
}

// parseAllowlist parses RUNNER_ALLOWED_IMAGES — comma-separated "app=image" pairs — into the app->image
// map. It is the trust boundary for which images may run, so it fails fast at startup: a malformed or
// empty list, an image not pinned by digest (@sha256:), or a duplicate app is an error — never a
// silently empty or ambiguous allowlist.
func parseAllowlist(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		app, image, ok := strings.Cut(pair, "=")
		app, image = strings.TrimSpace(app), strings.TrimSpace(image)
		if !ok || app == "" || image == "" {
			return nil, fmt.Errorf("RUNNER_ALLOWED_IMAGES entry %q: want app=image", pair)
		}
		if !strings.Contains(image, "@sha256:") {
			return nil, fmt.Errorf("RUNNER_ALLOWED_IMAGES %q: image must be pinned by digest (@sha256:)", app)
		}
		if _, dup := out[app]; dup {
			return nil, fmt.Errorf("RUNNER_ALLOWED_IMAGES: duplicate app %q", app)
		}
		out[app] = image
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("RUNNER_ALLOWED_IMAGES is empty; nothing can be run")
	}
	return out, nil
}

// serveAgent binds the tailnet listener and serves until ctx is cancelled (SIGTERM from
// systemd/launchd), then drains in-flight requests via Shutdown. addr is assumed validated
// (validateListenAddr) by the caller. The timeouts bound a slow/malicious tailnet client.
func serveAgent(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("rara-runner agent shutdown: %v", err)
		}
	}()
	log.Printf("rara-runner agent listening on %s", addr)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed { // a clean Shutdown is success, not a failure
		return nil
	}
	return err
}
