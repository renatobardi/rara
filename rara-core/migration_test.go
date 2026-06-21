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

// readMigration002 loads the Phase-3 gate_rules migration.
func readMigration002(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/002_gate_rules.sql")
	if err != nil {
		t.Fatalf("read migration 002: %v", err)
	}
	return string(b)
}

// TestMigration002GateRules asserts the Phase-3 rules table exists with the cascade's
// allow/deny + match_type CHECK contract, reuses the namespaced trigger, and stays
// isolated (no foreign domain tables).
func TestMigration002GateRules(t *testing.T) {
	sql := readMigration002(t)
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS gate_rules (") {
		t.Error("missing CREATE TABLE for gate_rules")
	}
	for _, c := range []string{
		"CHECK (action IN ('allow', 'deny'))",
		"CHECK (match_type IN ('channel', 'title_contains'))",
		"UNIQUE (action, match_type, value)",
	} {
		if !strings.Contains(sql, c) {
			t.Errorf("missing constraint %q", c)
		}
	}
	// Must reuse rara-core's namespaced trigger, never a foreign agent's.
	if !strings.Contains(sql, "EXECUTE FUNCTION core_set_updated_at()") {
		t.Error("gate_rules trigger must use the namespaced core_set_updated_at()")
	}
	for _, tbl := range []string{"channel_videos", "transcripts", "distillations"} {
		if strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+tbl) {
			t.Errorf("migration 002 must not create foreign table %q", tbl)
		}
	}
}

// readMigration004 loads the Phase-5 linkedin_posts migration.
func readMigration004(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/004_linkedin_posts.sql")
	if err != nil {
		t.Fatalf("read migration 004: %v", err)
	}
	return string(b)
}

// TestMigration004LinkedInPosts asserts the Phase-5 manual-inbox domain table exists, is keyed
// on the post URL (the spine's source_ref), reuses the namespaced trigger, and does not touch a
// foreign agent's domain tables (rara-core owns this one, as the manual inbox lives in its surface).
func TestMigration004LinkedInPosts(t *testing.T) {
	sql := readMigration004(t)
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS linkedin_posts (") {
		t.Error("missing CREATE TABLE for linkedin_posts")
	}
	if !strings.Contains(sql, "UNIQUE (url)") {
		t.Error("linkedin_posts must be keyed on the post URL (UNIQUE (url))")
	}
	if !strings.Contains(sql, "EXECUTE FUNCTION core_set_updated_at()") {
		t.Error("linkedin_posts trigger must use the namespaced core_set_updated_at()")
	}
	for _, tbl := range []string{"channel_videos", "transcripts", "distillations", "emails", "podcast_episodes"} {
		if strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+tbl) {
			t.Errorf("migration 004 must not create foreign table %q", tbl)
		}
	}
}

// readMigration005 loads the Phase-6 feedback.source CHECK migration.
func readMigration005(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/005_feedback_source_check.sql")
	if err != nil {
		t.Fatalf("read migration 005: %v", err)
	}
	return string(b)
}

// TestMigration005FeedbackSourceCheck asserts the Phase-6 KURA-contract change: feedback.source
// is constrained to the three known sources (admitting kura_implicit), the ADD CONSTRAINT is
// guarded for idempotency, and no foreign domain tables are touched.
func TestMigration005FeedbackSourceCheck(t *testing.T) {
	sql := readMigration005(t)
	if !strings.Contains(sql, "CHECK (source IN ('user_explicit', 'quarantine_review', 'kura_implicit'))") {
		t.Error("missing feedback.source CHECK admitting kura_implicit")
	}
	// Guarded so re-applying is a no-op (no ADD CONSTRAINT IF NOT EXISTS for CHECKs in Postgres).
	if !strings.Contains(sql, "conname = 'feedback_source_check'") {
		t.Error("ADD CONSTRAINT must be guarded on pg_constraint for idempotency")
	}
	// Nothing else: this migration must not create any table.
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("migration 005 should only constrain feedback.source, not create tables")
	}
}

// readMigration006 loads the Phase-6 interest_profile status/narrative migration.
func readMigration006(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/006_interest_profile_status.sql")
	if err != nil {
		t.Fatalf("read migration 006: %v", err)
	}
	return string(b)
}

// TestMigration006InterestProfileStatus asserts the Phase-6 learning-loop schema: the status +
// narrative columns, the status CHECK, the at-most-one-active partial unique index, and the
// defensive demote — all additive/idempotent, no foreign tables.
func TestMigration006InterestProfileStatus(t *testing.T) {
	sql := readMigration006(t)
	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS status",
		"ADD COLUMN IF NOT EXISTS narrative",
		"CHECK (status IN ('proposed', 'active', 'superseded'))",
		"conname = 'interest_profile_status_check'", // guarded ADD CONSTRAINT
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_interest_profile_active",
		"WHERE status = 'active'", // partial: at most one active
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration 006 missing %q", want)
		}
	}
	// The defensive demote keeps only the highest version active before the unique index.
	if !strings.Contains(sql, "SET status = 'superseded'") {
		t.Error("migration 006 should demote all-but-max active before creating the unique index")
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("migration 006 should only alter interest_profile, not create tables")
	}
}

// readMigration007 loads the P1a claim-frontier-index migration.
func readMigration007(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/007_claim_provider_index.sql")
	if err != nil {
		t.Fatalf("read migration 007: %v", err)
	}
	return string(b)
}

// TestMigration007ClaimProviderIndex asserts the P1a frontier index matches the provider-isolated
// claim query: a partial index on (capability, assigned_provider, id) for pending steps, retiring
// the now-redundant capability-only index. Additive/idempotent, no foreign tables.
func TestMigration007ClaimProviderIndex(t *testing.T) {
	sql := readMigration007(t)
	if !strings.Contains(sql, "CREATE INDEX IF NOT EXISTS idx_item_steps_claim_provider") {
		t.Fatal("missing new claim index idx_item_steps_claim_provider")
	}
	idx := strings.Index(sql, "idx_item_steps_claim_provider")
	block := sql[idx:min(idx+260, len(sql))]
	if !strings.Contains(block, "(capability, assigned_provider, id)") {
		t.Errorf("claim index must key on (capability, assigned_provider, id), got: %q", block)
	}
	if !strings.Contains(block, "WHERE status = 'pending'") {
		t.Errorf("claim index must be partial on status = 'pending', got: %q", block)
	}
	// The capability-only frontier index is now redundant and must be retired (idempotently).
	if !strings.Contains(sql, "DROP INDEX IF EXISTS idx_item_steps_claim") {
		t.Error("migration 007 should drop the redundant idx_item_steps_claim")
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("migration 007 should only adjust indexes, not create tables")
	}
}

// readMigration008 loads the P1b provider poke_url migration.
func readMigration008(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/008_provider_poke_url.sql")
	if err != nil {
		t.Fatalf("read migration 008: %v", err)
	}
	return string(b)
}

// TestMigration008ProviderPokeURL asserts the P1b activation-address column: a nullable poke_url
// added to providers, additive/idempotent, touching no foreign tables.
func TestMigration008ProviderPokeURL(t *testing.T) {
	sql := readMigration008(t)
	if !strings.Contains(sql, "ALTER TABLE providers") {
		t.Error("migration 008 must alter the providers table")
	}
	if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS poke_url") {
		t.Error("migration 008 must add poke_url idempotently (ADD COLUMN IF NOT EXISTS)")
	}
	// Nullable by default — existing providers must be unaffected (no NOT NULL / no DEFAULT clause
	// that would force a value on residents that rely on the poll alone).
	colIdx := strings.Index(sql, "ADD COLUMN IF NOT EXISTS poke_url")
	stmt := sql[colIdx:min(colIdx+120, len(sql))]
	if strings.Contains(stmt, "NOT NULL") {
		t.Errorf("poke_url must be nullable, got: %q", stmt)
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("migration 008 should only alter providers, not create tables")
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

// readMigration018 loads the P1a app-column migration.
func readMigration018(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/018_provider_app.sql")
	if err != nil {
		t.Fatalf("read migration 018: %v", err)
	}
	return string(b)
}

// TestMigration018ProviderApp asserts the P1a app-targeting column: a nullable
// VARCHAR(48) added to providers with an idempotent backfill (app = name),
// touching no foreign tables.
func TestMigration018ProviderApp(t *testing.T) {
	sql := readMigration018(t)
	if !strings.Contains(sql, "ALTER TABLE providers") {
		t.Error("migration 018 must alter the providers table")
	}
	if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS app") {
		t.Fatal("migration 018 must add app idempotently (ADD COLUMN IF NOT EXISTS)")
	}
	// Nullable: no NOT NULL on the ADD COLUMN line.
	colIdx := strings.Index(sql, "ADD COLUMN IF NOT EXISTS app")
	if colIdx == -1 {
		t.Fatal("ADD COLUMN IF NOT EXISTS app not found in migration 018")
	}
	stmt := sql[colIdx:min(colIdx+80, len(sql))]
	if strings.Contains(stmt, "NOT NULL") {
		t.Errorf("app must be nullable on add, got: %q", stmt)
	}
	// Idempotent backfill: UPDATE guarded by app IS NULL (new column is always NULL on first run).
	if !strings.Contains(sql, "WHERE app IS NULL") {
		t.Error("migration 018 must have an idempotent backfill guarded on app IS NULL")
	}
	// Backfill seeds app = name (simple equality, no suffix stripping).
	if !strings.Contains(sql, "SET app = name") {
		t.Error("migration 018 backfill must set app = name")
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("migration 018 should only alter providers, not create tables")
	}
}

// readMigration014 loads the E1 provider worker-column migration.
func readMigration014(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/014_provider_worker.sql")
	if err != nil {
		t.Fatalf("read migration 014: %v", err)
	}
	return string(b)
}

// TestMigration014ProviderWorker asserts the E1 worker-grouper column: a nullable worker
// VARCHAR(48) added to providers with an idempotent backfill, touching no foreign tables.
func TestMigration014ProviderWorker(t *testing.T) {
	sql := readMigration014(t)
	if !strings.Contains(sql, "ALTER TABLE providers") {
		t.Error("migration 014 must alter the providers table")
	}
	if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS worker") {
		t.Fatal("migration 014 must add worker idempotently (ADD COLUMN IF NOT EXISTS)")
	}
	// Nullable: no NOT NULL on the ADD COLUMN line (a future cleanup migration can add it
	// once every seeder always stamps worker).
	colIdx := strings.Index(sql, "ADD COLUMN IF NOT EXISTS worker")
	if colIdx == -1 {
		t.Fatal("ADD COLUMN IF NOT EXISTS worker not found in migration 014")
	}
	stmt := sql[colIdx:min(colIdx+80, len(sql))]
	if strings.Contains(stmt, "NOT NULL") {
		t.Errorf("worker must be nullable on add, got: %q", stmt)
	}
	// Idempotent backfill: UPDATE guarded by worker IS NULL (new column is always NULL on first run).
	if !strings.Contains(sql, "WHERE worker IS NULL") {
		t.Error("migration 014 must have an idempotent backfill guarded on worker IS NULL")
	}
	// Backfill strips the -local suffix to derive the logical worker name.
	if !strings.Contains(sql, "regexp_replace") || !strings.Contains(sql, "-local") {
		t.Error("migration 014 backfill must use regexp_replace to strip -local suffix")
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("migration 014 should only alter providers, not create tables")
	}
}
