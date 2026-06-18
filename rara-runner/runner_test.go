package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeHTTPDoer records the last request and returns a canned response for transport tests.
type fakeHTTPDoer struct {
	gotReq  *http.Request
	gotBody string
	status  int
	err     error
}

func (f *fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotReq = req
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.gotBody = string(b)
	}
	if f.err != nil {
		return nil, f.err
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

// fakeTransport records calls to assert dispatch routing without a real network.
type fakeTransport struct {
	called []RunRequest
	err    error
}

func (f *fakeTransport) Run(_ context.Context, req RunRequest) error {
	f.called = append(f.called, req)
	return f.err
}

// --- dispatchRunner -----------------------------------------------------------

func TestDispatchRunnerRoutesCloudRun(t *testing.T) {
	cr, host := &fakeTransport{}, &fakeTransport{}
	d := dispatchRunner{cloudRun: cr, host: host}

	if err := d.Run(context.Background(), RunRequest{App: "gate-barato", Runtime: runtimeCloudRun}); err != nil {
		t.Fatal(err)
	}
	if len(cr.called) != 1 {
		t.Errorf("cloud run called %d times, want 1", len(cr.called))
	}
	if len(host.called) != 0 {
		t.Errorf("host transport called %d times, want 0", len(host.called))
	}
}

func TestDispatchRunnerRoutesHostRunner(t *testing.T) {
	cr, host := &fakeTransport{}, &fakeTransport{}
	d := dispatchRunner{cloudRun: cr, host: host}

	req := RunRequest{App: "asr-youtube", Runtime: runtimeLocal, RunnerURL: "http://mac.tailnet:8473"}
	if err := d.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(host.called) != 1 {
		t.Errorf("host called %d times, want 1", len(host.called))
	}
	if len(cr.called) != 0 {
		t.Errorf("cloud run called %d times, want 0", len(cr.called))
	}
}

func TestDispatchRunnerNoOpWhenUnroutable(t *testing.T) {
	// runtime=local with no runner_url and no cloud run transport — no-op, not an error.
	d := dispatchRunner{}
	if err := d.Run(context.Background(), RunRequest{App: "x", Runtime: runtimeLocal}); err != nil {
		t.Errorf("unroutable dispatch must not error, got %v", err)
	}
}

func TestDispatchRunnerPropagatesSubError(t *testing.T) {
	cr := &fakeTransport{err: errors.New("cloud run boom")}
	d := dispatchRunner{cloudRun: cr}
	if err := d.Run(context.Background(), RunRequest{App: "x", Runtime: runtimeCloudRun}); err == nil {
		t.Error("want error from cloud run transport, got nil")
	}
}

// --- cloudRunTransport -------------------------------------------------------

func TestCloudRunTransportFiresJobsRun(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusOK}
	tr := &cloudRunTransport{project: "proj", region: "us-central1", http: doer, token: "tok123"}

	req := RunRequest{App: "gate-barato", Runtime: runtimeCloudRun}
	if err := tr.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantURL := "https://run.googleapis.com/v2/projects/proj/locations/us-central1/jobs/gate-barato:run"
	if got := doer.gotReq.URL.String(); got != wantURL {
		t.Errorf("url = %s, want %s", got, wantURL)
	}
	if got := doer.gotReq.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("authorization = %q, want Bearer tok123", got)
	}
}

func TestCloudRunTransportJobPrefix(t *testing.T) {
	doer := &fakeHTTPDoer{}
	tr := &cloudRunTransport{project: "p", region: "r", jobPrefix: "rara-", http: doer, token: "t"}
	if err := tr.Run(context.Background(), RunRequest{App: "gate-barato", Runtime: runtimeCloudRun}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doer.gotReq.URL.String(), "/jobs/rara-gate-barato:run") {
		t.Errorf("url = %s, want job rara-gate-barato", doer.gotReq.URL.String())
	}
}

func TestCloudRunTransportSendsEnvOverrides(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusOK}
	tr := &cloudRunTransport{project: "p", region: "r", http: doer, token: "t"}
	req := RunRequest{App: "gate-barato", Runtime: runtimeCloudRun, Env: map[string]string{"ITEM_STEP_ID": "42"}}
	if err := tr.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doer.gotBody, `"overrides"`) {
		t.Errorf("body = %q, want overrides block", doer.gotBody)
	}
	if !strings.Contains(doer.gotBody, `"ITEM_STEP_ID"`) || !strings.Contains(doer.gotBody, `"42"`) {
		t.Errorf("body = %q, want ITEM_STEP_ID=42 in env overrides", doer.gotBody)
	}
}

func TestCloudRunTransportEmptyEnvUsesDefaultBody(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusOK}
	tr := &cloudRunTransport{project: "p", region: "r", http: doer, token: "t"}
	if err := tr.Run(context.Background(), RunRequest{App: "gate-barato", Runtime: runtimeCloudRun}); err != nil {
		t.Fatal(err)
	}
	if doer.gotBody != "{}" {
		t.Errorf("empty env body = %q, want {}", doer.gotBody)
	}
}

func TestCloudRunTransportErrorOnNon2xx(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusForbidden}
	tr := &cloudRunTransport{project: "p", region: "r", http: doer, token: "t"}
	if err := tr.Run(context.Background(), RunRequest{App: "x", Runtime: runtimeCloudRun}); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestCloudRunTransportErrorOnTransport(t *testing.T) {
	doer := &fakeHTTPDoer{err: errors.New("dial timeout")}
	tr := &cloudRunTransport{project: "p", region: "r", http: doer, token: "t"}
	if err := tr.Run(context.Background(), RunRequest{App: "x", Runtime: runtimeCloudRun}); err == nil {
		t.Fatal("expected error on transport failure")
	}
}

// --- hostRunnerTransport -----------------------------------------------------

func TestHostRunnerTransportPostsRun(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusAccepted}
	tr := &hostRunnerTransport{token: "secret", http: doer}

	req := RunRequest{App: "asr-youtube", Runtime: runtimeLocal, RunnerURL: "http://mac.tailnet:8473"}
	if err := tr.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := doer.gotReq.URL.String(); got != "http://mac.tailnet:8473/run" {
		t.Errorf("url = %s, want .../run", got)
	}
	if got := doer.gotReq.Header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("authorization = %q, want Bearer secret", got)
	}
	if !strings.Contains(doer.gotBody, `"asr-youtube"`) {
		t.Errorf("body = %q, want app name in body", doer.gotBody)
	}
}

func TestHostRunnerTransportTrimsTrailingSlash(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusAccepted}
	tr := &hostRunnerTransport{token: "t", http: doer}
	req := RunRequest{App: "x", RunnerURL: "http://host:8473/"}
	if err := tr.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := doer.gotReq.URL.String(); got != "http://host:8473/run" {
		t.Errorf("url = %s, want single /run (no double slash)", got)
	}
}

func TestHostRunnerTransportPassesEnv(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusAccepted}
	tr := &hostRunnerTransport{token: "t", http: doer}
	req := RunRequest{App: "x", RunnerURL: "http://h:1", Env: map[string]string{"KEY": "val"}}
	if err := tr.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doer.gotBody, `"KEY"`) {
		t.Errorf("body = %q, want KEY in env", doer.gotBody)
	}
}

func TestHostRunnerTransportErrorOnNon2xx(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusUnauthorized}
	tr := &hostRunnerTransport{token: "t", http: doer}
	if err := tr.Run(context.Background(), RunRequest{App: "x", RunnerURL: "http://h:1"}); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestHostRunnerTransportErrorOnTransport(t *testing.T) {
	doer := &fakeHTTPDoer{err: errors.New("host down")}
	tr := &hostRunnerTransport{token: "t", http: doer}
	if err := tr.Run(context.Background(), RunRequest{App: "x", RunnerURL: "http://h:1"}); err == nil {
		t.Fatal("expected error on transport failure")
	}
}

// TestHostRunnerTransportRejectsInvalidRunnerURL: runner_url is operator config but carries the
// shared bearer, so a scheme-confused or hostless endpoint must be rejected BEFORE the token is sent.
func TestHostRunnerTransportRejectsInvalidRunnerURL(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "gopher://internal:70", "://nohost", "http://"} {
		doer := &fakeHTTPDoer{}
		tr := &hostRunnerTransport{token: "secret", http: doer}
		if err := tr.Run(context.Background(), RunRequest{App: "x", RunnerURL: bad}); err == nil {
			t.Errorf("runner_url %q: want error (untrusted endpoint), got nil", bad)
		}
		if doer.gotReq != nil {
			t.Errorf("runner_url %q: must not send bearer token, got request", bad)
		}
	}
}
