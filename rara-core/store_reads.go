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
	"log"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// providerColumns is the shared SELECT list for a providers row (mirrors Provider struct fields).
const providerColumns = `name, capability, runtime, activation,
       constraints, enabled, heartbeat_at, last_collect_at, COALESCE(runner_url, ''), env, COALESCE(worker, ''), last_error`

// maxProviderErrorLen is the rune count cap applied to last_error before it reaches the API.
const maxProviderErrorLen = 500

// truncateErrorMsg caps s to maxProviderErrorLen runes (not bytes) so multi-byte UTF-8
// characters are never split, and JSON marshaling of the result is always valid.
func truncateErrorMsg(s string) string {
	if utf8.RuneCountInString(s) <= maxProviderErrorLen {
		return s
	}
	n := 0
	for i := range s {
		if n == maxProviderErrorLen {
			return s[:i]
		}
		n++
	}
	return s
}

// scanProvider scans a single providers row using the caller's Scan function (works for both
// pgx.Row.Scan and pgx.Rows.Scan — both accept ...any and return error).
func scanProvider(scan func(dest ...any) error) (Provider, error) {
	var p Provider
	err := scan(&p.Name, &p.Capability, &p.Runtime, &p.Activation,
		&p.Constraints, &p.Enabled, &p.HeartbeatAt, &p.LastCollectAt, &p.RunnerURL, &p.Env, &p.Worker, &p.LastError)
	if err == nil && p.LastError != nil {
		truncated := truncateErrorMsg(*p.LastError)
		p.LastError = &truncated
	}
	return p, err
}

func (d *pgxDatabase) ListProvidersForCapability(ctx context.Context, capability string) ([]Provider, error) {
	// idx_providers_capability (partial, enabled=true) backs this lookup.
	const q = `SELECT ` + providerColumns + ` FROM providers WHERE capability = $1 AND enabled = true ORDER BY name`
	rows, err := d.conn.Query(ctx, q, capability)
	if err != nil {
		return nil, fmt.Errorf("list providers for capability %q: %w", capability, err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list providers for capability %q: scan: %w", capability, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) GetProvider(ctx context.Context, name string) (Provider, bool, error) {
	const q = `SELECT ` + providerColumns + ` FROM providers WHERE name = $1`
	p, err := scanProvider(d.conn.QueryRow(ctx, q, name).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, false, nil
	}
	if err != nil {
		return Provider{}, false, fmt.Errorf("get provider %q: %w", name, err)
	}
	return p, true, nil
}

func (d *pgxDatabase) GetRoutingPolicy(ctx context.Context, scope string) (RoutingPolicy, bool, error) {
	const q = `SELECT scope, fallback FROM routing_policies WHERE scope = $1`
	var p RoutingPolicy
	err := d.conn.QueryRow(ctx, q, scope).Scan(&p.Scope, &p.Fallback)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoutingPolicy{}, false, nil
	}
	if err != nil {
		return RoutingPolicy{}, false, fmt.Errorf("get routing policy %q: %w", scope, err)
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

// profileColumns is the shared SELECT list for an interest_profile row.
const profileColumns = `version, topics, authors, anti_topics, weights, status, COALESCE(narrative, ''), created_at`

func scanProfile(row pgx.Row) (InterestProfile, error) {
	var p InterestProfile
	err := row.Scan(&p.Version, &p.Topics, &p.Authors, &p.AntiTopics, &p.Weights, &p.Status, &p.Narrative, &p.CreatedAt)
	return p, err
}

// GetLatestInterestProfile returns the highest-version profile row (regardless of status). Used
// only for next-version numbering by the reviser; the gate path reads GetActiveInterestProfile.
func (d *pgxDatabase) GetLatestInterestProfile(ctx context.Context) (InterestProfile, bool, error) {
	const q = `SELECT ` + profileColumns + ` FROM interest_profile ORDER BY version DESC LIMIT 1`
	p, err := scanProfile(d.conn.QueryRow(ctx, q))
	if errors.Is(err, pgx.ErrNoRows) {
		return InterestProfile{}, false, nil
	}
	if err != nil {
		return InterestProfile{}, false, err
	}
	return p, true, nil
}

// GetActiveInterestProfile returns the single `active` version (the document in force). The
// partial unique index idx_interest_profile_active guarantees at most one.
func (d *pgxDatabase) GetActiveInterestProfile(ctx context.Context) (InterestProfile, bool, error) {
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

// ListInterestProfiles returns every version, ordered by version (config-as-data + the reviser's
// debounce/numbering view).
func (d *pgxDatabase) ListInterestProfiles(ctx context.Context) ([]InterestProfile, error) {
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

// ActivateInterestProfile activates a proposed version, atomically demoting the current active to
// `superseded` first (the partial unique index forbids two actives, so the order matters). If the
// target is not a proposed version the whole transaction rolls back and errProfileNotProposed is
// returned — nothing is demoted.
func (d *pgxDatabase) ActivateInterestProfile(ctx context.Context, version int) error {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	if _, err := tx.Exec(ctx, `UPDATE interest_profile SET status = 'superseded' WHERE status = 'active'`); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `UPDATE interest_profile SET status = 'active' WHERE version = $1 AND status = 'proposed'`, version)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errProfileNotProposed // rolls back the demote above
	}
	return tx.Commit(ctx)
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

// itemDisplayJoins is the shared SELECT list + LEFT JOINs for the two surface reads that
// return items with display metadata (ListItemsByStatus and ListQuarantinedItems). Each lane
// joins its own source table; the CASE picks the right column per lane so a single pass
// covers all lanes without N+1. Summary is truncated to 280 chars server-side.
//
// Cross-agent reads (read-only SELECTs, no FKs across agent boundaries):
//
//	podcast  → podcast_episodes (rara-dial), podcast_feeds (rara-dial)
//	youtube  → channel_videos (rara-harvest), target_channels (rara-harvest)
//	           playlist_videos (rara-shelf), playlists (rara-shelf) — fallback for playlist-only videos
//	email    → emails (rara-courier)
//	linkedin → linkedin_posts (rara-core)
//
// playlist_videos dedup: a video may appear in N playlists (UNIQUE per playlist_id,youtube_video_id).
// The LATERAL LIMIT 1 ensures at most one playlist row per item, preventing row multiplication.
const itemDisplaySelect = `
	SELECT i.id, i.lane, i.source_ref, i.flow_id, i.flow_version, i.status, i.sensitivity,
	  COALESCE(CASE i.lane
	    WHEN 'podcast'  THEN pe.title
	    WHEN 'youtube'  THEN COALESCE(NULLIF(cv.title,''), NULLIF(pv.title,''))
	    WHEN 'email'    THEN em.subject
	    WHEN 'news'     THEN ni.title
	    WHEN 'linkedin' THEN LEFT(lp.body, 100)
	    ELSE '' END, '') AS display_title,
	  COALESCE(CASE i.lane
	    WHEN 'podcast'  THEN pf.title
	    WHEN 'youtube'  THEN COALESCE(tc.channel_name, pl.title)
	    WHEN 'email'    THEN em.sender
	    WHEN 'news'     THEN ni.source
	    ELSE '' END, '') AS display_channel,
	  COALESCE(CASE i.lane
	    WHEN 'podcast'  THEN LEFT(pe.description, 280)
	    WHEN 'email'    THEN LEFT(em.body, 280)
	    WHEN 'news'     THEN LEFT(ni.excerpt, 280)
	    WHEN 'linkedin' THEN LEFT(lp.body, 280)
	    ELSE '' END, '') AS display_summary,
	  CASE i.lane
	    WHEN 'podcast' THEN pe.published_at
	    WHEN 'youtube' THEN COALESCE(cv.published_at, pv.published_at)
	    WHEN 'email'   THEN em.received_at
	    WHEN 'news'    THEN ni.published_at
	    ELSE NULL END AS published_at
	FROM items i
	LEFT JOIN podcast_episodes pe ON i.lane = 'podcast' AND pe.guid             = i.source_ref
	LEFT JOIN podcast_feeds    pf ON i.lane = 'podcast' AND pf.id               = pe.feed_id
	LEFT JOIN channel_videos   cv ON i.lane = 'youtube' AND cv.youtube_video_id = i.source_ref
	LEFT JOIN target_channels  tc ON i.lane = 'youtube' AND tc.id               = cv.channel_id
	LEFT JOIN LATERAL (
	    SELECT title, playlist_id, published_at
	    FROM playlist_videos
	    WHERE youtube_video_id = i.source_ref
	    ORDER BY playlist_id
	    LIMIT 1
	) pv ON i.lane = 'youtube'
	LEFT JOIN playlists        pl ON pl.id = pv.playlist_id
	LEFT JOIN emails           em ON i.lane = 'email'    AND em.message_id      = i.source_ref
	LEFT JOIN news_items       ni ON i.lane = 'news'     AND ni.url             = i.source_ref
	LEFT JOIN linkedin_posts   lp ON i.lane = 'linkedin' AND lp.url             = i.source_ref`

// itemBaseSelect is the DEGRADED projection used when a lane's domain table is absent
// (a non-deployed lane never created its table). It returns the same column shape as
// itemDisplaySelect — id..sensitivity plus the four display columns — but with the
// display columns empty and no JOINs, so it can never hit a missing relation. The empty
// display values serialize away via `omitempty`, so the JSON shape is unchanged. The
// column count/order matches itemDisplaySelect so scanItemWithDisplay scans both.
const itemBaseSelect = `
	SELECT i.id, i.lane, i.source_ref, i.flow_id, i.flow_version, i.status, i.sensitivity,
	  '' AS display_title, '' AS display_channel, '' AS display_summary,
	  NULL::timestamptz AS published_at
	FROM items i`

// scanItemWithDisplay scans one row from a query that uses itemDisplaySelect (or its
// degraded twin itemBaseSelect — same column shape).
func scanItemWithDisplay(rows interface {
	Scan(...any) error
}) (Item, error) {
	var it Item
	err := rows.Scan(
		&it.ID, &it.Lane, &it.SourceRef, &it.FlowID, &it.FlowVersion, &it.Status, &it.Sensitivity,
		&it.Title, &it.Channel, &it.Summary, &it.PublishedAt,
	)
	return it, err
}

// scanItemsWithDisplay runs q (an itemDisplaySelect/itemBaseSelect query) and collects the
// rows. It is the inner pass shared by the resilient read below — separated so the read
// can re-run it with the degraded query on a missing-table error.
func scanItemsWithDisplay(ctx context.Context, conn pgConn, q string, args ...any) ([]Item, error) {
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItemWithDisplay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// listItemsWithDisplay is the resilient core of both surface item reads. It runs the
// enriched read (itemDisplaySelect + the caller's WHERE/ORDER clause); if a lane's domain
// table is absent — Postgres 42P01 (undefined_table), raised because a non-deployed lane
// never created its table — it degrades to itemBaseSelect, returning the items WITHOUT the
// display fields instead of failing the whole endpoint for every lane. Only 42P01 degrades;
// any other error propagates. The missing relation is named in a warning for observability.
//
// A 42P01 can surface either when Query executes the statement or later from rows.Err()
// during iteration; scanItemsWithDisplay funnels both into the returned error, so a single
// errors.As here catches both.
func (d *pgxDatabase) listItemsWithDisplay(ctx context.Context, whereOrder string, args ...any) ([]Item, error) {
	out, err := scanItemsWithDisplay(ctx, d.conn, itemDisplaySelect+whereOrder, args...)
	if err == nil {
		return out, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		// table_name is not populated for undefined_table; the relation name lives in Message
		// (e.g. `relation "emails" does not exist`), so log Message to name the absent table.
		log.Printf("warning: item read degrading to base projection — lane domain table absent: %s", pgErr.Message)
		return scanItemsWithDisplay(ctx, d.conn, itemBaseSelect+whereOrder, args...)
	}
	return nil, err
}

// ListQuarantinedItems returns the deferred (quarantine) items — the cold-start review
// sample. idx_items_status backs the scan.
func (d *pgxDatabase) ListQuarantinedItems(ctx context.Context) ([]Item, error) {
	return d.listItemsWithDisplay(ctx, ` WHERE i.status = 'quarantine' ORDER BY i.id`)
}

// --- Surface reads (Phase 5) -------------------------------------------------
// Pure reads backing the HTTP core + MCP adapter: state observation and config-as-data.

// ListItemsByStatus returns the items in a given status, ordered by id. idx_items_status
// backs the scan. The status is validated by the caller (the surface).
func (d *pgxDatabase) ListItemsByStatus(ctx context.Context, status string) ([]Item, error) {
	return d.listItemsWithDisplay(ctx, ` WHERE i.status = $1 ORDER BY i.id`, status)
}

// ListGateDecisions returns the full curation audit trail for an item, oldest first
// (ascending id). idx_gate_decisions_item backs the scan.
func (d *pgxDatabase) ListGateDecisions(ctx context.Context, itemID int) ([]GateDecision, error) {
	const q = `
		SELECT item_id, gate, decision, score, rank, decided_by, COALESCE(reason, '')
		FROM gate_decisions WHERE item_id = $1 ORDER BY id`
	rows, err := d.conn.Query(ctx, q, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateDecision
	for rows.Next() {
		var dec GateDecision
		if err := rows.Scan(&dec.ItemID, &dec.Gate, &dec.Decision, &dec.Score, &dec.Rank, &dec.DecidedBy, &dec.Reason); err != nil {
			return nil, err
		}
		out = append(out, dec)
	}
	return out, rows.Err()
}

// ListRecentDecisions returns the most recent gate_decisions rows (newest first), capped at
// limit. Callers pass 0 for the default (50). Cap keeps the response bounded regardless of
// the limit param.
func (d *pgxDatabase) ListRecentDecisions(ctx context.Context, limit int) ([]RecentDecision, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	const q = `
		SELECT id, item_id, gate, decision, score, created_at
		FROM gate_decisions
		ORDER BY id DESC
		LIMIT $1`
	rows, err := d.conn.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentDecision
	for rows.Next() {
		var dec RecentDecision
		var when time.Time
		if err := rows.Scan(&dec.ID, &dec.ItemID, &dec.Gate, &dec.Decision, &dec.Score, &when); err != nil {
			return nil, err
		}
		dec.When = when.UTC().Format(time.RFC3339)
		out = append(out, dec)
	}
	return out, rows.Err()
}

// ListFlows returns every flow, ordered by id.
func (d *pgxDatabase) ListFlows(ctx context.Context) ([]Flow, error) {
	const q = `SELECT id, name, source_type, enabled, version FROM flows ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flow
	for rows.Next() {
		var f Flow
		if err := rows.Scan(&f.ID, &f.Name, &f.SourceType, &f.Enabled, &f.Version); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListPodcastFeeds returns every podcast feed (active or not), ordered by id — the operator's
// config-as-data view of rara-dial's podcast_feeds table (see the Database interface note).
func (d *pgxDatabase) ListPodcastFeeds(ctx context.Context) ([]PodcastFeed, error) {
	const q = `SELECT id, feed_url, COALESCE(title, ''), active FROM podcast_feeds ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PodcastFeed
	for rows.Next() {
		var f PodcastFeed
		if err := rows.Scan(&f.ID, &f.FeedURL, &f.Title, &f.Active); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListProviders returns every provider (enabled or not), ordered by name.
func (d *pgxDatabase) ListProviders(ctx context.Context) ([]Provider, error) {
	const q = `SELECT ` + providerColumns + ` FROM providers ORDER BY name`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list providers: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListRoutingPolicies returns every routing policy, ordered by scope.
func (d *pgxDatabase) ListRoutingPolicies(ctx context.Context) ([]RoutingPolicy, error) {
	const q = `SELECT scope, fallback FROM routing_policies ORDER BY scope`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutingPolicy
	for rows.Next() {
		var p RoutingPolicy
		if err := rows.Scan(&p.Scope, &p.Fallback); err != nil {
			return nil, fmt.Errorf("scan routing policy: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListAllGateRules returns every gate rule (enabled or not), ordered (action, match_type,
// value) — the config-as-data view (ListGateRules is the cascade's enabled-only read).
func (d *pgxDatabase) ListAllGateRules(ctx context.Context) ([]GateRule, error) {
	const q = `
		SELECT action, match_type, value, enabled
		FROM gate_rules ORDER BY action, match_type, value`
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

// ListRecentDistillations returns up to limit distillations (light projection, no content)
// from rara-distill's table, ordered newest-first. Cross-agent read: this SELECT is safe
// because rara-core never writes to distillations.
func (d *pgxDatabase) ListRecentDistillations(ctx context.Context, limit int) ([]DistillationSummary, error) {
	const q = `
		SELECT id, source_type, source_ref, title, doc_context, engine, status, created_at
		FROM distillations
		ORDER BY created_at DESC, id DESC
		LIMIT $1`
	rows, err := d.conn.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DistillationSummary, 0)
	for rows.Next() {
		var s DistillationSummary
		if err := rows.Scan(&s.ID, &s.SourceType, &s.SourceRef, &s.Title, &s.DocContext, &s.Engine, &s.Status, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetDistillation returns the full distillation (with content + structured data) by id.
func (d *pgxDatabase) GetDistillation(ctx context.Context, id int) (Distillation, bool, error) {
	const q = `
		SELECT id, source_type, source_ref, title, doc_context, engine, status, created_at,
		       source_key, pattern, context, strategy, session_patterns,
		       content, structured, structured_status, updated_at
		FROM distillations
		WHERE id = $1`
	var dist Distillation
	err := d.conn.QueryRow(ctx, q, id).Scan(
		&dist.ID, &dist.SourceType, &dist.SourceRef, &dist.Title, &dist.DocContext,
		&dist.Engine, &dist.Status, &dist.CreatedAt,
		&dist.SourceKey, &dist.Pattern, &dist.Context, &dist.Strategy, &dist.SessionPatterns,
		&dist.Content, &dist.Structured, &dist.StructuredStatus, &dist.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Distillation{}, false, nil
	}
	if err != nil {
		return Distillation{}, false, err
	}
	return dist, true, nil
}

// HealthPing verifies database connectivity with a lightweight SELECT 1.
func (d *pgxDatabase) HealthPing(ctx context.Context) error {
	var n int
	if err := d.conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		return fmt.Errorf("health ping: %w", err)
	}
	return nil
}

// UsageCounts returns exact COUNT(*) GROUP BY aggregates. The distillations cross-agent read
// degrades gracefully on 42P01 (table not deployed) — items/item_steps are owned by core,
// so their queries never degrade. Distillations = 0 when the table is absent.
func (d *pgxDatabase) UsageCounts(ctx context.Context) (UsageReport, error) {
	var r UsageReport
	var err error

	if r.Items, err = d.queryItemCounts(ctx); err != nil {
		return r, err
	}
	for _, ic := range r.Items {
		if ic.Status == itemQuarantine {
			r.Quarantine += ic.Count
		}
	}

	if r.ItemSteps, err = d.queryStepCounts(ctx); err != nil {
		return r, err
	}

	// distillations total — cross-agent table; degrade on 42P01 (table absent)
	var count int
	if err := d.conn.QueryRow(ctx, "SELECT COUNT(*) FROM distillations").Scan(&count); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			log.Printf("warning: distillations table absent, usage count degraded")
		} else {
			return r, fmt.Errorf("usage distillations: %w", err)
		}
	} else {
		r.Distillations = count
	}

	return r, nil
}

func (d *pgxDatabase) queryItemCounts(ctx context.Context) ([]ItemCount, error) {
	const q = `SELECT lane, status, COUNT(*) FROM items GROUP BY lane, status ORDER BY lane, status`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("usage items query: %w", err)
	}
	out := make([]ItemCount, 0)
	for rows.Next() {
		var ic ItemCount
		if err := rows.Scan(&ic.Lane, &ic.Status, &ic.Count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("usage items scan: %w", err)
		}
		out = append(out, ic)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage items rows: %w", err)
	}
	return out, nil
}

func (d *pgxDatabase) queryStepCounts(ctx context.Context) ([]StepCount, error) {
	const q = `SELECT capability, status, COUNT(*) FROM item_steps GROUP BY capability, status ORDER BY capability, status`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("usage item_steps query: %w", err)
	}
	out := make([]StepCount, 0)
	for rows.Next() {
		var sc StepCount
		if err := rows.Scan(&sc.Capability, &sc.Status, &sc.Count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("usage item_steps scan: %w", err)
		}
		out = append(out, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage item_steps rows: %w", err)
	}
	return out, nil
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

// ListAssignedSteps returns item_steps with status='pending' AND assigned_provider IS NOT NULL —
// the set the dispatcher observes to decide which providers need waking. Ordered by id (FIFO).
func (d *pgxDatabase) ListAssignedSteps(ctx context.Context) ([]ItemStep, error) {
	const q = `
		SELECT item_id, seq, capability, status,
		       COALESCE(assigned_provider, ''), attempt,
		       heartbeat_at, COALESCE(output_ref, ''), COALESCE(error, '')
		FROM item_steps
		WHERE status = 'pending' AND assigned_provider IS NOT NULL
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
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

// workerMetricAcc accumulates one (provider, status, n, lastAt, avgAttempt) observation
// into byProvider. Shared by pgxDatabase and MockDatabase to avoid duplication.
func workerMetricAcc(byProvider map[string]*WorkerMetric, order *[]string,
	provider, status string, n int, lastAt *time.Time, avgAttempt float64) {
	m, ok := byProvider[provider]
	if !ok {
		m = &WorkerMetric{Provider: provider, ByStatus: map[string]int{}}
		byProvider[provider] = m
		*order = append(*order, provider)
	}
	m.ByStatus[status] += n
	m.Total += n
	if lastAt != nil && (m.LastActivityAt == nil || lastAt.After(*m.LastActivityAt)) {
		m.LastActivityAt = lastAt
	}
	m.AvgAttempt += avgAttempt * float64(n) // weighted sum; divided by Total at finalize
}

// workerMetricFinalize resolves Done/Failed/Queue/SuccessRate/AvgAttempt after all
// rows have been accumulated. Shared by pgxDatabase and MockDatabase.
func workerMetricFinalize(m *WorkerMetric) {
	m.Done = m.ByStatus[stepDone]
	m.Failed = m.ByStatus[stepFailed]
	m.Queue = m.ByStatus[stepPending] + m.ByStatus[stepAssigned] + m.ByStatus[stepRunning]
	if terminal := m.Done + m.Failed; terminal > 0 {
		m.SuccessRate = float64(m.Done) / float64(terminal)
	}
	if m.Total > 0 {
		m.AvgAttempt /= float64(m.Total)
	}
}

// WorkerMetrics returns a per-provider rollup of item_steps for the Workers screen
// metric cards. Only rows with a non-NULL assigned_provider are counted; when since
// is non-nil only rows updated at or after that timestamp are included.
func (d *pgxDatabase) WorkerMetrics(ctx context.Context, since *time.Time) ([]WorkerMetric, error) {
	var (
		q    string
		args []any
	)
	const base = `
		SELECT assigned_provider,
		       status,
		       COUNT(*)        AS n,
		       MAX(updated_at) AS last_at,
		       AVG(attempt)    AS avg_attempt
		FROM item_steps
		WHERE assigned_provider IS NOT NULL`
	if since != nil {
		q = base + ` AND updated_at >= $1 GROUP BY assigned_provider, status ORDER BY assigned_provider, status`
		args = []any{since}
	} else {
		q = base + ` GROUP BY assigned_provider, status ORDER BY assigned_provider, status`
	}

	rows, err := d.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("worker metrics query: %w", err)
	}
	defer rows.Close()

	byProvider := map[string]*WorkerMetric{}
	var order []string
	for rows.Next() {
		var provider, status string
		var n int
		var lastAt *time.Time
		var avgAttempt float64
		if err := rows.Scan(&provider, &status, &n, &lastAt, &avgAttempt); err != nil {
			return nil, fmt.Errorf("worker metrics scan: %w", err)
		}
		workerMetricAcc(byProvider, &order, provider, status, n, lastAt, avgAttempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker metrics rows: %w", err)
	}

	out := make([]WorkerMetric, 0, len(order))
	for _, p := range order {
		m := byProvider[p]
		workerMetricFinalize(m)
		out = append(out, *m)
	}
	return out, nil
}
