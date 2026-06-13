package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateOnRune: the gate_rico transcript cap must never split a multi-byte UTF-8 rune
// (pt/en transcripts carry accents), so the result is always valid UTF-8 and at most max
// bytes. Short strings pass through untouched.
func TestTruncateOnRune(t *testing.T) {
	if got := truncateOnRune("hello", 100); got != "hello" {
		t.Errorf("under cap should pass through, got %q", got)
	}
	// "ação" is a-ç-ã-o; ç and ã are 2 bytes each. Cutting at a byte that lands mid-rune must
	// back up to a rune boundary and stay valid UTF-8.
	s := strings.Repeat("ação ", 50) // lots of multi-byte runes
	for max := 1; max < len(s); max++ {
		got := truncateOnRune(s, max)
		if len(got) > max {
			t.Fatalf("max=%d: result %d bytes exceeds cap", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d: result is not valid UTF-8: %q", max, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("max=%d: result is not a prefix of the input", max)
		}
	}
}
