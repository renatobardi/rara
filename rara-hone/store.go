// store.go — the domain types, the narrow persistence seam, and its pgx implementation.
//
// rara-hone is the interest_profile reviser, lifted out of rara-core into its own PERIODIC job
// (a systemd timer on the VPC, run-once-and-exit). It owns the learning loop; the CONTROL plane
// (rara-core) keeps only the human APPROVAL of a proposed version. So this seam is deliberately
// tiny — exactly the four operations ReviseProfile needs over the shared interest_profile /
// feedback tables, no more:
//
//   - GetActiveInterestProfile — the base to revise from (the version in force).
//   - ListInterestProfiles     — next-version numbering + debounce (last revision, pending proposal).
//   - ListFeedbackSince        — the new learning signal accumulated since the last revision.
//   - InsertInterestProfile    — append the revision as a NEW `proposed` version (never active).
//
// hone PROPOSES (append-only, status `proposed`); it NEVER activates. Activation
// (ActivateInterestProfile) stays in rara-core's surface, behind a human. The real seam talks to
// Neon via pgx; tests use an in-memory mock (mock_test.go) mirroring the SQL contract.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Enumerations — kept in sync with the SQL CHECK constraints rara-core owns in
// migrations/001_initial_schema.sql (+ 005/006). hone reads these tables cross-agent (the 1.0
// isolation convention: read a sibling's table through the shared contract, never call it), so it
// carries its own copy of the slices of the enums it touches.
// ---------------------------------------------------------------------------

// interest_profile.status — proposed-vs-active (migration 006). A revision is appended `proposed`;
// an explicit human approval (rara-core's surface) activates it. hone only ever writes `proposed`.
const (
	profileProposed   = "proposed"
	profileActive     = "active"
	profileSuperseded = "superseded"
)

func isValidProfileStatus(s string) bool {
	switch s {
	case profileProposed, profileActive, profileSuperseded:
		return true
	}
	return false
}

// profileStatusOr defaults an empty status to `proposed` — a profile version NEVER becomes active
// implicitly. hone always supplies `proposed` explicitly; this is the belt-and-braces default.
func profileStatusOr(s string) string {
	if s == "" {
		return profileProposed
	}
	return s
}

// feedback.signal — the learning direction of a signal.
const (
	signalUp   = "up"
	signalDown = "down"
)

// feedback.source — provenance of a learning signal (migration 005). The reviser consumes them
// all; quarantine_review tunes keep_threshold, the other two attribute term signal.
const (
	sourceUserExplicit     = "user_explicit"
	sourceQuarantineReview = "quarantine_review"
	sourceKURAImplicit     = "kura_implicit"
)

// feedback.target_type — what a signal is attached to.
const (
	targetItem         = "item"
	targetDistillation = "distillation"
)

// ---------------------------------------------------------------------------
// Domain types — the two control tables hone touches. JSONB columns are carried as
// json.RawMessage so the reviser stays agnostic about their inner shape until it parses them.
// ---------------------------------------------------------------------------

// InterestProfile is one immutable version of the living preferences document. Status is the
// proposed-vs-active lifecycle; Narrative is the LLM-written natural-language summary the gate's
// LLM-judge reads as context (the deterministic engine owns the structured fields, the LLM owns
// only this prose). CreatedAt is read-only (set on insert, populated on reads).
type InterestProfile struct {
	Version    int             `json:"version"`
	Topics     json.RawMessage `json:"topics,omitempty"`
	Authors    json.RawMessage `json:"authors,omitempty"`
	AntiTopics json.RawMessage `json:"anti_topics,omitempty"`
	Weights    json.RawMessage `json:"weights,omitempty"`
	Status     string          `json:"status,omitempty"`
	Narrative  string          `json:"narrative,omitempty"`
	CreatedAt  time.Time       `json:"created_at,omitempty"`
}

// Feedback is one append-only learning signal. CreatedAt is read-only (set by the DB default on
// insert, populated on reads); the reviser windows feedback by it.
type Feedback struct {
	TargetType string    `json:"target_type"`
	TargetRef  string    `json:"target_ref"`
	Signal     string    `json:"signal"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Persistence seam
//
// Database is the only storage seam the reviser talks to — the four operations ReviseProfile
// needs over the shared interest_profile / feedback tables. The real implementation talks to Neon
// via pgx; tests use an in-memory mock mirroring the SQL contract (UNIQUE(version), the
// at-most-one-active partial index, the append-only feedback window).
// ---------------------------------------------------------------------------
type Database interface {
	// GetActiveInterestProfile returns the single `active` version (the base the reviser revises
	// from), found=false if none is active.
	GetActiveInterestProfile(ctx context.Context) (InterestProfile, bool, error)
	// ListInterestProfiles returns every version, ordered by version — the reviser's
	// next-version numbering + debounce/pending-proposal view.
	ListInterestProfiles(ctx context.Context) ([]InterestProfile, error)
	// ListFeedbackSince returns the feedback created strictly after `since`, ordered by id (the
	// new learning signal since the last revision; a zero `since` returns all of it).
	ListFeedbackSince(ctx context.Context, since time.Time) ([]Feedback, error)
	// InsertInterestProfile appends a NEW version. hone only ever writes status `proposed`; the
	// row is inert until a human approves it through rara-core's surface.
	InsertInterestProfile(ctx context.Context, p InterestProfile) error
}

// ---------------------------------------------------------------------------
// Real database: Neon PostgreSQL via pgx
// ---------------------------------------------------------------------------

type pgxStore struct{ conn *pgx.Conn }

func newPgxStore(conn *pgx.Conn) *pgxStore { return &pgxStore{conn: conn} }

var _ Database = (*pgxStore)(nil)

// profileColumns is the shared SELECT list for an interest_profile row.
const profileColumns = `version, topics, authors, anti_topics, weights, status, COALESCE(narrative, ''), created_at`

func scanProfile(row pgx.Row) (InterestProfile, error) {
	var p InterestProfile
	err := row.Scan(&p.Version, &p.Topics, &p.Authors, &p.AntiTopics, &p.Weights, &p.Status, &p.Narrative, &p.CreatedAt)
	return p, err
}

// GetActiveInterestProfile returns the single `active` version (the document in force). The
// partial unique index idx_interest_profile_active guarantees at most one.
func (d *pgxStore) GetActiveInterestProfile(ctx context.Context) (InterestProfile, bool, error) {
	const q = `SELECT ` + profileColumns + ` FROM interest_profile WHERE status = 'active' LIMIT 1`
	p, err := scanProfile(d.conn.QueryRow(ctx, q))
	if errors.Is(err, pgx.ErrNoRows) {
		return InterestProfile{}, false, nil
	}
	if err != nil {
		return InterestProfile{}, false, err
	}
	return p, true, nil
}

// ListInterestProfiles returns every version, ordered by version (the reviser's
// debounce/numbering view).
func (d *pgxStore) ListInterestProfiles(ctx context.Context) ([]InterestProfile, error) {
	const q = `SELECT ` + profileColumns + ` FROM interest_profile ORDER BY version`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InterestProfile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListFeedbackSince returns the feedback rows created after `since`, ordered by id, with
// created_at populated. Windowing at the source keeps the reviser's scan bounded to the new
// signal since the last revision instead of the whole (ever-growing) table.
func (d *pgxStore) ListFeedbackSince(ctx context.Context, since time.Time) ([]Feedback, error) {
	const q = `SELECT target_type, target_ref, signal, source, created_at
	           FROM feedback WHERE created_at > $1 ORDER BY id`
	rows, err := d.conn.Query(ctx, q, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feedback
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.TargetType, &f.TargetRef, &f.Signal, &f.Source, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// InsertInterestProfile appends a new version (append-only — versions are immutable). hone always
// supplies `proposed`; the partial unique index forbids a second `active`, so a programming slip
// that tried to write `active` here would be rejected by the DB anyway.
func (d *pgxStore) InsertInterestProfile(ctx context.Context, p InterestProfile) error {
	status := profileStatusOr(p.Status)
	if !isValidProfileStatus(status) {
		return fmt.Errorf("invalid interest_profile status %q", p.Status)
	}
	const q = `
		INSERT INTO interest_profile (version, topics, authors, anti_topics, weights, status, narrative)
		VALUES ($1, $2::jsonb, $3::jsonb, $4::jsonb, $5::jsonb, $6, $7)`
	_, err := d.conn.Exec(ctx, q, p.Version,
		jsonOrEmpty(p.Topics, "[]"), jsonOrEmpty(p.Authors, "[]"),
		jsonOrEmpty(p.AntiTopics, "[]"), jsonOrEmpty(p.Weights, "{}"),
		status, nullStr(p.Narrative))
	return err
}

func jsonOrEmpty(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	return string(raw)
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
