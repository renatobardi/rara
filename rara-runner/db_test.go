package main

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBuildDispatchPoolConfig_simpleProtocol(t *testing.T) {
	cfg, err := buildDispatchPoolConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol", cfg.ConnConfig.DefaultQueryExecMode)
	}
}

// parseProviderEnv is the JSONB-text -> map seam GetProvider uses. Testing it directly keeps the
// suite zero-I/O (no pgx pool, no fake driver dep) while covering the empty/populated/garbage paths.
func TestParseProviderEnv(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty string", raw: "", want: map[string]string{}},
		{name: "empty object", raw: "{}", want: map[string]string{}},
		{name: "populated", raw: `{"DISTILL_RECIPE":"opus","LITELLM_MODEL":"gemini-flash"}`,
			want: map[string]string{"DISTILL_RECIPE": "opus", "LITELLM_MODEL": "gemini-flash"}},
		{name: "garbage", raw: "not json", wantErr: true},
		{name: "non-string value", raw: `{"num":123}`, wantErr: true},
		{name: "oversized", raw: `{"K":"` + strings.Repeat("x", maxProviderEnvBytes) + `"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProviderEnv(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error for %q, got nil", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProviderEnv(%q): %v", tt.raw, err)
			}
			if got == nil {
				t.Fatal("got nil map, want non-nil (no nil-panic downstream)")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
