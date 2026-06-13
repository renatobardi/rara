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
	const q = `SELECT id, lane, source_ref, flow_id, flow_version, status FROM items WHERE id = $1`
	var it Item
	err := d.conn.QueryRow(ctx, q, id).Scan(&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status)
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
		SELECT id, lane, source_ref, flow_id, flow_version, status
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
		if err := rows.Scan(&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status); err != nil {
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

// ClaimPendingStep implements the Postgres work-queue pull. The SELECT ... FOR UPDATE
// SKIP LOCKED inside a transaction is the whole point: concurrent workers each grab a
// distinct frontmost row, never the same one, with no broker. The claimed row is moved
// pending->running (heartbeat stamped, attempt bumped) before COMMIT, so it leaves the
// pending frontier atomically.
func (d *pgxDatabase) ClaimPendingStep(ctx context.Context, capability string) (*ItemStep, error) {
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
		WHERE capability = $1 AND status = 'pending'
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`
	var s ItemStep
	err = tx.QueryRow(ctx, sel, capability).Scan(
		&s.ItemID, &s.Seq, &s.Capability, &s.Status,
		&s.AssignedProvider, &s.Attempt, &s.OutputRef, &s.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // queue empty for this capability
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
