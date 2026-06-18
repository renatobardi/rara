// db.go — pgx implementation of DispatchDB. All queries are read-only: the dispatcher never
// mutates item_steps; that is the reconciler's (rara-core) and the worker's (rara-addon) job.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
	const q = `
		SELECT name, runtime, activation, COALESCE(runner_url, '')
		FROM providers
		WHERE name = $1`
	var p DispatchProvider
	err := d.pool.QueryRow(ctx, q, name).Scan(&p.Name, &p.Runtime, &p.Activation, &p.RunnerURL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DispatchProvider{}, false, nil
		}
		return DispatchProvider{}, false, fmt.Errorf("get provider %q: %w", name, err)
	}
	return p, true, nil
}
