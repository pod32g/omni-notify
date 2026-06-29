package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Cipher encrypts and decrypts provider secrets at rest using AES-256-GCM.
//
// A nil *Cipher is valid and represents "no encryption key configured": it
// cannot encrypt or decrypt, which lets the service run when no secret-bearing
// providers exist while still failing fast when they do.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a raw 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// NewCipherFromBase64 decodes a base64 (standard, with padding) 32-byte key.
// An empty string yields a nil Cipher (encryption disabled).
func NewCipherFromBase64(s string) (*Cipher, error) {
	if s == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key: %w", err)
	}
	return NewCipher(key)
}

// Encrypt seals plaintext, returning nonce||ciphertext. The empty string maps to
// a nil blob so that "no secret" round-trips cleanly.
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	if c == nil {
		return nil, fmt.Errorf("cannot store secret: no encryption key configured (set OMNI_NOTIFY_ENCRYPTION_KEY)")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt opens a nonce||ciphertext blob. A nil/empty blob maps to "".
func (c *Cipher) Decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	if c == nil {
		return "", fmt.Errorf("cannot read secret: no encryption key configured")
	}
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret (wrong key?): %w", err)
	}
	return string(plain), nil
}
