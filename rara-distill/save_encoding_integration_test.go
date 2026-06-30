package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSaveDistillationStructuredEncodingIntegration guards the simple-protocol jsonb bug:
// the pool runs in QueryExecModeSimpleProtocol (PgBouncer/pooler), where pgx encodes a []byte
// param as a bytea hex literal — which the `structured` jsonb column rejects with SQLSTATE 22P02.
// SaveDistillation must therefore pass structured as a string. Opt-in via DISTILL_TEST_DATABASE_URL
// (a throwaway Postgres with the distill migrations applied).
func TestSaveDistillationStructuredEncodingIntegration(t *testing.T) {
	dsn := os.Getenv("DISTILL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DISTILL_TEST_DATABASE_URL to a throwaway Postgres to run the integration test")
	}
	cfg, err := buildDistillPoolConfig(dsn) // production config -> SimpleProtocol
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db := &appDB{pool: pool}

	d := Distillation{
		SourceType: "youtube", SourceRef: "ref-enc", SourceKey: "vid-enc",
		Pattern: "extract_wisdom", Engine: "e/m",
		Structured: []byte(`{"concepts":["a"]}`), StructuredStatus: structOK,
		Status: statusDone, SourceSHA256: "h", RecipeSHA256: "r",
	}

	id, err := db.SaveDistillation(ctx, d)
	if err != nil {
		t.Fatalf("SaveDistillation must persist a jsonb structured under SimpleProtocol, got: %v", err)
	}
	if id == 0 {
		t.Fatal("want a row id")
	}

	var got string
	if err := pool.QueryRow(ctx, `SELECT structured::text FROM distillations WHERE id=$1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "concepts") {
		t.Errorf("structured did not round-trip: %s", got)
	}
}
