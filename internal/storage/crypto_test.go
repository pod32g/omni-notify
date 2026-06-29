package storage

import (
	"crypto/rand"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCipherRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	plain := "https://discord.com/api/webhooks/123/abc-secret"
	blob, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("round-trip mismatch: %q != %q", got, plain)
	}
}

func TestCipherEmpty(t *testing.T) {
	c := newTestCipher(t)
	blob, err := c.Encrypt("")
	if err != nil || blob != nil {
		t.Fatalf("empty plaintext should yield nil blob, got %v %v", blob, err)
	}
	got, err := c.Decrypt(nil)
	if err != nil || got != "" {
		t.Fatalf("nil blob should yield empty string, got %q %v", got, err)
	}
}

func TestCipherWrongKeyFails(t *testing.T) {
	c1, c2 := newTestCipher(t), newTestCipher(t)
	blob, _ := c1.Encrypt("secret")
	if _, err := c2.Decrypt(blob); err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestNilCipherCannotEncrypt(t *testing.T) {
	var c *Cipher
	if _, err := c.Encrypt("secret"); err == nil {
		t.Fatal("nil cipher should refuse to encrypt a non-empty secret")
	}
	// but empty is fine
	if blob, err := c.Encrypt(""); err != nil || blob != nil {
		t.Fatalf("nil cipher should allow empty secret, got %v %v", blob, err)
	}
}

func TestNewCipherFromBase64(t *testing.T) {
	if c, err := NewCipherFromBase64(""); err != nil || c != nil {
		t.Fatalf("empty key should yield nil cipher, got %v %v", c, err)
	}
	if _, err := NewCipherFromBase64("not-base64!!!"); err == nil {
		t.Fatal("invalid base64 should error")
	}
	if _, err := NewCipherFromBase64("c2hvcnQ="); err == nil { // "short"
		t.Fatal("wrong-length key should error")
	}
}
