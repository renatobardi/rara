package addon

import (
	"testing"
	"time"
)

func TestEnvDuration(t *testing.T) {
	const key = "ADDON_TEST_DURATION"
	// Unset is the same code path as empty (os.Getenv returns "" for both); cover it once here.
	if got := EnvDuration("ADDON_TEST_UNSET_KEY", 5*time.Second); got != 5*time.Second {
		t.Errorf("unset key = %v, want default", got)
	}
	cases := map[string]time.Duration{
		"":         5 * time.Second,  // empty -> default
		"30s":      30 * time.Second, // Go duration
		"2m":       2 * time.Minute,  // Go duration
		"45":       45 * time.Second, // bare integer => seconds
		"0":        5 * time.Second,  // non-positive -> default
		"-3":       5 * time.Second,  // negative -> default
		"nonsense": 5 * time.Second,  // invalid -> default
	}
	for val, want := range cases {
		t.Setenv(key, val)
		if got := EnvDuration(key, 5*time.Second); got != want {
			t.Errorf("EnvDuration(%q) = %v, want %v", val, got, want)
		}
	}
}
