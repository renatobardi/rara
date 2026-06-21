package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Seed helpers call os.Getenv("RUNNER_LOCAL_URL") to decide whether to enable VPC on_demand
	// providers. Tests that verify VPC-first routing (reconciler, seed, linkedin) need VPC
	// providers enabled, so set a dummy URL here rather than in every individual test.
	os.Setenv("RUNNER_LOCAL_URL", "http://test-runner:8080") //nolint:errcheck
	os.Exit(m.Run())
}
