// store_reads.go — the read + claim half of the persistence seam (Phase 1).
//
// Phase 0 shipped a write-only store (idempotent upserts + append-only inserts).
// The reconciler needs to OBSERVE state to act on it, and the worker needs to PULL
// its assignment; this file adds those pgx implementations. They mirror, on the real
// database, the same contract the in-memory MockDatabase enforces in the tests.
//
// Nothing here makes a routing or scheduling decision — these are pure reads and one
// atomic claim. The decisions live in reconciler.go (control plane) and worker.go
// (the pull side).
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (d *pgxDatabase) GetFlow(ctx context.Context, name string) (Flow, bool, error) {
	const q = `SELECT id, name, source_type, enabled, version FROM flows WHERE name = $1`
	var f Flow
	err := d.conn.QueryRow(ctx, q, name).Scan(&f.ID, &f.Name, &f.SourceType, &f.Enabled, &f.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Flow{}, false, nil
	}
	if err != nil {
		return Flow{}, false, err
	}
	return f, true, nil
}

func (d *pgxDatabase) GetItem(ctx context.Context, id int) (Item, bool, error) {
	const q = `SELECT id, lane, source_ref, flow_id, flow_version, status, sensitivity FROM items WHERE id = $1`
	var it Item
	err := d.conn.QueryRow(ctx, q, id).Scan(&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status, &it.Sensitivity)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	return it, true, nil
}

func (d *pgxDatabase) ListActiveItems(ctx context.Context) ([]Item, error) {
	// Terminal statuses are excluded; the index idx_items_status backs this scan.
	const q = `
		SELECT id, lane, source_ref, flow_id, flow_version, status, sensitivity
		FROM items
		WHERE status NOT IN ('done', 'filtered', 'failed', 'quarantine')
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status, &it.Sensitivity); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) ListFlowSteps(ctx context.Context, flowID int) ([]FlowStep, error) {
	const q = `
		SELECT flow_id, seq, capability, options, enabled
		FROM flow_steps
		WHERE flow_id = $1 AND enabled = true
		ORDER BY seq`
	rows, err := d.conn.Query(ctx, q, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowStep
	for rows.Next() {
		var s FlowStep
		if err := rows.Scan(&s.FlowID, &s.Seq, &s.Capability, &s.Options, &s.Enabled); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) ListItemSteps(ctx context.Context, itemID int) ([]ItemStep, error) {
	const q = `
		SELECT item_id, seq, capability, status,
		       COALESCE(assigned_provider, ''), attempt, heartbeat_at,
		       COALESCE(output_ref, ''), COALESCE(error, '')
		FROM item_steps
		WHERE item_id = $1
		ORDER BY seq`
	rows, err := d.conn.Query(ctx, q, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemStep
	for rows.Next() {
		var s ItemStep
		if err := rows.Scan(&s.ItemID, &s.Seq, &s.Capability, &s.Status,
			&s.AssignedProvider, &s.Attempt, &s.HeartbeatAt, &s.OutputRef, &s.Error); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) ListProvidersForCapability(ctx context.Context, capability string) ([]Provider, error) {
	// idx_providers_capability (partial, enabled=true) backs this lookup.
	const q = `
		SELECT name, capability, runtime, activation, cost, quality, latency_ms,
		       constraints, enabled, heartbeat_at
		FROM providers
		WHERE capability = $1 AND enabled = true
		ORDER BY name`
	rows, err := d.conn.Query(ctx, q, capability)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Name, &p.Capability, &p.Runtime, &p.Activation, &p.Cost,
			&p.Quality, &p.LatencyMs, &p.Constraints, &p.Enabled, &p.HeartbeatAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) GetProvider(ctx context.Context, name string) (Provider, bool, error) {
	const q = `
		SELECT name, capability, runtime, activation, cost, quality, latency_ms,
		       constraints, enabled, heartbeat_at
		FROM providers
		WHERE name = $1`
	var p Provider
	err := d.conn.QueryRow(ctx, q, name).Scan(&p.Name, &p.Capability, &p.Runtime, &p.Activation,
		&p.Cost, &p.Quality, &p.LatencyMs, &p.Constraints, &p.Enabled, &p.HeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, false, nil
	}
	if err != nil {
		return Provider{}, false, err
	}
	return p, true, nil
}

func (d *pgxDatabase) GetRoutingPolicy(ctx context.Context, scope string) (RoutingPolicy, bool, error) {
	const q = `SELECT scope, cost_weight, quality_weight, fallback FROM routing_policies WHERE scope = $1`
	var p RoutingPolicy
	err := d.conn.QueryRow(ctx, q, scope).Scan(&p.Scope, &p.CostWeight, &p.QualityWeight, &p.Fallback)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoutingPolicy{}, false, nil
	}
	if err != nil {
		return RoutingPolicy{}, false, err
	}
	return p, true, nil
}

// ListGateRules returns the enabled allow/deny rules in a deterministic order
// (action, match_type, value) so the audit reason is stable. Order does not affect the
// outcome — the cascade enforces deny precedence regardless (applyRules). The partial
// idx_gate_rules_enabled backs the scan.
func (d *pgxDatabase) ListGateRules(ctx context.Context) ([]GateRule, error) {
	const q = `
		SELECT action, match_type, value, enabled
		FROM gate_rules
		WHERE enabled = true
		ORDER BY action, match_type, value`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateRule
	for rows.Next() {
		var r GateRule
		if err := rows.Scan(&r.Action, &r.MatchType, &r.Value, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLatestInterestProfile returns the highest-version profile row. UNIQUE(version) plus
// MAX(version) gives the single live document the gate cascade reads.
func (d *pgxDatabase) GetLatestInterestProfile(ctx context.Context) (InterestProfile, bool, error) {
	const q = `
		SELECT version, topics, authors, anti_topics, weights
		FROM interest_profile
		ORDER BY version DESC
		LIMIT 1`
	var p InterestProfile
	err := d.conn.QueryRow(ctx, q).Scan(&p.Version, &p.Topics, &p.Authors, &p.AntiTopics, &p.Weights)
	if errors.Is(err, pgx.ErrNoRows) {
		return InterestProfile{}, false, nil
	}
	if err != nil {
		return InterestProfile{}, false, err
	}
	return p, true, nil
}

// LatestGateDecision returns the most recent decision for (item, gate). gate_decisions is
// append-only, so the highest id is the latest run; idx_gate_decisions_item backs the scan.
func (d *pgxDatabase) LatestGateDecision(ctx context.Context, itemID int, gate string) (GateDecision, bool, error) {
	const q = `
		SELECT item_id, gate, decision, score, rank, decided_by, COALESCE(reason, '')
		FROM gate_decisions
		WHERE item_id = $1 AND gate = $2
		ORDER BY id DESC
		LIMIT 1`
	var dec GateDecision
	err := d.conn.QueryRow(ctx, q, itemID, gate).Scan(
		&dec.ItemID, &dec.Gate, &dec.Decision, &dec.Score, &dec.Rank, &dec.DecidedBy, &dec.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return GateDecision{}, false, nil
	}
	if err != nil {
		return GateDecision{}, false, err
	}
	return dec, true, nil
}

// ListQuarantinedItems returns the deferred (quarantine) items — the cold-start review
// sample. idx_items_status backs the scan.
func (d *pgxDatabase) ListQuarantinedItems(ctx context.Context) ([]Item, error) {
	const q = `
		SELECT id, lane, source_ref, flow_id, flow_version, status, sensitivity
		FROM items
		WHERE status = 'quarantine'
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status, &it.Sensitivity); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// TouchProviderHeartbeat stamps heartbeat_at = now for a live provider. An unknown name
// updates zero rows (no error) — liveness is best-effort. It deliberately touches only
// heartbeat_at, never the config columns (unlike the full-record UpsertProvider).
func (d *pgxDatabase) TouchProviderHeartbeat(ctx context.Context, name string) error {
	const q = `UPDATE providers SET heartbeat_at = CURRENT_TIMESTAMP WHERE name = $1`
	_, err := d.conn.Exec(ctx, q, name)
	return err
}

// ClaimPendingStep implements the Postgres work-queue pull. The SELECT ... FOR UPDATE
// SKIP LOCKED inside a transaction is the whole point: concurrent workers each grab a
// distinct frontmost row, never the same one, with no broker. The claimed row is moved
// pending->running (heartbeat stamped, attempt bumped) before COMMIT, so it leaves the
// pending frontier atomically.
func (d *pgxDatabase) ClaimPendingStep(ctx context.Context, capability, provider string) (*ItemStep, error) {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	const sel = `
		SELECT item_id, seq, capability, status,
		       COALESCE(assigned_provider, ''), attempt,
		       COALESCE(output_ref, ''), COALESCE(error, '')
		FROM item_steps
		WHERE capability = $1 AND assigned_provider = $2 AND status = 'pending'
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`
	var s ItemStep
	err = tx.QueryRow(ctx, sel, capability, provider).Scan(
		&s.ItemID, &s.Seq, &s.Capability, &s.Status,
		&s.AssignedProvider, &s.Attempt, &s.OutputRef, &s.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // queue empty for this capability+provider
	}
	if err != nil {
		return nil, err
	}

	const upd = `
		UPDATE item_steps
		SET status = 'running', attempt = attempt + 1, heartbeat_at = CURRENT_TIMESTAMP
		WHERE item_id = $1 AND seq = $2
		RETURNING attempt, heartbeat_at`
	if err := tx.QueryRow(ctx, upd, s.ItemID, s.Seq).Scan(&s.Attempt, &s.HeartbeatAt); err != nil {
		return nil, fmt.Errorf("claim transition: %w", err)
	}
	s.Status = stepRunning
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &s, nil
}
