// Package config provides module-level functionality for config.
// input: plaintext or enc:aes256: values and a 32-byte AES-256 key
// output: AES-256-GCM encrypt/decrypt helpers sharing the enc:aes256: prefix
// pos: security boundary for protecting sensitive configuration and source-password values
// note: if this file changes, update this header and module README.md.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	crypto_rand "crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// EncryptionPrefix 是加密配置值的前缀。
	EncryptionPrefix = "enc:aes256:"
)

var (
	// ErrInvalidEncryptionFormat 表示加密值格式无效。
	ErrInvalidEncryptionFormat = errors.New("invalid encryption format")
	// ErrEncryptionKeyRequired 表示需要加密密钥但未提供。
	ErrEncryptionKeyRequired = errors.New("encryption key required for encrypted values")
	// ErrInvalidEncryptionKey 表示加密密钥无效。
	ErrInvalidEncryptionKey = errors.New("invalid encryption key")
)

// Decryptor 管理配置值解密。
type Decryptor struct {
	key []byte
}

// NewDecryptor 创建解密器。
// key 必须是 32 字节（AES-256）。
func NewDecryptor(key string) (*Decryptor, error) {
	if key == "" {
		return nil, ErrEncryptionKeyRequired
	}
	keyBytes := []byte(key)
	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("%w: key must be 32 bytes for AES-256, got %d", ErrInvalidEncryptionKey, len(keyBytes))
	}
	return &Decryptor{key: keyBytes}, nil
}

// DecryptIfEncrypted 检查值是否加密，如果是则解密。
// 如果 decryptor 为 nil 且值已加密，返回错误。
func (d *Decryptor) DecryptIfEncrypted(value string) (string, error) {
	if !strings.HasPrefix(value, EncryptionPrefix) {
		return value, nil
	}
	if d == nil {
		return "", ErrEncryptionKeyRequired
	}
	return d.Decrypt(value)
}

// Decrypt 解密 AES-256-GCM 加密的值。
// 格式: enc:aes256:<base64-encoded-ciphertext>
func (d *Decryptor) Decrypt(encrypted string) (string, error) {
	if !strings.HasPrefix(encrypted, EncryptionPrefix) {
		return "", ErrInvalidEncryptionFormat
	}

	encoded := strings.TrimPrefix(encrypted, EncryptionPrefix)
	if encoded == "" {
		return "", ErrInvalidEncryptionFormat
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode failed: %w", err)
	}

	block, err := aes.NewCipher(d.key)
	if err != nil {
		return "", fmt.Errorf("create cipher failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM failed: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short: %d < %d", len(ciphertext), nonceSize)
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt failed: %w", err)
	}

	return string(plaintext), nil
}

// Encrypt 使用 AES-256-GCM 加密值（用于生成加密配置）。
func (d *Decryptor) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return "", fmt.Errorf("create cipher failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM failed: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	// 使用加密安全的随机数生成器生成 nonce
	// AES-GCM 的安全性依赖于 nonce 的唯一性，必须使用 crypto/rand
	if _, err := crypto_rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce failed: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.StdEncoding.EncodeToString(ciphertext)

	return EncryptionPrefix + encoded, nil
}

// HasEncryptedValues 检查配置中是否有加密值。
func HasEncryptedValues(v interface{}) bool {
	switch val := v.(type) {
	case string:
		return strings.HasPrefix(val, EncryptionPrefix)
	case map[string]interface{}:
		for _, v := range val {
			if HasEncryptedValues(v) {
				return true
			}
		}
	case []interface{}:
		for _, v := range val {
			if HasEncryptedValues(v) {
				return true
			}
		}
	}
	return false
}
