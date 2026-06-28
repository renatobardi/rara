// llm_reconcile.go — materializes the rara LLM registry (llm_providers + llm_models) into
// the LiteLLM gateway, level-triggered and idempotent.
//
// INVARIANT: this reconciler is the ONLY writer of the gateway's DB-backed model registry.
// Sync is a full-sync over the enabled models in Neon:
//   - enabled alias in Neon, absent in the gateway        → create (/model/new)
//   - present in the gateway as a db_model, but the alias  → delete the orphan (/model/delete)
//     is disabled/soft-deleted/absent in Neon
//   - content changed (upstream/cost/params/key)           → delete + recreate
//
// Models that came from config.yaml (model_info.db_model == false) are READ-ONLY and never
// touched — only db_model == true entries are managed here. The source of truth is the rara
// tables; anything created out-of-band via the Admin UI is removed on the next pass.
//
// It is the only place that decrypts a provider API key (via internal/secretbox); the
// plaintext key is handed to the gateway client and never logged.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"rara-core/internal/litellm"
	"rara-core/internal/secretbox"
)

// llmModelSync is one enabled model joined to its enabled provider, including the encrypted
// key material — the server-side read the reconciler decrypts. Never exposed over HTTP.
type llmModelSync struct {
	Alias         string
	Upstream      string
	ProviderKind  string
	BaseURL       string
	KeyCiphertext []byte
	KeyNonce      []byte
	InputCost     float64
	OutputCost    float64
	Params        json.RawMessage
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
	desired, err := r.desiredModels(ctx)
	if err != nil {
		return fmt.Errorf("llm reconcile: build desired: %w", err)
	}
	actual, err := r.gw.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("llm reconcile: list gateway models: %w", err)
	}

	create, deleteIDs := diffLLMModels(desired, actual)

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

// desiredModels reads the enabled registry and decrypts each key into a gateway model spec.
func (r *LLMReconciler) desiredModels(ctx context.Context) ([]litellm.Model, error) {
	rows, err := r.db.ListEnabledLLMModelsForSync(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]litellm.Model, 0, len(rows))
	for _, row := range rows {
		m := litellm.Model{
			ModelName:  row.Alias,
			Upstream:   row.Upstream,
			InputCost:  row.InputCost,
			OutputCost: row.OutputCost,
			APIBase:    row.BaseURL, // set whenever a provider declares one (BYO/openai_compatible)
		}
		if len(row.KeyCiphertext) > 0 {
			if r.box == nil {
				return nil, fmt.Errorf("model %q has an encrypted key but RARA_SECRETS_KEY is not configured", row.Alias)
			}
			key, err := r.box.Decrypt(row.KeyCiphertext, row.KeyNonce)
			if err != nil {
				return nil, fmt.Errorf("decrypt key for model %q: %w", row.Alias, err)
			}
			m.APIKey = string(key)
		}
		params, canonical, err := canonicalParams(row.Params)
		if err != nil {
			return nil, fmt.Errorf("model %q params: %w", row.Alias, err)
		}
		m.Params = params
		m.Fingerprint = fingerprintModel(m, canonical)
		out = append(out, m)
	}
	return out, nil
}

// diffLLMModels is the pure full-sync diff. It returns the models to create and the gateway
// ids to delete so the gateway's db_model set equals desired exactly. config.yaml models
// (DBModel==false) are ignored entirely.
func diffLLMModels(desired, actual []litellm.Model) (create []litellm.Model, deleteIDs []string) {
	actualByName := make(map[string]litellm.Model, len(actual))
	for _, a := range actual {
		if !a.DBModel {
			continue // config.yaml model — read-only, never managed here
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
		if !desiredNames[name] {
			deleteIDs = append(deleteIDs, a.ID)
		}
	}
	return create, deleteIDs
}

// canonicalParams parses the model's params jsonb into a map (for the gateway client) and a
// canonical, key-sorted JSON encoding (for the fingerprint). Empty/null params → empty map.
func canonicalParams(raw json.RawMessage) (map[string]any, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, "{}", nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(m) // json.Marshal sorts map keys → stable encoding
	if err != nil {
		return nil, "", err
	}
	return m, string(canonical), nil
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
