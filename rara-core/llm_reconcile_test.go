package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"

	"rara-core/internal/litellm"
)

// fakeGateway is an in-memory litellmGateway: it records writes and serves a scripted
// ListModels result, so reconcile tests do zero real I/O.
type fakeGateway struct {
	models    []litellm.Model // what ListModels returns (the "actual" gateway state)
	added     []litellm.Model
	deleted   []string
	healthErr error
	listErr   error
}

func (f *fakeGateway) Health(context.Context) error { return f.healthErr }
func (f *fakeGateway) ListModels(context.Context) ([]litellm.Model, error) {
	return f.models, f.listErr
}
func (f *fakeGateway) AddModel(_ context.Context, m litellm.Model) error {
	f.added = append(f.added, m)
	return nil
}
func (f *fakeGateway) DeleteModel(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// seedLLMProvider creates an enabled llm_provider of the given kind with a real encrypted key.
func seedLLMProvider(t *testing.T, core *Core, name, kind, apiKey string) {
	t.Helper()
	if err := core.UpsertLLMProvider(context.Background(), LLMProviderInput{
		Name: name, Kind: kind, APIKey: apiKey,
	}); err != nil {
		t.Fatalf("seed llm provider %q: %v", name, err)
	}
}

// seedWorkerBinding creates an enabled worker (providers row) whose env pins LITELLM_MODEL —
// the source of the reconciler's desired set.
func seedWorkerBinding(t *testing.T, db *MockDatabase, name, litellmModel string) {
	t.Helper()
	mustCapability(t, db, capDestilar)
	env, err := json.Marshal(map[string]string{"DISTILL_PROVIDER": name, "LITELLM_MODEL": litellmModel})
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	mustProvider(t, db, Provider{
		Name: name, Capability: capDestilar, Worker: "distill",
		Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Enabled: true, Env: env,
	})
}

func TestLLMReconcileCreatesConcreteModelForBoundUpstream(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedLLMProvider(t, core, "groq-main", "groq", "sk-test-123") // gitleaks:allow
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	gw := &fakeGateway{}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(gw.added) != 1 {
		// Don't print gw.added — its Models carry the decrypted APIKey.
		t.Fatalf("want 1 create, got %d", len(gw.added))
	}
	got := gw.added[0]
	// model_name == litellm_params.model == the full upstream string (no alias indirection).
	if got.ModelName != "groq/llama-3.3-70b-versatile" || got.Upstream != "groq/llama-3.3-70b-versatile" {
		t.Errorf("model_name/upstream = %q / %q, want both groq/llama-3.3-70b-versatile", got.ModelName, got.Upstream)
	}
	if got.APIKey != "sk-test-123" { // gitleaks:allow
		// Never echo got.APIKey — a mismatch would leak the decrypted key into CI logs.
		t.Errorf("decrypted key not handed to gateway (value redacted)")
	}
	if len(gw.deleted) != 0 {
		t.Errorf("unexpected deletes: %v", gw.deleted)
	}
}

// TestLLMReconcileDeletesLegacyAliasFromGateway: after CORR-INFER #5 the reconciler cleans up
// legacy bare aliases (db_model, no '/') from the gateway. A worker still pinning a bare alias
// does not enter the desired set (ListBoundUpstreams filters it out), but that alias IS deleted
// from the gateway because bound only contains kind/model strings.
func TestLLMReconcileDeletesLegacyAliasFromGateway(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedLLMProvider(t, core, "groq-main", "groq", "sk-test-123") // gitleaks:allow
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	// A legacy worker still pinning a bare alias (no '/') must NOT enter the desired set.
	seedWorkerBinding(t, db, "sift-cloud", "groq-llama")
	gw := &fakeGateway{models: []litellm.Model{
		// Legacy alias managed in the gateway DB (db_model, no '/') — no longer retained after CORR-#5.
		{ModelName: "groq-llama", Upstream: "groq/llama-3.3-70b-versatile", ID: "db-legacy", DBModel: true},
		// config.yaml model — read-only, never touched.
		{ModelName: "config-model", Upstream: "groq/old", ID: "cfg1", DBModel: false},
	}}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(gw.added) != 1 || gw.added[0].ModelName != "groq/llama-3.3-70b-versatile" {
		t.Errorf("want only the concrete model created, got %d adds", len(gw.added))
	}
	// Legacy alias (no binding in bound set) must now be deleted; config.yaml model untouched.
	if len(gw.deleted) != 1 || gw.deleted[0] != "db-legacy" {
		t.Errorf("want delete of legacy alias db-legacy, got %v", gw.deleted)
	}
}

func TestLLMReconcileSkipsBoundUpstreamWithoutProvider(t *testing.T) {
	_, db, box := newTestCoreWithBox(t)
	// Worker binds groq/... but no enabled groq provider exists → skip, log, don't fail.
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	gw := &fakeGateway{}

	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(old)

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile must not fail when a provider is missing: %v", err)
	}
	if len(gw.added) != 0 || len(gw.deleted) != 0 {
		t.Errorf("no writes when the provider is absent, got +%d -%d", len(gw.added), len(gw.deleted))
	}
	if !strings.Contains(logBuf.String(), "groq/llama-3.3-70b-versatile") {
		t.Errorf("expected a skip log mentioning the upstream, got %q", logBuf.String())
	}
}

func TestLLMReconcileSkipsProviderWithoutKey(t *testing.T) {
	_, db, box := newTestCoreWithBox(t)
	// Enabled provider for kind groq but with no key material (degenerate row).
	db.llmProviders = append(db.llmProviders, mockLLMProvider{
		ID: 1, Name: "groq-nokey", Kind: "groq", Enabled: true,
	})
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	gw := &fakeGateway{}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile must not fail when a provider has no key: %v", err)
	}
	if len(gw.added) != 0 {
		t.Errorf("a keyless provider cannot be registered, got %d adds", len(gw.added))
	}
}

func TestLLMReconcileDeletesManagedConcreteWithoutBinding(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedLLMProvider(t, core, "groq-main", "groq", "sk-test-123") // gitleaks:allow
	// No worker binds any upstream. A managed concrete model lingers in the gateway.
	gw := &fakeGateway{models: []litellm.Model{
		{ModelName: "groq/retired-model", Upstream: "groq/retired-model", ID: "db-old", DBModel: true},
	}}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != "db-old" {
		t.Errorf("want delete of orphan managed concrete db-old, got %v", gw.deleted)
	}
	if len(gw.added) != 0 {
		t.Errorf("unexpected creates: %d adds", len(gw.added))
	}
}

func TestLLMReconcileRetainsModelWhenProviderTemporarilyMissing(t *testing.T) {
	_, db, box := newTestCoreWithBox(t)
	// A worker still binds the upstream, but its provider is currently absent (disabled/removed).
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	// The gateway already serves the managed concrete model from an earlier healthy pass.
	gw := &fakeGateway{models: []litellm.Model{
		{ModelName: "groq/llama-3.3-70b-versatile", Upstream: "groq/llama-3.3-70b-versatile", ID: "db1", DBModel: true},
	}}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The binding is still active — a transient provider misconfig must NOT delete a live model.
	if len(gw.deleted) != 0 {
		t.Errorf("must retain a still-bound model when its provider is temporarily missing, got deletes %v", gw.deleted)
	}
	if len(gw.added) != 0 {
		t.Errorf("cannot recreate without a key; expected no creates, got %d adds", len(gw.added))
	}
}

func TestLLMReconcileIdempotentSecondPassNoWrites(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedLLMProvider(t, core, "groq-main", "groq", "sk-test-123") // gitleaks:allow
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	gw := &fakeGateway{}
	r := NewLLMReconciler(db, gw, box)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// Feed the created model back as the gateway's actual state, as a db_model with its fingerprint.
	created := gw.added[0]
	gw.models = []litellm.Model{{
		ModelName: created.ModelName, Upstream: created.Upstream,
		Fingerprint: created.Fingerprint, ID: "db1", DBModel: true,
	}}
	gw.added = nil
	gw.deleted = nil

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(gw.added) != 0 || len(gw.deleted) != 0 {
		t.Errorf("second pass must be a no-op, got +%d -%d", len(gw.added), len(gw.deleted))
	}
}

func TestLLMReconcileNeverLogsDecryptedKey(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	const secret = "sk-super-secret-9f3" // gitleaks:allow
	seedLLMProvider(t, core, "groq-main", "groq", secret)
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	gw := &fakeGateway{}

	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(old)

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if strings.Contains(logBuf.String(), secret) {
		t.Errorf("decrypted key leaked into logs: %q", logBuf.String())
	}
	// Sanity: the key DID reach the gateway client (decryption happened).
	if len(gw.added) != 1 || gw.added[0].APIKey != secret {
		// Don't print gw.added — it carries the plaintext key.
		t.Errorf("decrypted key not delivered to gateway (got %d adds; value redacted)", len(gw.added))
	}
}

func TestLLMReconcileGatewayUnavailableNoWrites(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedLLMProvider(t, core, "groq-main", "groq", "sk-test-123") // gitleaks:allow
	seedWorkerBinding(t, db, "distill-cloud", "groq/llama-3.3-70b-versatile")
	gw := &fakeGateway{healthErr: errors.New("connection refused")}

	r := NewLLMReconciler(db, gw, box)
	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("want error when gateway is unavailable")
	}
	if len(gw.added) != 0 || len(gw.deleted) != 0 {
		t.Errorf("no writes allowed when gateway is down, got +%d -%d", len(gw.added), len(gw.deleted))
	}
}

// TestDiffLLMModelsLegacyAliasWithoutBindingDeleted: a legacy db_model alias (no '/') with no
// active binding is deleted from the gateway. bound only contains kind/model strings (from
// ListBoundUpstreams), so legacy aliases are never in bound and are always eligible for cleanup.
func TestDiffLLMModelsLegacyAliasWithoutBindingDeleted(t *testing.T) {
	desired := []litellm.Model{
		{ModelName: "groq/keep", Fingerprint: "fp-keep"},
		{ModelName: "groq/new", Fingerprint: "fp-new"},
		{ModelName: "groq/changed", Fingerprint: "fp-v2"},
	}
	actual := []litellm.Model{
		{ModelName: "groq/keep", Fingerprint: "fp-keep", ID: "a1", DBModel: true},
		{ModelName: "groq/changed", Fingerprint: "fp-v1", ID: "a2", DBModel: true},
		{ModelName: "groq/orphan", Fingerprint: "x", ID: "a3", DBModel: true},   // not bound → delete
		{ModelName: "groq-llama", Fingerprint: "x", ID: "a4", DBModel: true},    // legacy alias, no binding → delete
		{ModelName: "config/model", Fingerprint: "x", ID: "a5", DBModel: false}, // config.yaml → never touched
		{ModelName: "groq/retained", Fingerprint: "x", ID: "a6", DBModel: true}, // bound, unresolvable → retain
	}
	// Every desired name is bound; "groq/retained" is bound but absent from desired (provider
	// transiently unresolvable). "groq/orphan" and "groq-llama" are NOT bound.
	bound := map[string]bool{"groq/keep": true, "groq/new": true, "groq/changed": true, "groq/retained": true}

	create, del := diffLLMModels(desired, actual, bound)

	gotCreate := map[string]bool{}
	for _, c := range create {
		gotCreate[c.ModelName] = true
	}
	wantCreate := map[string]bool{"groq/new": true, "groq/changed": true}
	if !reflect.DeepEqual(gotCreate, wantCreate) {
		t.Errorf("create set = %v, want exactly %v", gotCreate, wantCreate)
	}
	gotDel := map[string]bool{}
	for _, id := range del {
		gotDel[id] = true
	}
	// a2 (changed) + a3 (orphan) + a4 (legacy alias, no binding). NOT a1 (keep), a5 (config), a6 (retained bound).
	wantDel := map[string]bool{"a2": true, "a3": true, "a4": true}
	if !reflect.DeepEqual(gotDel, wantDel) {
		t.Errorf("delete set = %v, want exactly %v (a6 must be retained as still-bound)", gotDel, wantDel)
	}
}

// TestDiffLLMModelsDeletesLegacyAlias: a legacy db_model alias (no '/') with no active binding
// is included in deleteIDs; a config.yaml model is never touched regardless.
func TestDiffLLMModelsDeletesLegacyAlias(t *testing.T) {
	desired := []litellm.Model{
		{ModelName: "groq/llama-3.3-70b-versatile", Fingerprint: "fp1"},
	}
	actual := []litellm.Model{
		{ModelName: "groq/llama-3.3-70b-versatile", Fingerprint: "fp1", ID: "a1", DBModel: true},
		{ModelName: "groq-llama", Fingerprint: "x", ID: "a2", DBModel: true},  // legacy — must be deleted
		{ModelName: "config/m", Fingerprint: "x", ID: "a3", DBModel: false},   // config.yaml — never touched
	}
	bound := map[string]bool{"groq/llama-3.3-70b-versatile": true}

	_, del := diffLLMModels(desired, actual, bound)

	gotDel := map[string]bool{}
	for _, id := range del {
		gotDel[id] = true
	}
	if !gotDel["a2"] {
		t.Errorf("delete set = %v, want a2 (legacy alias without binding) included", gotDel)
	}
	if gotDel["a1"] || gotDel["a3"] {
		t.Errorf("delete set = %v, must not include a1 (keep) or a3 (config.yaml)", gotDel)
	}
}
