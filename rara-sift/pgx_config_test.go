package main

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBuildSiftPoolConfig_simpleProtocol(t *testing.T) {
	cfg, err := buildSiftPoolConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol", cfg.ConnConfig.DefaultQueryExecMode)
	}
}
