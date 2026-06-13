package main

import (
	"os"
	"strings"
	"testing"
)

// readMigration loads the single Phase-0 migration. It runs with the package dir as
// cwd (go test convention), so the relative path is stable. Zero network I/O — this is
// a structural lint of the SQL, complementing the BEGIN/ROLLBACK validation that
// database-core.yml runs against a real Postgres in CI.
func readMigration(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/001_initial_schema.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	return string(b)
}

// TestMigrationCreatesAllControlTables asserts every rara-core table named in the
// ARCHITECTURE-2.0 data model is created by the migration.
func TestMigrationCreatesAllControlTables(t *testing.T) {
	sql := readMigration(t)
	tables := []string{
		"capabilities", "providers", "flows", "flow_steps", "routing_policies",
		"items", "item_steps", "gate_decisions", "feedback", "interest_profile",
	}
	for _, tbl := range tables {
		if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+tbl+" (") {
			t.Errorf("missing CREATE TABLE for %q", tbl)
		}
	}
}

// TestMigrationTriggerIsNamespaced guards the shared-database convention: the
// updated_at trigger function must be namespaced (core_set_updated_at) and the bare
// set_updated_at() must never appear (it belongs to rara-harvest).
func TestMigrationTriggerIsNamespaced(t *testing.T) {
	sql := readMigration(t)
	if !strings.Contains(sql, "CREATE OR REPLACE FUNCTION core_set_updated_at()") {
		t.Error("expected namespaced trigger function core_set_updated_at()")
	}
	// EXECUTE FUNCTION core_set_updated_at() must be the only set_updated_at flavor used.
	if strings.Contains(sql, "EXECUTE FUNCTION set_updated_at()") {
		t.Error("found bare set_updated_at() — would collide with rara-harvest in the shared DB")
	}
	for _, other := range []string{"shelf_set_updated_at", "scribe_set_updated_at", "distill_set_updated_at", "feed_set_updated_at"} {
		if strings.Contains(sql, other) {
			t.Errorf("found foreign trigger %q — rara-core must own core_set_updated_at only", other)
		}
	}
}

// TestMigrationClaimIndexes asserts the indexes that make the pull/claim work-queue
// efficient exist: a partial index on (capability, id) for pending steps (the
// SELECT ... FOR UPDATE SKIP LOCKED frontier) and a heartbeat sweep index.
func TestMigrationClaimIndexes(t *testing.T) {
	sql := readMigration(t)
	claim := "CREATE INDEX IF NOT EXISTS idx_item_steps_claim"
	if !strings.Contains(sql, claim) {
		t.Fatal("missing claim index idx_item_steps_claim")
	}
	// It must be partial on pending and ordered by (capability, id) for FIFO claiming.
	idx := strings.Index(sql, claim)
	block := sql[idx:min(idx+220, len(sql))]
	if !strings.Contains(block, "(capability, id)") {
		t.Errorf("claim index should key on (capability, id), got: %q", block)
	}
	if !strings.Contains(block, "WHERE status = 'pending'") {
		t.Errorf("claim index should be partial on status = 'pending', got: %q", block)
	}
	if !strings.Contains(sql, "idx_item_steps_heartbeat") {
		t.Error("missing stale-heartbeat sweep index idx_item_steps_heartbeat")
	}
}

// TestMigrationKeyConstraints asserts the uniqueness constraints the persistence seam
// relies on for idempotency are declared in SQL (and so backstop the in-memory mock).
func TestMigrationKeyConstraints(t *testing.T) {
	sql := readMigration(t)
	wantUnique := []string{
		"UNIQUE (name)",             // capabilities / providers / flows
		"UNIQUE (flow_id, seq)",     // flow_steps
		"UNIQUE (scope)",            // routing_policies
		"UNIQUE (lane, source_ref)", // items
		"UNIQUE (item_id, seq)",     // item_steps
		"UNIQUE (version)",          // interest_profile
	}
	for _, u := range wantUnique {
		if !strings.Contains(sql, u) {
			t.Errorf("missing uniqueness constraint %q", u)
		}
	}
	// items carries flow_version (stamped per the architecture).
	if !strings.Contains(sql, "flow_version") {
		t.Error("items must carry flow_version")
	}
	// item_steps carries the mutable output_ref back-link.
	if !strings.Contains(sql, "output_ref") {
		t.Error("item_steps must carry output_ref")
	}
}

// TestMigrationRangeChecks asserts the normalized-range guards on the router
// weights / quality and the gate confidence, and that gate ranking lives in a
// separate integer column (a rank position can exceed score's [0,1] ceiling).
func TestMigrationRangeChecks(t *testing.T) {
	sql := readMigration(t)
	wantChecks := []string{
		"CHECK (quality >= 0 AND quality <= 1)",                // providers.quality
		"CHECK (cost_weight >= 0 AND cost_weight <= 1)",        // routing_policies
		"CHECK (quality_weight >= 0 AND quality_weight <= 1)",  // routing_policies
		"CHECK (score IS NULL OR (score >= 0 AND score <= 1))", // gate_decisions.score
	}
	for _, c := range wantChecks {
		if !strings.Contains(sql, c) {
			t.Errorf("missing range CHECK %q", c)
		}
	}
	if !strings.Contains(sql, "rank       INT") {
		t.Error("gate_decisions must carry a separate integer rank column for gate_rico ordering")
	}
}

// TestMigrationNoCrossAgentTables guards isolation: rara-core's migration must not
// touch another agent's domain tables.
func TestMigrationNoCrossAgentTables(t *testing.T) {
	sql := readMigration(t)
	foreign := []string{
		"channel_videos", "playlist_videos", "transcripts", "transcript_segments",
		"distillations", "news_items", "feed_sources", "target_channels", "playlists",
	}
	for _, tbl := range foreign {
		if strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+tbl) {
			t.Errorf("rara-core migration must not create foreign table %q", tbl)
		}
	}
}
