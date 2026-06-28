package main

import (
	"encoding/base64"
	"testing"
)

// TestLoadSecretbox covers the service-path loader for RARA_SECRETS_KEY. The loader must
// never crash the always-on reconciler: a missing key disables LLM-key writes (nil box,
// no error); a malformed key also disables them but surfaces an error for visibility.
func TestLoadSecretbox(t *testing.T) {
	key32 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	key16 := base64.StdEncoding.EncodeToString(make([]byte, 16))

	// loadSecretbox reads the key via os.Getenv, which returns "" for both an empty and an
	// unset var — so an empty value faithfully exercises the absent-key branch (and t.Setenv
	// restores the prior value automatically, so no manual cleanup is needed).
	cases := []struct {
		name    string
		env     string
		wantBox bool
		wantErr bool
	}{
		{"valid key returns a box", key32, true, false},
		{"absent key returns nil box and no error", "", false, false},
		{"invalid base64 returns an error", "!!!not base64!!!", false, true},
		{"wrong key length returns an error", key16, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RARA_SECRETS_KEY", tc.env)
			box, err := loadSecretbox()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error = %v", err, tc.wantErr)
			}
			if (box != nil) != tc.wantBox {
				t.Fatalf("box != nil = %v, want box = %v", box != nil, tc.wantBox)
			}
		})
	}
}
