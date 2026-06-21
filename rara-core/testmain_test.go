package main

import (
	"log"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Seed helpers call os.Getenv("RUNNER_LOCAL_URL") to decide whether to enable VPC on_demand
	// providers. Tests that verify VPC-first routing (reconciler, seed, linkedin) need VPC
	// providers enabled, so set a dummy URL here rather than in every individual test.
	if err := os.Setenv("RUNNER_LOCAL_URL", "http://test-runner:8080"); err != nil {
		log.Fatalf("TestMain: set RUNNER_LOCAL_URL: %v", err)
	}
	os.Exit(m.Run())
}
