// llm_reconcile.go — materializes the rara LLM registry into the LiteLLM gateway as
// CONCRETE provider/model entries, level-triggered and idempotent.
//
// The desired set is the set of CONCRETE upstreams ("{kind}/{model}", e.g.
// "groq/llama-3.3-70b-versatile") referenced by enabled worker bindings
// (providers.env->>'LITELLM_MODEL' values that contain a '/'). Each is registered with
// model_name == litellm_params.model == the full upstream string and the matching provider's
// decrypted key — the model is its own name (no alias indirection). The spike (CORR-INFER #0,
// docs/SPIKE-CORR-INFER.md) confirmed a concrete entry persists, lists and deletes by id,
// whereas a wildcard does not.
//
// Sync is a full-sync over ONLY the concrete entries this reconciler manages (db_model == true
// AND model_name contains '/'):
//   - bound upstream absent in the gateway   → create (/model/new)
//   - managed concrete entry with no binding  → delete the orphan (/model/delete)
//   - content changed (upstream/key)          → delete + recreate
//
// Legacy bare aliases (groq-llama, …) and config.yaml models (db_model == false) are left
// untouched — they coexist and keep routing until CORR-INFER #5 migrates the workers off them.
//
// It is the only place that decrypts a provider API key (via internal/secretbox); the
// plaintext key is handed to the gateway client and never logged.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"rara-core/internal/litellm"
	"rara-core/internal/secretbox"
)

// llmProviderSync is an enabled, non-deleted provider with its encrypted key material — the
// server-side read the reconciler decrypts to register that provider's concrete models.
// Never exposed over HTTP.
type llmProviderSync struct {
	Kind          string
	BaseURL       string
	KeyCiphertext []byte
	KeyNonce      []byte
}

// litellmGateway is the slice of the LiteLLM admin client the reconciler needs. A fake
// implements it in tests; *litellm.Client implements it in production.
type litellmGateway interface {
	Health(ctx context.Context) error
	ListModels(ctx context.Context) ([]litellm.Model, error)
	AddModel(ctx context.Context, m litellm.Model) error
	DeleteModel(ctx context.Context, id string) error
}

// LLMReconciler drives one gateway from the rara registry.
type LLMReconciler struct {
	db  Database
	gw  litellmGateway
	box *secretbox.Box
}

// NewLLMReconciler wires a reconciler. box may be nil only if no enabled model has a key.
func NewLLMReconciler(db Database, gw litellmGateway, box *secretbox.Box) *LLMReconciler {
	return &LLMReconciler{db: db, gw: gw, box: box}
}

// newLLMReconcilerFromEnv builds a reconciler from LITELLM_BASE_URL + LITELLM_MASTER_KEY and
// the AES box from RARA_SECRETS_KEY. Fails fast if the gateway endpoint/key are missing; a
// missing RARA_SECRETS_KEY only errors later if an enabled model actually carries a key.
func newLLMReconcilerFromEnv(db Database) (*LLMReconciler, error) {
	gw, err := litellm.New(os.Getenv("LITELLM_BASE_URL"), os.Getenv("LITELLM_MASTER_KEY"))
	if err != nil {
		return nil, err
	}
	box, err := loadSecretbox()
	if err != nil {
		return nil, err
	}
	return NewLLMReconciler(db, gw, box), nil
}

// runReconcileLLM is the one-shot `core-job reconcile-llm` subcommand: sync the registry into
// the gateway once and exit. The always-on path is runReconcile --loop (level-triggered).
func runReconcileLLM(ctx context.Context, db Database) {
	r, err := newLLMReconcilerFromEnv(db)
	if err != nil {
		log.Fatalf("reconcile-llm: %v", err)
	}
	if err := r.Reconcile(ctx); err != nil {
		log.Fatalf("reconcile-llm: %v", err)
	}
	log.Println("rara-core: llm registry reconciled into gateway")
}

// Reconcile makes the gateway's db_model set match the enabled rara models exactly. It is
// level-triggered and idempotent: a second pass with no registry change performs zero writes.
// On a gateway error during read it aborts without writing; during apply it best-effort
// continues and returns the joined error so the next pass retries the failed items.
func (r *LLMReconciler) Reconcile(ctx context.Context) error {
	if err := r.gw.Health(ctx); err != nil {
		return fmt.Errorf("llm reconcile: gateway unavailable: %w", err)
	}
	desired, bound, err := r.desiredModels(ctx)
	if err != nil {
		return fmt.Errorf("llm reconcile: build desired: %w", err)
	}
	actual, err := r.gw.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("llm reconcile: list gateway models: %w", err)
	}

	create, deleteIDs := diffLLMModels(desired, actual, bound)

	var errs []error
	// Deletes first: an update is delete+create, and removing the stale row before adding
	// the new one avoids a duplicate model_name window.
	for _, id := range deleteIDs {
		if err := r.gw.DeleteModel(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	for _, m := range create {
		if err := r.gw.AddModel(ctx, m); err != nil {
			// m carries the plaintext key — log the alias only, never the model.
			errs = append(errs, fmt.Errorf("add %q: %w", m.ModelName, err))
		}
	}
	if len(create) > 0 || len(deleteIDs) > 0 {
		log.Printf("llm reconcile: +%d created, -%d deleted", len(create), len(deleteIDs))
	}
	if len(errs) > 0 {
		return fmt.Errorf("llm reconcile: %d apply error(s): %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// desiredModels builds the concrete gateway entries from the bound upstreams: for each enabled
// worker binding's "{kind}/{model}" it finds the matching enabled provider (by the prefix before
// the first '/' == llm_providers.kind), decrypts its key, and registers model_name == upstream.
//
// It returns the desired entries plus `bound`, the set of every valid-shaped bound upstream
// (resolvable or not). A bound upstream whose provider is absent/disabled/keyless is skipped from
// the desired set with a log — but it stays in `bound`, so the full-sync RETAINS its existing
// gateway model instead of deleting it. That keeps a transient provider misconfig from tearing down
// a model that an active binding still routes through (it does not abort the pass either).
func (r *LLMReconciler) desiredModels(ctx context.Context) (desired []litellm.Model, bound map[string]bool, err error) {
	upstreams, err := r.db.ListBoundUpstreams(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(upstreams) == 0 {
		return nil, nil, nil
	}
	provs, err := r.db.ListLLMProvidersForSync(ctx)
	if err != nil {
		return nil, nil, err
	}
	// One provider per kind. If an operator created two of the same kind, the first by id wins
	// (ListLLMProvidersForSync is ordered by id) — deterministic, and the duplicate is theirs to fix.
	byKind := make(map[string]llmProviderSync, len(provs))
	for _, p := range provs {
		if _, ok := byKind[p.Kind]; !ok {
			byKind[p.Kind] = p
		}
	}

	bound = make(map[string]bool, len(upstreams))
	desired = make([]litellm.Model, 0, len(upstreams))
	for _, up := range upstreams {
		// Split on the FIRST '/': kind is the prefix; model may itself contain '/'
		// (e.g. "groq/openai/gpt-oss-120b"). Both halves must be non-empty.
		kind, model, ok := strings.Cut(up, "/")
		if !ok || kind == "" || model == "" {
			continue // malformed upstream — not a valid "{kind}/{model}" (defensive; the query filters too)
		}
		bound[up] = true // a valid binding exists → never delete its gateway model as an "orphan"
		m, ok, err := r.resolveUpstream(up, kind, byKind)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			desired = append(desired, m)
		}
	}
	return desired, bound, nil
}

// resolveUpstream builds the concrete gateway model for one bound upstream by finding its provider
// (by kind) and decrypting the key. It returns ok=false (with a skip log) when the provider is
// absent/disabled/keyless — a soft skip that keeps the upstream bound (retained) without aborting
// the pass. A non-nil err is a hard failure (missing secretbox or a decrypt error). The decrypted
// key is placed on the returned Model and never logged.
func (r *LLMReconciler) resolveUpstream(up, kind string, byKind map[string]llmProviderSync) (litellm.Model, bool, error) {
	p, ok := byKind[kind]
	if !ok {
		log.Printf("llm reconcile: skip upstream %q — no enabled provider for kind %q", up, kind)
		return litellm.Model{}, false, nil
	}
	if len(p.KeyCiphertext) == 0 {
		log.Printf("llm reconcile: skip upstream %q — provider kind %q has no key", up, kind)
		return litellm.Model{}, false, nil
	}
	if r.box == nil {
		return litellm.Model{}, false, fmt.Errorf("upstream %q has an encrypted provider key but RARA_SECRETS_KEY is not configured", up)
	}
	key, err := r.box.Decrypt(p.KeyCiphertext, p.KeyNonce)
	if err != nil {
		return litellm.Model{}, false, fmt.Errorf("decrypt key for kind %q: %w", kind, err)
	}
	m := litellm.Model{
		ModelName: up,
		Upstream:  up,
		APIBase:   p.BaseURL, // set whenever a provider declares one (BYO/openai_compatible)
		APIKey:    string(key),
	}
	// Concrete models carry no per-model params/costs — litellm prices them from its own
	// cost-map. The fingerprint hashes empty params so drift detection stays consistent.
	m.Fingerprint = fingerprintModel(m, "{}")
	return m, true, nil
}

// diffLLMModels is the pure full-sync diff. It returns the models to create and the gateway
// ids to delete so the gateway's MANAGED concrete set converges to desired. Only db_model entries
// whose model_name is a concrete "{kind}/{model}" (contains '/') are managed; config.yaml models
// (DBModel==false) and legacy bare aliases (no '/') are ignored entirely.
//
// A managed model is deleted only when its name is NOT in `bound` — i.e. no active worker binding
// references it. A name that is bound but missing from desired (its provider is transiently
// unresolvable) is RETAINED: not recreated, not deleted, so a misconfig never tears down a live model.
func diffLLMModels(desired, actual []litellm.Model, bound map[string]bool) (create []litellm.Model, deleteIDs []string) {
	actualByName := make(map[string]litellm.Model, len(actual))
	for _, a := range actual {
		if !a.DBModel {
			continue // config.yaml model — read-only, never managed here
		}
		if !strings.Contains(a.ModelName, "/") {
			// ponytail: CORR-INFER #5 cleanup — legacy alias (no '/') deleted when unbound.
			// bound only contains kind/model strings, so legacy aliases are never retained via bound.
			if !bound[a.ModelName] {
				deleteIDs = append(deleteIDs, a.ID)
			}
			continue
		}
		if prev, ok := actualByName[a.ModelName]; ok {
			// Duplicate db_model alias (out-of-band create): keep one, delete the extra.
			deleteIDs = append(deleteIDs, prev.ID)
		}
		actualByName[a.ModelName] = a
	}

	desiredNames := make(map[string]bool, len(desired))
	for _, d := range desired {
		desiredNames[d.ModelName] = true
		a, ok := actualByName[d.ModelName]
		switch {
		case !ok:
			create = append(create, d)
		case a.Fingerprint != d.Fingerprint:
			deleteIDs = append(deleteIDs, a.ID)
			create = append(create, d)
		default:
			// identical → no-op (keeps the second pass at zero writes)
		}
	}
	for name, a := range actualByName {
		if !desiredNames[name] && !bound[name] {
			deleteIDs = append(deleteIDs, a.ID) // managed model with no active binding → orphan
		}
	}
	return create, deleteIDs
}

// ListBoundUpstreams feeds the LLM reconciler's desired set: the DISTINCT concrete upstreams
// ("{kind}/{model}") that enabled worker bindings pin via env->>'LITELLM_MODEL'. The regex requires
// a non-empty kind and a non-empty model around the first '/', so malformed values ("groq/", "/m")
// and legacy bare aliases (no '/') are excluded; a multi-slash model ("groq/openai/gpt-oss-120b")
// is kept (kind is the prefix before the first '/'). A NULL env value is excluded too.
func (d *pgxDatabase) ListBoundUpstreams(ctx context.Context) ([]string, error) {
	const q = `
		SELECT DISTINCT env->>'LITELLM_MODEL' AS upstream
		FROM providers
		WHERE enabled = true AND env->>'LITELLM_MODEL' ~ '^[^/]+/.+$'
		ORDER BY upstream`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list bound upstreams: query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var up string
		if err := rows.Scan(&up); err != nil {
			return nil, fmt.Errorf("list bound upstreams: scan: %w", err)
		}
		out = append(out, up)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list bound upstreams: rows: %w", err)
	}
	return out, nil
}

// ListLLMProvidersForSync feeds the LLM reconciler: enabled, non-deleted providers with their
// encrypted key material, so an upstream's kind prefix can be resolved to a provider and its key
// decrypted. Ordered by id so a duplicate kind resolves deterministically (first id wins).
func (d *pgxDatabase) ListLLMProvidersForSync(ctx context.Context) ([]llmProviderSync, error) {
	const q = `
		SELECT kind, COALESCE(base_url,''), key_ciphertext, key_nonce
		FROM llm_providers
		WHERE deleted_at IS NULL AND enabled
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list llm providers for sync: query: %w", err)
	}
	defer rows.Close()
	var out []llmProviderSync
	for rows.Next() {
		var s llmProviderSync
		if err := rows.Scan(&s.Kind, &s.BaseURL, &s.KeyCiphertext, &s.KeyNonce); err != nil {
			return nil, fmt.Errorf("list llm providers for sync: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list llm providers for sync: rows: %w", err)
	}
	return out, nil
}

// fingerprintModel hashes the full desired content of a model, including the decrypted key,
// so any change — upstream, cost, params, base URL, or a key rotation — yields a new
// fingerprint and triggers a recreate. It is stored in model_info.rara_fp and read back
// next pass to detect drift without comparing masked/normalized gateway fields.
func fingerprintModel(m litellm.Model, canonicalParams string) string {
	h := sha256.New()
	fmt.Fprintf(h, "name=%s\nupstream=%s\nbase=%s\nin=%s\nout=%s\nkey=%s\nparams=%s",
		m.ModelName, m.Upstream, m.APIBase,
		strconv.FormatFloat(m.InputCost, 'g', -1, 64),
		strconv.FormatFloat(m.OutputCost, 'g', -1, 64),
		m.APIKey, canonicalParams)
	return hex.EncodeToString(h.Sum(nil))
}
