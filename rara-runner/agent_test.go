package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

var errFakeDocker = errors.New("docker boom")

// fakeRunner records the last Run call so tests assert the dispatched image + env without docker.
// The mutex makes it safe under the concurrent test (and `go test -race`).
type fakeRunner struct {
	mu       sync.Mutex
	calls    int
	gotImage string
	gotEnv   map[string]string
	err      error
}

func (f *fakeRunner) Run(image string, env map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotImage = image
	f.gotEnv = env
	return f.err
}

const (
	testToken = "s3cret-tailnet-token"
	testApp   = "rara-distill"
	testImage = "us-docker.pkg.dev/p/r/rara-distill@sha256:deadbeef"
)

func newTestServer(t *testing.T, token string, runner ContainerRunner) http.Handler {
	t.Helper()
	return newAgentServer(token, map[string]string{testApp: testImage}, runner)
}

func post(t *testing.T, h http.Handler, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	newTestServer(t, testToken, &fakeRunner{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", rr.Code)
	}
}

func TestRunRejectsMissingToken(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), "", `{"app":"rara-distill"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", rr.Code)
	}
	if f.calls != 0 {
		t.Fatalf("runner must not run on auth failure (calls=%d)", f.calls)
	}
}

func TestRunRejectsWrongToken(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), "wrong", `{"app":"rara-distill"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rr.Code)
	}
	if f.calls != 0 {
		t.Fatalf("runner must not run on auth failure")
	}
}

// Fail-closed: a server configured with an empty token rejects everything, even an empty bearer.
func TestRunFailsClosedOnEmptyServerToken(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, "", f), "", `{"app":"rara-distill"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("empty server token: got %d, want 401", rr.Code)
	}
}

func TestRunRejectsAppNotInAllowlist(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), testToken, `{"app":"evil-image-runner"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unknown app: got %d, want 403", rr.Code)
	}
	if f.calls != 0 {
		t.Fatalf("runner must not run for a non-allowlisted app")
	}
}

func TestRunValidAppDispatchesPinnedImage(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), testToken,
		`{"app":"rara-distill","env":{"DISTILL_RECIPE":"opus","FOO":"bar"}}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("valid run: got %d, want 202", rr.Code)
	}
	if f.calls != 1 {
		t.Fatalf("runner calls: got %d, want 1", f.calls)
	}
	if f.gotImage != testImage {
		t.Fatalf("image: got %q, want pinned %q", f.gotImage, testImage)
	}
	if f.gotEnv["DISTILL_RECIPE"] != "opus" || f.gotEnv["FOO"] != "bar" {
		t.Fatalf("env not forwarded: %v", f.gotEnv)
	}
}

// Security: the image is resolved ONLY from the allowlist; an "image" field in the body is ignored.
func TestRunNeverTrustsBodyImage(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), testToken,
		`{"app":"rara-distill","image":"attacker/evil:latest"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rr.Code)
	}
	if f.gotImage != testImage {
		t.Fatalf("body image must be ignored: got %q, want %q", f.gotImage, testImage)
	}
}

// Forward-compat with the control plane's Runner contract (rara-core F0): the F3 dispatcher marshals
// a full RunRequest{App, Provider, Capability, Env, ItemStepID}. The agent's wire subset is App+Env;
// the reserved fields must be accepted and ignored, not rejected.
func TestRunAcceptsFullControlPlaneRequest(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), testToken,
		`{"app":"rara-distill","provider":"distill-mac","capability":"destilar","env":{"DISTILL_RECIPE":"opus"},"itemStepID":42}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("full control-plane request: got %d, want 202", rr.Code)
	}
	if f.gotImage != testImage || f.gotEnv["DISTILL_RECIPE"] != "opus" {
		t.Fatalf("image/env wrong: image=%q env=%v", f.gotImage, f.gotEnv)
	}
}

func TestRunRejectsNonPost(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/run", nil)
	newTestServer(t, testToken, &fakeRunner{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /run: got %d, want 405", rr.Code)
	}
}

func TestRunReportsRunnerFailure(t *testing.T) {
	f := &fakeRunner{err: errFakeDocker}
	rr := post(t, newTestServer(t, testToken, f), testToken, `{"app":"rara-distill"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("runner error: got %d, want 500", rr.Code)
	}
}

func TestRunRejectsMissingApp(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), testToken, `{"env":{"X":"y"}}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing app: got %d, want 400", rr.Code)
	}
	if f.calls != 0 {
		t.Fatalf("runner must not run without an app")
	}
}

func TestValidateListenAddrRejectsWildcard(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:8473", ":8473", "[::]:8473", ""} {
		if err := validateListenAddr(bad); err == nil {
			t.Errorf("validateListenAddr(%q): want error (wildcard/empty bind), got nil", bad)
		}
	}
	for _, ok := range []string{"100.64.0.1:8473", "127.0.0.1:8473", "[fd7a::1]:8473"} {
		if err := validateListenAddr(ok); err != nil {
			t.Errorf("validateListenAddr(%q): want nil, got %v", ok, err)
		}
	}
}

func TestParseAllowlist(t *testing.T) {
	got, err := parseAllowlist("rara-distill=img-a@sha256:aa, rara-sift=img-b@sha256:bb")
	if err != nil {
		t.Fatalf("parseAllowlist: %v", err)
	}
	if got["rara-distill"] != "img-a@sha256:aa" || got["rara-sift"] != "img-b@sha256:bb" {
		t.Fatalf("parsed wrong: %v", got)
	}
	for _, bad := range []string{
		"",                                // empty allowlist
		"no-equals-sign",                  // not app=image
		"app=",                            // empty image
		"app=img:latest",                  // not pinned by digest
		"a=img@sha256:1,a=other@sha256:2", // duplicate app
	} {
		if _, err := parseAllowlist(bad); err == nil {
			t.Errorf("parseAllowlist(%q): want error, got nil", bad)
		}
	}
}

func TestRunRejectsMalformedJSON(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServer(t, testToken, f), testToken, `{malformed`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed json: got %d, want 400", rr.Code)
	}
	if f.calls != 0 {
		t.Fatalf("runner must not run on a decode error")
	}
}

// The env keys reach `docker run -e KEY=VAL`; a malformed name is rejected at the boundary (400)
// rather than handed to docker as an opaque failure.
func TestRunRejectsMalformedEnvKey(t *testing.T) {
	for _, body := range []string{
		`{"app":"rara-distill","env":{"BAD KEY":"x"}}`,
		`{"app":"rara-distill","env":{"1LEADING_DIGIT":"x"}}`,
		`{"app":"rara-distill","env":{"":"x"}}`,
	} {
		f := &fakeRunner{}
		rr := post(t, newTestServer(t, testToken, f), testToken, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("env key in %s: got %d, want 400", body, rr.Code)
		}
		if f.calls != 0 {
			t.Errorf("runner must not run with a malformed env key: %s", body)
		}
	}
}

// Auth/authorization errors must never echo the configured token back to the caller.
func TestErrorBodyDoesNotLeakToken(t *testing.T) {
	h := newTestServer(t, testToken, &fakeRunner{})
	for _, bearer := range []string{"", "wrong"} {
		rr := post(t, h, bearer, `{"app":"rara-distill"}`)
		if strings.Contains(rr.Body.String(), testToken) {
			t.Fatalf("response body leaked the token: %q", rr.Body.String())
		}
	}
}

func TestRunConcurrentRequests(t *testing.T) {
	f := &fakeRunner{}
	h := newTestServer(t, testToken, f)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rr := post(t, h, testToken, `{"app":"rara-distill"}`); rr.Code != http.StatusAccepted {
				t.Errorf("concurrent run: got %d, want 202", rr.Code)
			}
		}()
	}
	wg.Wait()
	if f.calls != n {
		t.Fatalf("runner calls: got %d, want %d", f.calls, n)
	}
}

func TestValidateDockerBin(t *testing.T) {
	for _, ok := range []string{"docker", "podman", "/usr/bin/docker", "/usr/local/bin/podman"} {
		if err := validateDockerBin(ok); err != nil {
			t.Errorf("validateDockerBin(%q): want nil, got %v", ok, err)
		}
	}
	for _, bad := range []string{"", "docker;rm -rf /", "rel/path/docker", "../docker", "kubectl"} {
		if err := validateDockerBin(bad); err == nil {
			t.Errorf("validateDockerBin(%q): want error, got nil", bad)
		}
	}
}
