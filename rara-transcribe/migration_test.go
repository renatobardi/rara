package main

import (
	"os"
	"strings"
	"testing"
)

// TestMigration005WidensSourceType guards the widening of transcripts.source_type from
// youtube|podcast to all five lanes. The extrair lanes (email, linkedin, news) write
// transcripts too, so the original 2-value CHECK made the extract worker fail with
// chk_transcripts_source_type (SQLSTATE 23514). Migration 005 must drop BOTH legacy
// constraints (the inline transcripts_source_type_check from 001 and the named
// chk_transcripts_source_type from 004) and re-add the widened set.
func TestMigration005WidensSourceType(t *testing.T) {
	b, err := os.ReadFile("migrations/005_widen_source_type.sql")
	if err != nil {
		t.Fatalf("read migration 005: %v", err)
	}
	sql := string(b)
	for _, c := range []string{"transcripts_source_type_check", "chk_transcripts_source_type"} {
		if !strings.Contains(sql, "DROP CONSTRAINT IF EXISTS "+c) {
			t.Errorf("migration 005 must drop legacy constraint %q", c)
		}
	}
	if !strings.Contains(sql, "ADD CONSTRAINT chk_transcripts_source_type") {
		t.Error("migration 005 must re-add chk_transcripts_source_type")
	}
	const wantClause = "CHECK (source_type IN ('youtube', 'podcast', 'email', 'linkedin', 'news'))"
	if !strings.Contains(sql, wantClause) {
		t.Errorf("migration 005 CHECK clause missing or incomplete; want %q", wantClause)
	}
}

// TestInitialSchemaSourceTypeWidened keeps the greenfield schema (001) in sync with the
// migrated one: a fresh DB must accept the same five source_types as a migrated DB.
func TestInitialSchemaSourceTypeWidened(t *testing.T) {
	b, err := os.ReadFile("migrations/001_initial_schema.sql")
	if err != nil {
		t.Fatalf("read migration 001: %v", err)
	}
	const wantClause = "CHECK (source_type IN ('youtube', 'podcast', 'email', 'linkedin', 'news'))"
	if !strings.Contains(string(b), wantClause) {
		t.Errorf("001 source_type CHECK must cover all five lanes; want %q", wantClause)
	}
}
