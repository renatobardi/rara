// db.go — pgx implementation of DispatchDB. All queries are read-only: the dispatcher never
// mutates item_steps; that is the reconciler's (rara-core) and the worker's (rara-addon) job.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxDispatchErrorRunes is the rune cap applied to last_error before writing to Postgres.
// 1000 runes covers any human-readable error message while guarding against unbounded blobs.
const maxDispatchErrorRunes = 1000

// capDispatchError truncates s to maxDispatchErrorRunes runes (not bytes) so multi-byte UTF-8
// characters are never split.
func capDispatchError(s string) string {
	if utf8.RuneCountInString(s) <= maxDispatchErrorRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxDispatchErrorRunes {
			return s[:i]
		}
		n++
	}
	return s
}

// pgxDispatchDB implements DispatchDB against a live Neon PostgreSQL pool.
type pgxDispatchDB struct {
	pool *pgxpool.Pool
}

// compile-time interface compliance — catches drift between the concrete type and the interface.
var _ DispatchDB = &pgxDispatchDB{}

func (d *pgxDispatchDB) ListAssignedSteps(ctx context.Context) ([]AssignedStep, error) {
	const q = `
		SELECT item_id, seq, capability, COALESCE(assigned_provider, '')
		FROM item_steps
		WHERE status = 'pending' AND assigned_provider IS NOT NULL
		ORDER BY id`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list assigned steps query: %w", err)
	}
	defer rows.Close()
	var out []AssignedStep
	for rows.Next() {
		var s AssignedStep
		if err := rows.Scan(&s.ItemID, &s.Seq, &s.Capability, &s.AssignedProvider); err != nil {
			return nil, fmt.Errorf("list assigned steps scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list assigned steps rows: %w", err)
	}
	return out, nil
}

func (d *pgxDispatchDB) GetProvider(ctx context.Context, name string) (DispatchProvider, bool, error) {
	// env is JSONB; cast to text so we deserialize it ourselves (parseProviderEnv) — keeps the
	// empty/'{}'/populated handling in one tested seam. COALESCE guards a NULL column.
	const q = `
		SELECT name, runtime, activation, COALESCE(runner_url, ''), COALESCE(env::text, '{}')
		FROM providers
		WHERE name = $1`
	var p DispatchProvider
	var envJSON string
	err := d.pool.QueryRow(ctx, q, name).Scan(&p.Name, &p.Runtime, &p.Activation, &p.RunnerURL, &envJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DispatchProvider{}, false, nil
		}
		return DispatchProvider{}, false, fmt.Errorf("get provider %q: %w", name, err)
	}
	p.Env, err = parseProviderEnv(envJSON)
	if err != nil {
		return DispatchProvider{}, false, fmt.Errorf("get provider %q env: %w", name, err)
	}
	return p, true, nil
}

// parseProviderEnv deserializes the providers.env JSONB (as text) into a non-nil map. Empty input
// or '{}' yields an empty map so the dispatcher and transports never nil-panic on a config-less
// provider. providers.env is constrained to a JSON object upstream (CodeRabbit #133).
func parseProviderEnv(raw string) (map[string]string, error) {
	env := map[string]string{}
	if raw == "" || raw == "{}" {
		return env, nil
	}
	if len(raw) > maxProviderEnvBytes {
		return nil, fmt.Errorf("provider env too large: %d bytes (max %d)", len(raw), maxProviderEnvBytes)
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("parse provider env JSON: %w", err)
	}
	return env, nil
}

func (d *pgxDispatchDB) ListDueCollectors(ctx context.Context) ([]DispatchProvider, error) {
	const q = `
		SELECT name, runtime, activation, COALESCE(runner_url, ''), COALESCE(env::text, '{}')
		FROM providers
		WHERE collect_cadence_seconds IS NOT NULL
		  AND enabled = true
		  AND (last_collect_at IS NULL
		       OR now() - last_collect_at > collect_cadence_seconds * interval '1 second')
		  AND (last_attempt_at IS NULL
		       OR retry_interval_seconds IS NULL
		       OR now() - last_attempt_at > retry_interval_seconds * interval '1 second')
		ORDER BY name`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list due collectors query: %w", err)
	}
	defer rows.Close()
	var out []DispatchProvider
	for rows.Next() {
		var p DispatchProvider
		var envJSON string
		if err := rows.Scan(&p.Name, &p.Runtime, &p.Activation, &p.RunnerURL, &envJSON); err != nil {
			return nil, fmt.Errorf("list due collectors scan: %w", err)
		}
		p.Env, err = parseProviderEnv(envJSON)
		if err != nil {
			return nil, fmt.Errorf("list due collectors env %q: %w", p.Name, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due collectors rows: %w", err)
	}
	return out, nil
}

func (d *pgxDispatchDB) TouchCollectorAttempted(ctx context.Context, name string) error {
	const q = `UPDATE providers SET last_attempt_at = now() WHERE name = $1`
	_, err := d.pool.Exec(ctx, q, name)
	if err != nil {
		return fmt.Errorf("touch attempt %q: %w", name, err)
	}
	return nil
}

// maxProviderEnvBytes caps the JSONB we deserialize — defense-in-depth against a runaway env blob,
// independent of the upstream JSON-object constraint. Per-run config is a handful of small vars.
const maxProviderEnvBytes = 10240

func (d *pgxDispatchDB) StampDispatchError(ctx context.Context, name, msg string) error {
	const q = `UPDATE providers SET last_error = $2, last_attempt_at = now() WHERE name = $1`
	if _, err := d.pool.Exec(ctx, q, name, capDispatchError(msg)); err != nil {
		return fmt.Errorf("stamp dispatch error %q: %w", name, err)
	}
	return nil
}

func (d *pgxDispatchDB) ClearDispatchError(ctx context.Context, name string) error {
	const q = `UPDATE providers SET last_error = NULL WHERE name = $1`
	if _, err := d.pool.Exec(ctx, q, name); err != nil {
		return fmt.Errorf("clear dispatch error %q: %w", name, err)
	}
	return nil
}
