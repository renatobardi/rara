package main

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBuildCoreConnConfig_simpleProtocol(t *testing.T) {
	cfg, err := buildCoreConnConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol", cfg.DefaultQueryExecMode)
	}
}

func TestBuildSurfacePoolConfig_simpleProtocol(t *testing.T) {
	cfg, err := buildSurfacePoolConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol", cfg.ConnConfig.DefaultQueryExecMode)
	}
}
