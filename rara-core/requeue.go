// requeue.go — `core-job requeue` command: reset failed item_steps back to pending.
//
// Encodes the "fix-failed-by-SQL-on-Neon" workflow as a safe, idempotent operation:
// steps matching (capability, fromStatus) are reset atomically with their parent item's
// status, so the reconciler picks them up on the next pass with no manual SQL.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

// capabilityItemStatus is the minimum map from capability to the item lifecycle status
// that item should be at when that capability's steps are re-enqueued. Derived from the
// YouTube flow rail; capabilities not in this map require an explicit --item-status flag.
var capabilityItemStatus = map[string]string{
	capGateBarato:  itemDiscovered,
	capTranscrever: itemToText,
	capGateRico:    itemToText,
	capDestilar:    itemDistilled,
}

func (d *pgxDatabase) RequeueSteps(ctx context.Context, capability, fromStatus string, limit int, itemStatus string) (int, error) {
	if !isValidStepStatus(fromStatus) {
		return 0, fmt.Errorf("invalid step status %q", fromStatus)
	}
	if !isValidItemStatus(itemStatus) {
		return 0, fmt.Errorf("invalid item status %q", itemStatus)
	}

	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("requeue: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the rows to reset; order by id for deterministic FIFO processing.
	var rows pgx.Rows
	if limit > 0 {
		rows, err = tx.Query(ctx,
			`SELECT id, item_id FROM item_steps WHERE capability=$1 AND status=$2 ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED`,
			capability, fromStatus, limit)
	} else {
		rows, err = tx.Query(ctx,
			`SELECT id, item_id FROM item_steps WHERE capability=$1 AND status=$2 ORDER BY id FOR UPDATE SKIP LOCKED`,
			capability, fromStatus)
	}
	if err != nil {
		return 0, err
	}

	var stepIDs []int
	itemSet := make(map[int]struct{})
	for rows.Next() {
		var sid, iid int
		if err := rows.Scan(&sid, &iid); err != nil {
			rows.Close()
			return 0, err
		}
		stepIDs = append(stepIDs, sid)
		itemSet[iid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(stepIDs) == 0 {
		return 0, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE item_steps SET status='pending', attempt=0, heartbeat_at=NULL, assigned_provider=NULL, error=NULL WHERE id=ANY($1)`,
		stepIDs); err != nil {
		return 0, err
	}

	itemIDs := make([]int, 0, len(itemSet))
	for id := range itemSet {
		itemIDs = append(itemIDs, id)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE items SET status=$1 WHERE id=ANY($2)`,
		itemStatus, itemIDs); err != nil {
		return 0, err
	}

	return len(stepIDs), tx.Commit(ctx)
}

// runRequeue implements `core-job requeue --capability <cap> [--status <s>] [--limit N] [--item-status <s>]`.
func runRequeue(ctx context.Context, db Database, argv []string) {
	fs := flag.NewFlagSet("requeue", flag.ExitOnError)
	capability := fs.String("capability", "", "capability whose steps to requeue (required)")
	fromStatus := fs.String("status", stepFailed, "source step status to match (default: failed)")
	limit := fs.Int("limit", 0, "max steps to requeue, 0 = no limit")
	itemStatus := fs.String("item-status", "", "override item status (default: derived from capability)")
	_ = fs.Parse(argv)

	if *capability == "" {
		log.Fatalf("requeue: --capability is required")
	}
	if !isValidStepStatus(*fromStatus) {
		log.Fatalf("requeue: invalid --status %q", *fromStatus)
	}

	target := *itemStatus
	if target == "" {
		var ok bool
		target, ok = capabilityItemStatus[*capability]
		if !ok {
			log.Fatalf("requeue: unknown capability %q; provide --item-status explicitly", *capability)
		}
	}
	if !isValidItemStatus(target) {
		log.Fatalf("requeue: invalid --item-status %q", target)
	}

	n, err := db.RequeueSteps(ctx, *capability, *fromStatus, *limit, target)
	if err != nil {
		log.Fatalf("requeue: %v", err)
	}
	log.Printf("rara-core: requeued %d steps for capability=%s (item status → %s)", n, *capability, target)
}
