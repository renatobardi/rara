package addon

import (
	"log"
	"os"
	"strconv"
	"time"
)

// EnvDuration reads a Go duration (e.g. "30s", "2m") or a bare integer (seconds) from env, or
// returns def when unset/invalid. It is the shared helper for the resident-mode knobs every worker
// tunes the same way (notably WORK_POLL_INTERVAL) — the SDK owns that convention, so a worker reads
// it the same regardless of which app it lives in.
func EnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 { // bare integer => seconds
		return time.Duration(n) * time.Second
	}
	log.Printf("addon: ignoring invalid %s=%q", key, v)
	return def
}
