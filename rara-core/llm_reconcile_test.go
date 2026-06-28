package main

import (
	"bytes"
	"context"
	"errors"
	"log"
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

// seedSyncModel creates an enabled provider (with a real encrypted key) + enabled model.
func seedSyncModel(t *testing.T, core *Core, alias, upstream, apiKey string) {
	t.Helper()
	ctx := context.Background()
	if err := core.UpsertLLMProvider(ctx, LLMProviderInput{
		Name: "groq-test", Kind: "groq", APIKey: apiKey,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	providers, err := core.ListLLMProviders(ctx)
	if err != nil || len(providers) == 0 {
		t.Fatalf("list providers: %v", err)
	}
	if err := core.UpsertLLMModel(ctx, LLMModelInput{
		ProviderID: providers[0].ID, Alias: alias, Upstream: upstream,
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
}

func TestLLMReconcileCreatesNewModel(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedSyncModel(t, core, "groq-llama", "groq/llama-3.3", "sk-test-123") // gitleaks:allow
	gw := &fakeGateway{}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(gw.added) != 1 || gw.added[0].ModelName != "groq-llama" {
		t.Fatalf("want 1 create of groq-llama, got %+v", gw.added)
	}
	if gw.added[0].Upstream != "groq/llama-3.3" {
		t.Errorf("upstream = %q", gw.added[0].Upstream)
	}
	if gw.added[0].APIKey != "sk-test-123" { // gitleaks:allow
		t.Errorf("decrypted key not handed to gateway: %q", gw.added[0].APIKey)
	}
	if len(gw.deleted) != 0 {
		t.Errorf("unexpected deletes: %v", gw.deleted)
	}
}

func TestLLMReconcileDeletesOrphanButIgnoresConfigModels(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedSyncModel(t, core, "groq-llama", "groq/llama-3.3", "sk-test-123") // gitleaks:allow
	gw := &fakeGateway{models: []litellm.Model{
		{ModelName: "config-model", Upstream: "groq/old", ID: "cfg1", DBModel: false}, // config.yaml — read-only
		{ModelName: "stale-db", Upstream: "x/y", ID: "db9", DBModel: true},            // orphan db_model
	}}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// groq-llama is created (not in gateway); stale-db orphan is deleted; config-model untouched.
	if len(gw.added) != 1 || gw.added[0].ModelName != "groq-llama" {
		t.Errorf("want create groq-llama, got %+v", gw.added)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != "db9" {
		t.Errorf("want delete of orphan db9 only, got %v", gw.deleted)
	}
}

func TestLLMReconcileIdempotentSecondPassNoWrites(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedSyncModel(t, core, "groq-llama", "groq/llama-3.3", "sk-test-123") // gitleaks:allow
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

func TestLLMReconcileUpdatesOnContentChange(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedSyncModel(t, core, "groq-llama", "groq/llama-3.3", "sk-test-123") // gitleaks:allow
	// Gateway has the alias as a db_model but with a stale fingerprint (e.g. old upstream).
	gw := &fakeGateway{models: []litellm.Model{
		{ModelName: "groq-llama", Upstream: "groq/OLD", Fingerprint: "stale", ID: "db1", DBModel: true},
	}}

	r := NewLLMReconciler(db, gw, box)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != "db1" {
		t.Errorf("want delete of stale db1, got %v", gw.deleted)
	}
	if len(gw.added) != 1 || gw.added[0].Upstream != "groq/llama-3.3" {
		t.Errorf("want recreate with new upstream, got %+v", gw.added)
	}
}

func TestLLMReconcileNeverLogsDecryptedKey(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	const secret = "sk-super-secret-9f3" // gitleaks:allow
	seedSyncModel(t, core, "groq-llama", "groq/llama-3.3", secret)
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
		t.Errorf("key not delivered to gateway: %+v", gw.added)
	}
}

func TestLLMReconcileGatewayUnavailableNoWrites(t *testing.T) {
	core, db, box := newTestCoreWithBox(t)
	seedSyncModel(t, core, "groq-llama", "groq/llama-3.3", "sk-test-123") // gitleaks:allow
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

func TestDiffLLMModelsPure(t *testing.T) {
	desired := []litellm.Model{
		{ModelName: "keep", Fingerprint: "fp-keep"},
		{ModelName: "new", Fingerprint: "fp-new"},
		{ModelName: "changed", Fingerprint: "fp-v2"},
	}
	actual := []litellm.Model{
		{ModelName: "keep", Fingerprint: "fp-keep", ID: "a1", DBModel: true},
		{ModelName: "changed", Fingerprint: "fp-v1", ID: "a2", DBModel: true},
		{ModelName: "orphan", Fingerprint: "x", ID: "a3", DBModel: true},
		{ModelName: "fromconfig", Fingerprint: "x", ID: "a4", DBModel: false},
	}
	create, del := diffLLMModels(desired, actual)

	gotCreate := map[string]bool{}
	for _, c := range create {
		gotCreate[c.ModelName] = true
	}
	if !gotCreate["new"] || !gotCreate["changed"] || gotCreate["keep"] {
		t.Errorf("create set wrong: %v", gotCreate)
	}
	gotDel := map[string]bool{}
	for _, id := range del {
		gotDel[id] = true
	}
	if !gotDel["a2"] || !gotDel["a3"] || gotDel["a1"] || gotDel["a4"] {
		t.Errorf("delete set wrong: %v (a4=config must be ignored)", gotDel)
	}
}
