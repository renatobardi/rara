// store_reads_test.go — resilient surface item reads (A3).
//
// The two enriched reads (ListItemsByStatus / ListQuarantinedItems) LEFT JOIN each lane's
// own domain table (emails / channel_videos / target_channels / linkedin_posts / ...). A
// lane that hasn't been deployed never created its table, so Postgres raises 42P01
// (undefined_table) and — without the degradation below — the WHOLE query fails, taking
// down /v1/items and /v1/quarantine for EVERY lane, not just the absent one.
//
// These tests drive the resilient read over a fake pgConn (zero I/O): a missing-table
// error degrades to the base projection (items, no display), all-present populates display
// as before, and a non-42P01 error still propagates.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- fake pgConn / pgx.Rows (display-read seam) ------------------------------

// displayRow is one row in the itemDisplaySelect/itemBaseSelect column shape.
type displayRow struct {
	id          int
	lane        string
	sourceRef   string
	flowID      int
	flowVersion int
	status      string
	sensitivity string
	title       string
	channel     string
	summary     string
}

// fakeDisplayConn is a pgConn whose Query distinguishes the enriched read (the SELECT
// carries the LEFT JOINs) from the degraded base read. The enriched read fails with
// queryErr (e.g. a 42P01) when set; the base read always returns baseRows. This lets a
// test assert the fallback path without a real database.
type fakeDisplayConn struct {
	pgConn // unused methods (QueryRow/Exec/Begin) — nil, never called here

	queryErr error        // returned by the enriched (JOIN) query when non-nil
	baseRows []displayRow // returned by the degraded (no-JOIN) query
	fullRows []displayRow // returned by the enriched query when queryErr is nil

	enrichedCalls int
	baseCalls     int
}

func (c *fakeDisplayConn) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "LEFT JOIN") {
		c.enrichedCalls++
		if c.queryErr != nil {
			return nil, c.queryErr
		}
		return &fakeRows{rows: c.fullRows}, nil
	}
	c.baseCalls++
	return &fakeRows{rows: c.baseRows}, nil
}

// fakeRows is a minimal pgx.Rows over a slice of displayRow. Only Next/Scan/Close/Err are
// exercised; the rest are inherited (nil) from the embedded interface and never called.
type fakeRows struct {
	pgx.Rows

	rows []displayRow
	idx  int
	cur  displayRow
}

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.cur = r.rows[r.idx]
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	*dest[0].(*int) = r.cur.id
	*dest[1].(*string) = r.cur.lane
	*dest[2].(*string) = r.cur.sourceRef
	*dest[3].(*int) = r.cur.flowID
	*dest[4].(*int) = r.cur.flowVersion
	*dest[5].(*string) = r.cur.status
	*dest[6].(*string) = r.cur.sensitivity
	*dest[7].(*string) = r.cur.title
	*dest[8].(*string) = r.cur.channel
	*dest[9].(*string) = r.cur.summary
	// dest[10] is *(*time.Time) for published_at — left nil; not asserted here.
	return nil
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }

// --- tests -------------------------------------------------------------------

// When a lane's domain table is absent (42P01), the read degrades to the base projection:
// the items still come back, just without the display fields — never a 500.
func TestListItemsByStatus_DegradesWhenLaneTableAbsent(t *testing.T) {
	conn := &fakeDisplayConn{
		queryErr: &pgconn.PgError{Code: "42P01", Message: `relation "emails" does not exist`},
		baseRows: []displayRow{
			{id: 1, lane: "email", sourceRef: "m-1", status: "ready"},
			{id: 2, lane: "podcast", sourceRef: "g-2", status: "ready"},
		},
	}
	d := &pgxDatabase{conn: conn}

	items, err := d.ListItemsByStatus(context.Background(), "ready")
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	if conn.enrichedCalls != 1 || conn.baseCalls != 1 {
		t.Fatalf("expected one enriched + one base query, got enriched=%d base=%d", conn.enrichedCalls, conn.baseCalls)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	for _, it := range items {
		if it.Title != "" || it.Channel != "" || it.Summary != "" || it.PublishedAt != nil {
			t.Errorf("item %d: expected empty display fields in degraded read, got %+v", it.ID, it)
		}
	}
}

// The fix is shared, so /v1/quarantine degrades the same way as /v1/items.
func TestListQuarantinedItems_DegradesWhenLaneTableAbsent(t *testing.T) {
	conn := &fakeDisplayConn{
		queryErr: &pgconn.PgError{Code: "42P01", Message: `relation "channel_videos" does not exist`},
		baseRows: []displayRow{{id: 7, lane: "youtube", sourceRef: "v-7", status: "quarantine"}},
	}
	d := &pgxDatabase{conn: conn}

	items, err := d.ListQuarantinedItems(context.Background())
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	if conn.baseCalls != 1 {
		t.Fatalf("expected the base fallback to run once, got %d", conn.baseCalls)
	}
	if len(items) != 1 || items[0].ID != 7 {
		t.Fatalf("expected the quarantined item back, got %+v", items)
	}
	if items[0].Title != "" {
		t.Errorf("expected empty display in degraded read, got title=%q", items[0].Title)
	}
}

// All tables present: the enriched read succeeds and display is populated as today — no
// fallback, JSON shape unchanged.
func TestListItemsByStatus_PopulatesDisplayWhenAllTablesPresent(t *testing.T) {
	conn := &fakeDisplayConn{
		fullRows: []displayRow{
			{id: 1, lane: "email", sourceRef: "m-1", status: "ready",
				title: "Q2 report", channel: "boss@corp.com", summary: "the numbers"},
		},
	}
	d := &pgxDatabase{conn: conn}

	items, err := d.ListItemsByStatus(context.Background(), "ready")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.baseCalls != 0 {
		t.Fatalf("expected no fallback when all tables present, base ran %d times", conn.baseCalls)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Title != "Q2 report" || items[0].Channel != "boss@corp.com" || items[0].Summary != "the numbers" {
		t.Errorf("expected display populated, got %+v", items[0])
	}
}

// A real (non-42P01) query error must still propagate — degradation is ONLY for missing
// tables, never a blanket error-swallow.
func TestListItemsByStatus_PropagatesNonMissingTableError(t *testing.T) {
	conn := &fakeDisplayConn{
		queryErr: &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"},
	}
	d := &pgxDatabase{conn: conn}

	_, err := d.ListItemsByStatus(context.Background(), "ready")
	if err == nil {
		t.Fatal("expected a non-42P01 error to propagate, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("expected the original 57014 error, got %v", err)
	}
	if conn.baseCalls != 0 {
		t.Fatalf("expected no fallback for a real error, base ran %d times", conn.baseCalls)
	}
}

// A 42P01 that surfaces mid-iteration (from rows.Err(), not Query) must degrade too.
func TestListItemsByStatus_DegradesWhenMissingTableSurfacesFromRowsErr(t *testing.T) {
	conn := &fakeErrRowsConn{
		fakeDisplayConn: fakeDisplayConn{
			baseRows: []displayRow{{id: 3, lane: "linkedin", sourceRef: "u-3", status: "ready"}},
		},
		rowsErr: &pgconn.PgError{Code: "42P01", Message: `relation "linkedin_posts" does not exist`},
	}
	d := &pgxDatabase{conn: conn}

	items, err := d.ListItemsByStatus(context.Background(), "ready")
	if err != nil {
		t.Fatalf("expected degradation on rows.Err() 42P01, got: %v", err)
	}
	if conn.baseCalls != 1 || len(items) != 1 || items[0].ID != 3 {
		t.Fatalf("expected base fallback to return the item, got base=%d items=%+v", conn.baseCalls, items)
	}
}

// fakeErrRowsConn makes the enriched query return rows that fail late (rows.Err()), the
// way pgx surfaces a missing relation during iteration rather than at Query time.
type fakeErrRowsConn struct {
	fakeDisplayConn
	rowsErr error
}

func (c *fakeErrRowsConn) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "LEFT JOIN") {
		c.enrichedCalls++
		return &errRows{err: c.rowsErr}, nil
	}
	c.baseCalls++
	return &fakeRows{rows: c.baseRows}, nil
}

// errRows yields no rows and reports its error from Err(), like pgx on a deferred failure.
type errRows struct {
	pgx.Rows
	err error
}

func (r *errRows) Next() bool   { return false }
func (r *errRows) Close()       {}
func (r *errRows) Err() error   { return r.err }
