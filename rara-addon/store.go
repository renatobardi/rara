// store.go — the persistence seam of the SDK.
//
// Store is the minimal contract the loop needs: the atomic claim, the heartbeat, the item read,
// the step result writes (mark/requeue) and the curate-out (filter). PgxStore is the reference
// implementation on Postgres/Neon for external Go workers; tests use a fake. rara-core wires its
// own existing persistence to this interface via a thin adapter instead of PgxStore, so the claim
// SQL is defined and tested once per backing store.
//
// The SDK owns only the CONTRACT tables here: it reads items, claims/writes item_steps, and stamps
// providers.heartbeat_at. It never touches a domain table — that is the worker's, behind the
// Handler.
package addon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Store is the persistence seam. A real implementation is atomic where it must be (Claim); the
// rest are single statements. Heartbeat is best-effort (an unknown provider is a no-op).
//
// Implementations MUST be safe for concurrent use: in resident mode Run calls Claim/GetItem/Mark
// from the drain loop while a background goroutine calls Heartbeat. PgxStore inherits this from its
// PgxConn — a *pgxpool.Pool is concurrency-safe; a single *pgx.Conn is NOT, so back a resident
// worker with a pool (or serialize access in the adapter).
type Store interface {
	// Claim atomically pulls the frontmost pending step for (capability, provider):
	//   SELECT ... WHERE capability=$1 AND assigned_provider=$2 AND status='pending'
	//   ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
	// then transitions it pending->running, bumps attempt and stamps the heartbeat. The provider
	// filter is the isolation guarantee: a worker never claims a sibling provider's step. Returns
	// (nil, nil) when the queue is empty.
	Claim(ctx context.Context, capability, provider string) (*Step, error)

	// Heartbeat stamps providers.heartbeat_at = now for a live provider. Best-effort: an unknown
	// name updates zero rows and returns nil. It touches only heartbeat_at, never config columns.
	Heartbeat(ctx context.Context, provider string) error

	// GetItem returns the spine item by id (found=false if it vanished).
	GetItem(ctx context.Context, id int) (Item, bool, error)

	// Mark writes a step terminal (StatusDone with outputRef, or StatusFailed with errMsg). The
	// step carries the fields a full-record backing store needs to preserve (capability,
	// assigned_provider, attempt, heartbeat).
	Mark(ctx context.Context, step Step, status, outputRef, errMsg string) error

	// Requeue returns a transiently-failed step to the pending frontier (heartbeat cleared so it
	// reads as un-owned, output cleared, error recorded). attempt is left as the claim bumped it so
	// the ceiling is reached.
	Requeue(ctx context.Context, step Step, errMsg string) error

	// FilterItem curates an item out (terminal `filtered`) — the benign no-content path. Idempotent.
	FilterItem(ctx context.Context, item Item) error
}

// PgxConn is the subset of pgx used by PgxStore; both *pgx.Conn and *pgxpool.Pool satisfy it, so a
// worker can back the store with a single connection or a pool.
type PgxConn interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PgxStore is the Postgres/Neon implementation of Store for external Go workers.
type PgxStore struct{ conn PgxConn }

// NewPgxStore wraps a pgx connection or pool as a Store.
func NewPgxStore(conn PgxConn) *PgxStore { return &PgxStore{conn: conn} }

var _ Store = (*PgxStore)(nil)

// Claim implements the Postgres work-queue pull. The SELECT ... FOR UPDATE SKIP LOCKED inside a
// transaction is the whole point: concurrent workers each grab a distinct frontmost row, never the
// same one, with no broker.
func (s *PgxStore) Claim(ctx context.Context, capability, provider string) (*Step, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	const sel = `
		SELECT item_id, seq, capability, COALESCE(assigned_provider, ''), attempt
		FROM item_steps
		WHERE capability = $1 AND assigned_provider = $2 AND status = 'pending'
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`
	var st Step
	err = tx.QueryRow(ctx, sel, capability, provider).Scan(
		&st.ItemID, &st.Seq, &st.Capability, &st.AssignedProvider, &st.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // queue empty for this (capability, provider)
	}
	if err != nil {
		return nil, err
	}

	const upd = `
		UPDATE item_steps
		SET status = 'running', attempt = attempt + 1, heartbeat_at = CURRENT_TIMESTAMP
		WHERE item_id = $1 AND seq = $2
		RETURNING attempt, heartbeat_at`
	if err := tx.QueryRow(ctx, upd, st.ItemID, st.Seq).Scan(&st.Attempt, &st.HeartbeatAt); err != nil {
		return nil, fmt.Errorf("claim transition: %w", err)
	}
	st.Status = StatusRunning
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *PgxStore) Heartbeat(ctx context.Context, provider string) error {
	const q = `UPDATE providers SET heartbeat_at = CURRENT_TIMESTAMP WHERE name = $1`
	_, err := s.conn.Exec(ctx, q, provider)
	return err
}

func (s *PgxStore) GetItem(ctx context.Context, id int) (Item, bool, error) {
	const q = `SELECT id, lane, source_ref, flow_id, flow_version, status, sensitivity FROM items WHERE id = $1`
	var it Item
	err := s.conn.QueryRow(ctx, q, id).Scan(
		&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status, &it.Sensitivity)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	return it, true, nil
}

// Mark writes the step terminal with a targeted UPDATE; the claimed row already exists, so the
// heartbeat the claim stamped is preserved untouched.
func (s *PgxStore) Mark(ctx context.Context, step Step, status, outputRef, errMsg string) error {
	if status != StatusDone && status != StatusFailed {
		return fmt.Errorf("addon: Mark: invalid terminal status %q", status)
	}
	const q = `
		UPDATE item_steps
		SET status = $3, output_ref = NULLIF($4, ''), error = NULLIF($5, '')
		WHERE item_id = $1 AND seq = $2`
	_, err := s.conn.Exec(ctx, q, step.ItemID, step.Seq, status, outputRef, errMsg)
	return err
}

func (s *PgxStore) Requeue(ctx context.Context, step Step, errMsg string) error {
	const q = `
		UPDATE item_steps
		SET status = 'pending', heartbeat_at = NULL, output_ref = NULL, error = NULLIF($3, '')
		WHERE item_id = $1 AND seq = $2`
	_, err := s.conn.Exec(ctx, q, step.ItemID, step.Seq, errMsg)
	return err
}

func (s *PgxStore) FilterItem(ctx context.Context, item Item) error {
	const q = `UPDATE items SET status = 'filtered' WHERE id = $1 AND status <> 'filtered'`
	_, err := s.conn.Exec(ctx, q, item.ID)
	return err
}
