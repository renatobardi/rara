package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// fakeDoer records the last request and returns a canned response/error, so the activator wire shape
// is asserted without a live endpoint.
type fakeDoer struct {
	gotReq  *http.Request
	gotBody string
	status  int
	body    string
	err     error
	calls   int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls++
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
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

// recordActivator is a sub-runner that records the providers it was asked to wake (for dispatch
// tests). A non-nil err is returned to assert error propagation.
type recordActivator struct {
	woken []string
	err   error
}

func (r *recordActivator) Run(_ context.Context, req RunRequest) error {
	r.woken = append(r.woken, req.Provider.Name)
	return r.err
}

func staticToken(tok string) tokenSource {
	return func(_ context.Context) (string, error) { return tok, nil }
}

// rr builds a RunRequest for a provider the way the reconciler does (App == provider name, the
// deployable unit today). Keeps the transport tests reading as "wake this provider".
func rr(p Provider) RunRequest {
	return RunRequest{App: p.Name, Provider: p, Capability: p.Capability}
}

// fakeOAuth2Source is a fake oauth2.TokenSource injected into cloudRunTokenSource for tests.
type fakeOAuth2Source struct{ token string }

func (f *fakeOAuth2Source) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: f.token}, nil
}

// --- cloudRunTokenSource selection --------------------------------------------

func TestCloudRunTokenSourcePrefersEnvVar(t *testing.T) {
	t.Setenv("CLOUD_RUN_OAUTH_TOKEN", "static-override-token")
	ts := cloudRunTokenSource(&fakeOAuth2Source{token: "should-not-be-used"})
	tok, err := ts(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "static-override-token" {
		t.Errorf("got %q, want static-override-token (env override must win)", tok)
	}
}

func TestCloudRunTokenSourceFallsToADCWhenEnvEmpty(t *testing.T) {
	t.Setenv("CLOUD_RUN_OAUTH_TOKEN", "")
	fake := &fakeOAuth2Source{token: "adc-refreshed-token"}
	ts := cloudRunTokenSource(fake)
	tok, err := ts(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "adc-refreshed-token" {
		t.Errorf("got %q, want adc-refreshed-token (ADC source must be used when env is unset)", tok)
	}
}

func TestCloudRunTokenSourceHonorsCancelledContext(t *testing.T) {
	t.Setenv("CLOUD_RUN_OAUTH_TOKEN", "")
	ts := cloudRunTokenSource(&fakeOAuth2Source{token: "should-not-be-reached"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ts(ctx); err == nil {
		t.Error("want error for a cancelled context, got nil")
	}
}

// --- cloudRunActivator --------------------------------------------------------

func TestCloudRunActivatorFiresJobsRun(t *testing.T) {
	doer := &fakeDoer{status: http.StatusOK, body: `{"name":"op"}`}
	a := &cloudRunActivator{project: "proj", region: "us-central1", http: doer, token: staticToken("tok123")}

	p := Provider{Name: "asr-direct-audio", Runtime: runtimeCloudRun, Activation: activationOnDemand}
	if err := a.Run(context.Background(), rr(p)); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	req := doer.gotReq
	if req == nil {
		t.Fatal("no request issued")
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	want := "https://run.googleapis.com/v2/projects/proj/locations/us-central1/jobs/asr-direct-audio:run"
	if req.URL.String() != want {
		t.Errorf("url = %s, want %s", req.URL.String(), want)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("authorization = %q, want Bearer tok123", got)
	}
}

func TestCloudRunActivatorJobPrefix(t *testing.T) {
	doer := &fakeDoer{}
	a := &cloudRunActivator{project: "p", region: "r", jobPrefix: "rara-", http: doer, token: staticToken("t")}
	if err := a.Run(context.Background(), rr(Provider{Name: "gate-barato", Runtime: runtimeCloudRun})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !strings.Contains(doer.gotReq.URL.String(), "/jobs/rara-gate-barato:run") {
		t.Errorf("url = %s, want job rara-gate-barato", doer.gotReq.URL.String())
	}
}

// TestCloudRunActivatorEscapesAppInPath: a job name with a path separator must be percent-escaped
// so it cannot inject extra URL segments and redirect the authenticated call to another endpoint.
func TestCloudRunActivatorEscapesAppInPath(t *testing.T) {
	doer := &fakeDoer{}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: staticToken("t")}
	if err := a.Run(context.Background(), RunRequest{App: "evil/../../jobs"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := doer.gotReq.URL.String(); strings.Contains(got, "evil/") {
		t.Errorf("app name not escaped in url: %s", got)
	}
}

// TestCloudRunActivatorRejectsEmptyApp: an empty app would POST to .../jobs/:run — reject it
// before issuing the authenticated request.
func TestCloudRunActivatorRejectsEmptyApp(t *testing.T) {
	doer := &fakeDoer{}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: staticToken("t")}
	if err := a.Run(context.Background(), RunRequest{App: ""}); err == nil {
		t.Fatal("expected error for empty app")
	}
	if doer.calls != 0 {
		t.Errorf("must not issue a request with an empty app, got %d calls", doer.calls)
	}
}

// TestCloudRunActivatorSendsEnvOverrides: when the RunRequest carries a non-empty Env map the
// body must include the Cloud Run v2 container override format so the job can read per-run config.
func TestCloudRunActivatorSendsEnvOverrides(t *testing.T) {
	doer := &fakeDoer{status: http.StatusOK, body: `{"name":"op"}`}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: staticToken("t")}
	req := RunRequest{
		App:      "gate-barato",
		Provider: Provider{Name: "gate-barato", Runtime: runtimeCloudRun},
		Env:      map[string]string{"ITEM_STEP_ID": "42"},
	}
	if err := a.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(doer.gotBody, `"overrides"`) {
		t.Errorf("body = %q, want overrides block", doer.gotBody)
	}
	if !strings.Contains(doer.gotBody, `"ITEM_STEP_ID"`) || !strings.Contains(doer.gotBody, `"42"`) {
		t.Errorf("body = %q, want ITEM_STEP_ID=42 in env overrides", doer.gotBody)
	}
}

// TestCloudRunActivatorEmptyEnvUsesDefaultBody: no Env → body is `{}` (deployed defaults only).
func TestCloudRunActivatorEmptyEnvUsesDefaultBody(t *testing.T) {
	doer := &fakeDoer{status: http.StatusOK}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: staticToken("t")}
	if err := a.Run(context.Background(), rr(Provider{Name: "gate-barato", Runtime: runtimeCloudRun})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if doer.gotBody != "{}" {
		t.Errorf("empty env body = %q, want {}", doer.gotBody)
	}
}

func TestCloudRunActivatorErrorOnNon2xx(t *testing.T) {
	doer := &fakeDoer{status: http.StatusForbidden, body: "PERMISSION_DENIED"}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: staticToken("t")}
	err := a.Run(context.Background(), rr(Provider{Name: "distill", Runtime: runtimeCloudRun}))
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Errorf("error = %v, want it to mention status + body", err)
	}
}

func TestCloudRunActivatorErrorOnTransport(t *testing.T) {
	doer := &fakeDoer{err: errors.New("dial tcp: timeout")}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: staticToken("t")}
	if err := a.Run(context.Background(), rr(Provider{Name: "distill", Runtime: runtimeCloudRun})); err == nil {
		t.Fatal("expected error on transport failure")
	}
}

func TestCloudRunActivatorErrorOnTokenFailure(t *testing.T) {
	doer := &fakeDoer{}
	a := &cloudRunActivator{project: "p", region: "r", http: doer, token: func(context.Context) (string, error) {
		return "", errors.New("no token")
	}}
	if err := a.Run(context.Background(), rr(Provider{Name: "distill", Runtime: runtimeCloudRun})); err == nil {
		t.Fatal("expected error when token source fails")
	}
	if doer.calls != 0 {
		t.Errorf("must not issue a request without a token, got %d calls", doer.calls)
	}
}

// --- pokeActivator ------------------------------------------------------------

func TestPokeActivatorPostsPoke(t *testing.T) {
	doer := &fakeDoer{status: http.StatusAccepted}
	a := &pokeActivator{token: "poketok", http: doer}

	p := Provider{Name: "distill-local", Runtime: runtimeVPC, Activation: activationResident, PokeURL: "http://mac.tailnet:7700"}
	if err := a.Run(context.Background(), rr(p)); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	req := doer.gotReq
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.URL.String() != "http://mac.tailnet:7700/poke" {
		t.Errorf("url = %s, want .../poke", req.URL.String())
	}
	if got := req.Header.Get("Authorization"); got != "Bearer poketok" {
		t.Errorf("authorization = %q, want Bearer poketok", got)
	}
}

func TestPokeActivatorTrimsTrailingSlash(t *testing.T) {
	doer := &fakeDoer{}
	a := &pokeActivator{token: "t", http: doer}
	if err := a.Run(context.Background(), rr(Provider{Name: "x", PokeURL: "http://host:7700/"})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if doer.gotReq.URL.String() != "http://host:7700/poke" {
		t.Errorf("url = %s, want single /poke", doer.gotReq.URL.String())
	}
}

func TestPokeActivatorNoopWithoutURL(t *testing.T) {
	doer := &fakeDoer{}
	a := &pokeActivator{token: "t", http: doer}
	if err := a.Run(context.Background(), rr(Provider{Name: "asr-youtube", Activation: activationResident})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if doer.calls != 0 {
		t.Errorf("must not poke a provider with no poke_url, got %d calls", doer.calls)
	}
}

func TestPokeActivatorErrorOnNon2xx(t *testing.T) {
	doer := &fakeDoer{status: http.StatusUnauthorized}
	a := &pokeActivator{token: "t", http: doer}
	if err := a.Run(context.Background(), rr(Provider{Name: "x", PokeURL: "http://h:1"})); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestPokeActivatorErrorOnTransport(t *testing.T) {
	doer := &fakeDoer{err: errors.New("host down")}
	a := &pokeActivator{token: "t", http: doer}
	if err := a.Run(context.Background(), rr(Provider{Name: "x", PokeURL: "http://h:1"})); err == nil {
		t.Fatal("expected error when the Mac is asleep / unreachable")
	}
}

// TestPokeActivatorRejectsNonHTTPURL: poke_url is operator config but carries the shared bearer, so
// a scheme-confused or hostless endpoint must be rejected BEFORE the token is ever sent (no request).
func TestPokeActivatorRejectsNonHTTPURL(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "gopher://internal:70", "ftp://host", "://nohost", "http://"} {
		doer := &fakeDoer{}
		a := &pokeActivator{token: "secret", http: doer}
		if err := a.Run(context.Background(), rr(Provider{Name: "x", PokeURL: bad})); err == nil {
			t.Errorf("poke_url %q: want error (untrusted/malformed endpoint), got nil", bad)
		}
		if doer.calls != 0 {
			t.Errorf("poke_url %q: must not send the bearer token, got %d calls", bad, doer.calls)
		}
	}
}

// --- dispatchActivator --------------------------------------------------------

func TestDispatchRoutesCloudRun(t *testing.T) {
	cr, poke := &recordActivator{}, &recordActivator{}
	d := dispatchActivator{cloudRun: cr, poke: poke}
	if err := d.Run(context.Background(), rr(Provider{Name: "distill", Runtime: runtimeCloudRun, Activation: activationOnDemand})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(cr.woken) != 1 || cr.woken[0] != "distill" {
		t.Errorf("cloud run woken = %v, want [distill]", cr.woken)
	}
	if len(poke.woken) != 0 {
		t.Errorf("poke must not fire for a cloudrun provider, got %v", poke.woken)
	}
}

func TestDispatchRoutesResidentToPoke(t *testing.T) {
	cr, poke := &recordActivator{}, &recordActivator{}
	d := dispatchActivator{cloudRun: cr, poke: poke}
	if err := d.Run(context.Background(), rr(Provider{Name: "asr-youtube", Runtime: runtimeLocal, Activation: activationResident})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(poke.woken) != 1 || poke.woken[0] != "asr-youtube" {
		t.Errorf("poke woken = %v, want [asr-youtube]", poke.woken)
	}
	if len(cr.woken) != 0 {
		t.Errorf("cloud run must not fire for a resident, got %v", cr.woken)
	}
}

func TestDispatchUnconfiguredBranchIsNoop(t *testing.T) {
	// A cloudrun provider with no cloud run activator configured: best-effort no-op, not an error.
	d := dispatchActivator{poke: &recordActivator{}}
	if err := d.Run(context.Background(), rr(Provider{Name: "distill", Runtime: runtimeCloudRun})); err != nil {
		t.Errorf("unconfigured cloud run must be a no-op, got %v", err)
	}
}

func TestDispatchDefaultBranchIsNoop(t *testing.T) {
	// An on_demand VPC provider fits no activation branch (VPC providers are normally resident,
	// woken by nothing) — so the dispatcher's default arm is a best-effort no-op.
	cr, poke := &recordActivator{}, &recordActivator{}
	d := dispatchActivator{cloudRun: cr, poke: poke}
	if err := d.Run(context.Background(), rr(Provider{Name: "vpc-ondemand-example", Runtime: runtimeVPC, Activation: activationOnDemand})); err != nil {
		t.Errorf("default branch must be a no-op, got %v", err)
	}
	if len(cr.woken)+len(poke.woken) != 0 {
		t.Errorf("no sub-activator should fire, got cloudrun=%v poke=%v", cr.woken, poke.woken)
	}
}

func TestDispatchPropagatesSubError(t *testing.T) {
	d := dispatchActivator{cloudRun: &recordActivator{err: errors.New("boom")}}
	if err := d.Run(context.Background(), rr(Provider{Name: "distill", Runtime: runtimeCloudRun})); err == nil {
		t.Fatal("dispatch must surface a sub-activator error (the reconciler logs it best-effort)")
	}
}
