package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCipherRoundTrip(t *testing.T) {
	kp := filepath.Join(t.TempDir(), "secret.key")
	c, err := NewCipher(kp)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("hunter2"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != "hunter2" {
		t.Errorf("got %q, want hunter2", got)
	}
}

func TestKeyFileGeneratedWith0600(t *testing.T) {
	kp := filepath.Join(t.TempDir(), "secret.key")
	if _, err := NewCipher(kp); err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	fi, err := os.Stat(kp)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perm = %v, want 0600", fi.Mode().Perm())
	}
	if fi.Size() != 32 {
		t.Errorf("key size = %d, want 32", fi.Size())
	}
}

func TestKeyFilePersistsAcrossOpens(t *testing.T) {
	kp := filepath.Join(t.TempDir(), "secret.key")
	c1, _ := NewCipher(kp)
	blob, _ := c1.Encrypt([]byte("x"))
	c2, err := NewCipher(kp) // same key file
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := c2.Decrypt(blob)
	if err != nil || string(got) != "x" {
		t.Fatalf("cross-open decrypt failed: %v %q", err, got)
	}
}

func TestNonceRandomized(t *testing.T) {
	c, _ := NewCipher(filepath.Join(t.TempDir(), "k"))
	a, _ := c.Encrypt([]byte("same"))
	b, _ := c.Encrypt([]byte("same"))
	if bytes.Equal(a, b) {
		t.Error("expected different ciphertexts (random nonce)")
	}
}

func TestDecryptRejectsTampered(t *testing.T) {
	c, _ := NewCipher(filepath.Join(t.TempDir(), "k"))
	blob, _ := c.Encrypt([]byte("data"))
	blob[len(blob)-1] ^= 0xff // flip a byte
	if _, err := c.Decrypt(blob); err == nil {
		t.Error("expected error decrypting tampered blob")
	}
	if _, err := c.Decrypt([]byte("short")); err == nil {
		t.Error("expected error decrypting too-short blob")
	}
}
