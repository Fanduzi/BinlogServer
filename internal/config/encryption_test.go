// Package config provides module-level functionality for config.
// input: AES-256 keys and plaintext/ciphertext fixture values
// output: enc:aes256: encrypt/decrypt round-trip coverage
// pos: configuration boundary translating external settings into internal options
// note: if this file changes, update this header and module README.md.
package config

import (
	"strings"
	"testing"
)

func TestDecryptor_EncryptDecryptRoundTrip(t *testing.T) {
	d, err := NewDecryptor("01234567890123456789012345678901")
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	encrypted, err := d.Encrypt("secret-password")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(encrypted, EncryptionPrefix) {
		t.Fatalf("expected %s prefix, got %q", EncryptionPrefix, encrypted)
	}
	plain, err := d.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "secret-password" {
		t.Fatalf("expected plaintext password, got %q", plain)
	}
	passthrough, err := d.DecryptIfEncrypted("plain")
	if err != nil {
		t.Fatalf("DecryptIfEncrypted plaintext: %v", err)
	}
	if passthrough != "plain" {
		t.Fatalf("expected plaintext passthrough, got %q", passthrough)
	}
}
