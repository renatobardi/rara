package main

import (
	"log"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Seed helpers read env vars to configure providers. Set stable test values here so
	// every test in the package sees a consistent, pre-configured seed environment.
	for k, v := range map[string]string{
		"RUNNER_LOCAL_URL": "http://test-runner:8080", // enables VPC on_demand providers
		"DISTILL_MODEL":    "groq-llama",
		"GATE_MODEL":       "groq-fast",
	} {
		if err := os.Setenv(k, v); err != nil {
			log.Fatalf("TestMain: set %s: %v", k, err)
		}
	}
	os.Exit(m.Run())
}
