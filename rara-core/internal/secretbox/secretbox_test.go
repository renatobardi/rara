package secretbox_test

import (
	"bytes"
	"testing"

	"rara-core/internal/secretbox"
)

var testKey = bytes.Repeat([]byte("k"), 32)

func mustBox(t *testing.T) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := mustBox(t)
	plain := []byte("sk-test-api-key-1234567890")

	ct, nonce, err := b.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := b.Decrypt(ct, nonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plain)
	}
}

func TestNonceUniquePerEncrypt(t *testing.T) {
	b := mustBox(t)
	plain := []byte("same plaintext")

	_, n1, _ := b.Encrypt(plain)
	_, n2, _ := b.Encrypt(plain)

	if bytes.Equal(n1, n2) {
		t.Fatal("Encrypt produced identical nonces on two calls")
	}
}

func TestCiphertextDoesNotContainPlaintext(t *testing.T) {
	b := mustBox(t)
	plain := []byte("supersecretkey99")

	ct, _, err := b.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext contains plaintext bytes")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	b := mustBox(t)
	ct, nonce, _ := b.Encrypt([]byte("sensitive"))

	ct[0] ^= 0xff // flip first byte

	if _, err := b.Decrypt(ct, nonce); err == nil {
		t.Fatal("expected error on tampered ciphertext, got nil")
	}
}

func TestDecryptWrongNonceFails(t *testing.T) {
	b := mustBox(t)
	ct, nonce, _ := b.Encrypt([]byte("sensitive"))

	nonce[0] ^= 0xff

	if _, err := b.Decrypt(ct, nonce); err == nil {
		t.Fatal("expected error with wrong nonce, got nil")
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	for _, bad := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte("x"), 31), bytes.Repeat([]byte("x"), 33)} {
		if _, err := secretbox.New(bad); err == nil {
			t.Fatalf("New(%d bytes): expected error, got nil", len(bad))
		}
	}
}

func TestLast4(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sk-test-1234abcd", "abcd"},
		{"abcd", "abcd"},
		{"ab", "ab"},
		{"", ""},
		{"x", "x"},
	}
	for _, c := range cases {
		got := secretbox.Last4(c.in)
		if got != c.want {
			t.Errorf("Last4(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
