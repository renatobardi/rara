package addon

import (
	"testing"
	"time"
)

func TestEnvDuration(t *testing.T) {
	const key = "ADDON_TEST_DURATION"
	cases := []struct {
		set  bool
		val  string
		want time.Duration
	}{
		{false, "", 5 * time.Second},        // unset -> default
		{true, "", 5 * time.Second},         // empty -> default
		{true, "30s", 30 * time.Second},     // Go duration
		{true, "2m", 2 * time.Minute},       // Go duration
		{true, "45", 45 * time.Second},      // bare integer => seconds
		{true, "0", 5 * time.Second},        // non-positive -> default
		{true, "-3", 5 * time.Second},       // negative -> default
		{true, "nonsense", 5 * time.Second}, // invalid -> default
	}
	for _, c := range cases {
		t.Setenv(key, "") // ensure a clean slate
		if c.set {
			t.Setenv(key, c.val)
		} else {
			// Unset: t.Setenv can't unset, so use a key that is definitely unset.
			if got := EnvDuration("ADDON_TEST_UNSET_KEY", 5*time.Second); got != 5*time.Second {
				t.Errorf("unset key = %v, want default", got)
			}
			continue
		}
		if got := EnvDuration(key, 5*time.Second); got != c.want {
			t.Errorf("EnvDuration(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}
