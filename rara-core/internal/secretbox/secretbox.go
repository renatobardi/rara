package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
)

// Box holds the AES-256-GCM key for symmetric authenticated encryption.
type Box struct {
	key [32]byte
}

// New returns a Box from a 32-byte key. Returns an error if key length != 32.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secretbox: key must be 32 bytes, got %d", len(key))
	}
	b := &Box{}
	copy(b.key[:], key)
	return b, nil
}

// MustLoad reads RARA_SECRETS_KEY (base64-encoded 32-byte key) from env.
// Calls log.Fatalf if absent or invalid — never logs the key value.
func MustLoad() *Box {
	raw := os.Getenv("RARA_SECRETS_KEY")
	if raw == "" {
		log.Fatalf("RARA_SECRETS_KEY environment variable is required")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		log.Fatalf("RARA_SECRETS_KEY: invalid base64")
	}
	b, err := New(key)
	if err != nil {
		log.Fatalf("RARA_SECRETS_KEY: %v", err)
	}
	return b
}

// Encrypt encrypts plaintext with AES-256-GCM using a fresh random nonce.
// Returns ciphertext and nonce separately so both can be stored.
func (b *Box) Encrypt(plaintext []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// Decrypt decrypts ciphertext with the given nonce using AES-256-GCM.
// Returns an error if authentication fails (tampered data or wrong key/nonce).
func (b *Box) Decrypt(ciphertext, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// Last4 returns the last 4 characters of s for masked display ("•••• xxxx").
// Never use this to derive the key.
func Last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
