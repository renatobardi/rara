package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func (f *fakeRunner) Run(_ context.Context, image string, env map[string]string) error {
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
	testPath  = "us-docker.pkg.dev/p/r/rara-distill" // bare registry path; no tag or digest
	testImage = testPath + ":latest"                 // resolved at run time by the agent
)

func newTestServer(t *testing.T, token string, runner ContainerRunner) http.Handler {
	t.Helper()
	return newAgentServer(token, map[string]string{testApp: testPath}, nil, runner)
}

func newTestServerWithBase(t *testing.T, base map[string]string, runner ContainerRunner) http.Handler {
	t.Helper()
	return newAgentServer(testToken, map[string]string{testApp: testPath}, base, runner)
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

func TestRunValidAppDispatchesAllowlistedImage(t *testing.T) {
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
		t.Fatalf("image: got %q, want %q (path:latest)", f.gotImage, testImage)
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
	for _, bad := range []string{
		"0.0.0.0:8473",     // IPv4 wildcard
		":8473",            // bare port = all interfaces
		"[::]:8473",        // IPv6 wildcard
		"",                 // empty
		"192.168.1.1:8473", // LAN — not tailnet
		"10.0.0.1:8473",    // private RFC-1918 — not tailnet
		"8.8.8.8:8473",     // public IP
		"[fd7a::1]:8473",   // fd7a:: but outside Tailscale /48
		"notanip:8473",     // hostname (not an IP literal)
	} {
		if err := validateListenAddr(bad); err == nil {
			t.Errorf("validateListenAddr(%q): want error, got nil", bad)
		}
	}
	for _, ok := range []string{
		"100.64.0.1:8473",          // Tailscale CGNAT (100.64.0.0/10)
		"100.127.255.254:8473",     // top of Tailscale CGNAT range
		"127.0.0.1:8473",           // loopback (dev/test)
		"[::1]:8473",               // IPv6 loopback
		"[fd7a:115c:a1e0::1]:8473", // Tailscale IPv6 (fd7a:115c:a1e0::/48)
	} {
		if err := validateListenAddr(ok); err != nil {
			t.Errorf("validateListenAddr(%q): want nil, got %v", ok, err)
		}
	}
}

func TestParseAllowlist(t *testing.T) {
	const (
		pathA = "us-docker.pkg.dev/p/r/rara-distill"
		pathB = "us-docker.pkg.dev/p/r/rara-sift"
	)
	got, err := parseAllowlist("rara-distill=" + pathA + ", rara-sift=" + pathB)
	if err != nil {
		t.Fatalf("parseAllowlist: %v", err)
	}
	if got["rara-distill"] != pathA || got["rara-sift"] != pathB {
		t.Fatalf("parsed wrong: %v", got)
	}
	for _, bad := range []string{
		"",                           // empty allowlist
		"no-equals-sign",             // not app=path
		"app=",                       // empty path
		"app=img:latest",             // tagged — must be bare path
		"app=img@sha256:abc123",      // digest-pinned — must be bare path
		"a=" + pathA + ",a=" + pathB, // duplicate app
	} {
		if _, err := parseAllowlist(bad); err == nil {
			t.Errorf("parseAllowlist(%q): want error, got nil", bad)
		}
	}
}

func TestImagePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"us-docker.pkg.dev/p/r/img", "us-docker.pkg.dev/p/r/img"},
		{"us-docker.pkg.dev/p/r/img:latest", "us-docker.pkg.dev/p/r/img"},
		{"us-docker.pkg.dev/p/r/img@sha256:" + strings.Repeat("a", 64), "us-docker.pkg.dev/p/r/img"},
		{"localhost:5000/img:tag", "localhost:5000/img"},
		{"localhost:5000/img", "localhost:5000/img"},
	}
	for _, c := range cases {
		if got := imagePath(c.in); got != c.want {
			t.Errorf("imagePath(%q) = %q, want %q", c.in, got, c.want)
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

// --- loadEnvFile ---

func writeTempEnvFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "worker*.env")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestLoadEnvFileEmptyPathReturnsEmpty(t *testing.T) {
	got, err := loadEnvFile("")
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty path: want empty map, got %v", got)
	}
}

func TestLoadEnvFileParsesKeyValue(t *testing.T) {
	f := writeTempEnvFile(t, "DATABASE_URL=postgres://localhost/db\nLITELLM_BASE_URL=http://localhost:4000\n")
	got, err := loadEnvFile(f)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if got["DATABASE_URL"] != "postgres://localhost/db" {
		t.Fatalf("DATABASE_URL: got %q", got["DATABASE_URL"])
	}
	if got["LITELLM_BASE_URL"] != "http://localhost:4000" {
		t.Fatalf("LITELLM_BASE_URL: got %q", got["LITELLM_BASE_URL"])
	}
}

func TestLoadEnvFileSkipsCommentsAndBlanks(t *testing.T) {
	f := writeTempEnvFile(t, "# comment\n\nKEY=val\n  # another\n")
	got, err := loadEnvFile(f)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if len(got) != 1 || got["KEY"] != "val" {
		t.Fatalf("want {KEY:val}, got %v", got)
	}
}

func TestLoadEnvFileValueWithEqualsSign(t *testing.T) {
	f := writeTempEnvFile(t, "URL=postgres://host/db?key=val\n")
	got, err := loadEnvFile(f)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if got["URL"] != "postgres://host/db?key=val" {
		t.Fatalf("URL: got %q", got["URL"])
	}
}

func TestLoadEnvFileMissingFileReturnsEmpty(t *testing.T) {
	got, err := loadEnvFile(filepath.Join(t.TempDir(), "nonexistent.env"))
	if err != nil {
		t.Fatalf("missing file: want nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing file: want empty map, got %v", got)
	}
}

func TestLoadEnvFilePermissionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"allow_0600", 0o600, false},
		{"allow_0640", 0o640, false},
		{"reject_0644_world_readable", 0o644, true},
		{"reject_0602_world_writable", 0o602, true},
		{"reject_0620_group_writable", 0o620, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := writeTempEnvFile(t, "KEY=val\n")
			if err := os.Chmod(f, tc.mode); err != nil {
				t.Fatal(err)
			}
			_, err := loadEnvFile(f)
			if tc.wantErr && err == nil {
				t.Fatalf("mode %04o: want error, got nil", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mode %04o: want nil, got %v", tc.mode, err)
			}
		})
	}
}

func TestLoadEnvFileRejectsInvalidKey(t *testing.T) {
	f := writeTempEnvFile(t, "VALID=ok\nBAD-KEY=x\n")
	_, err := loadEnvFile(f)
	if err == nil {
		t.Fatal("want error for invalid env key, got nil")
	}
}

// --- mergeEnv ---

func TestMergeEnvCombinesBothMaps(t *testing.T) {
	got := mergeEnv(map[string]string{"A": "1", "B": "2"}, map[string]string{"C": "3"})
	if got["A"] != "1" || got["B"] != "2" || got["C"] != "3" {
		t.Fatalf("merge: got %v", got)
	}
}

func TestMergeEnvOverrideWins(t *testing.T) {
	got := mergeEnv(map[string]string{"FOO": "base"}, map[string]string{"FOO": "body"})
	if got["FOO"] != "body" {
		t.Fatalf("override: got %q, want %q", got["FOO"], "body")
	}
}

func TestMergeEnvNilBaseIsNoop(t *testing.T) {
	got := mergeEnv(nil, map[string]string{"X": "y"})
	if got["X"] != "y" || len(got) != 1 {
		t.Fatalf("nil base: got %v", got)
	}
}

// --- HTTP integration: base env injected into docker run ---

func TestRunInjectsBaseEnvIntoContainer(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServerWithBase(t, map[string]string{"DATABASE_URL": "postgres://secret"}, f),
		testToken, `{"app":"rara-distill","env":{"DISTILL_RECIPE":"opus"}}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rr.Code)
	}
	if f.gotEnv["DATABASE_URL"] != "postgres://secret" {
		t.Fatalf("base env not injected: DATABASE_URL=%q", f.gotEnv["DATABASE_URL"])
	}
	if f.gotEnv["DISTILL_RECIPE"] != "opus" {
		t.Fatalf("body env missing: DISTILL_RECIPE=%q", f.gotEnv["DISTILL_RECIPE"])
	}
}

func TestRunBodyEnvOverridesBaseEnv(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServerWithBase(t, map[string]string{"FOO": "from-base"}, f),
		testToken, `{"app":"rara-distill","env":{"FOO":"from-body"}}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rr.Code)
	}
	if f.gotEnv["FOO"] != "from-body" {
		t.Fatalf("body should override base: FOO=%q", f.gotEnv["FOO"])
	}
}

func TestRunNilBaseEnvDoesNotBreak(t *testing.T) {
	f := &fakeRunner{}
	rr := post(t, newTestServerWithBase(t, nil, f),
		testToken, `{"app":"rara-distill","env":{"KEY":"val"}}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("nil base: got %d, want 202", rr.Code)
	}
	if f.gotEnv["KEY"] != "val" {
		t.Fatalf("body env: KEY=%q", f.gotEnv["KEY"])
	}
}
