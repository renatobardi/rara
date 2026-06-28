package main

import (
	"encoding/base64"
	"os"
	"testing"
)

// TestLoadSecretbox covers the service-path loader for RARA_SECRETS_KEY. The loader must
// never crash the always-on reconciler: a missing key disables LLM-key writes (nil box,
// no error); a malformed key also disables them but surfaces an error for visibility.
func TestLoadSecretbox(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString(make([]byte, 32))

	t.Run("valid key returns a box", func(t *testing.T) {
		t.Setenv("RARA_SECRETS_KEY", validKey)
		box, err := loadSecretbox()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if box == nil {
			t.Fatal("expected non-nil box for a valid key")
		}
	})

	t.Run("absent key returns nil box and no error", func(t *testing.T) {
		// Truly unset (not just empty) to exercise the absent-key branch faithfully.
		if err := os.Unsetenv("RARA_SECRETS_KEY"); err != nil {
			t.Fatalf("unset RARA_SECRETS_KEY: %v", err)
		}
		box, err := loadSecretbox()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if box != nil {
			t.Fatal("expected nil box when key is absent")
		}
	})

	t.Run("invalid base64 returns nil box and an error", func(t *testing.T) {
		t.Setenv("RARA_SECRETS_KEY", "!!!not base64!!!")
		box, err := loadSecretbox()
		if err == nil {
			t.Fatal("expected an error for invalid base64")
		}
		if box != nil {
			t.Fatal("expected nil box on error")
		}
	})

	t.Run("wrong key length returns nil box and an error", func(t *testing.T) {
		t.Setenv("RARA_SECRETS_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
		box, err := loadSecretbox()
		if err == nil {
			t.Fatal("expected an error for a 16-byte key")
		}
		if box != nil {
			t.Fatal("expected nil box on error")
		}
	})
}
