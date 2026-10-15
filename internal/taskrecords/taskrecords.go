// Package taskrecords provides module-level functionality for taskrecords.
// input: a 32-byte key (BINLOG_SERVER_TASK_KEY or a 0600 key file), an optional previous key, data_dir, and an opaque plaintext snapshot from the scheduler
// output: {data_dir}/.tasks.enc as a versioned JSON envelope (format, version, key_id, nonce, AES-256-GCM ciphertext with the header in the AAD) written atomically (O_EXCL temp 0600, fsync, rename, directory fsync); Load that tells a missing file, a missing key, a wrong key and a corrupt or unknown-version file apart and decrypts a file on the previous key so the caller re-saves it on the current key; LoadKey and GenerateKeyFile
// pos: ADR 0010 (#385): the standalone task record; the only reader and writer of .tasks.enc
// note: if this file changes, update this header and module README.md.
package taskrecords

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// FileName is the record file under data_dir. The leading dot keeps it
	// out of the on-disk task listing.
	FileName = ".tasks.enc"
	// Format names the envelope.
	Format = "binlog-server/task-records"
	// Version is the only envelope version this binary reads and writes.
	Version = 1
	// KeyEnv holds the current key (base64 or hex of 32 bytes).
	KeyEnv = "BINLOG_SERVER_TASK_KEY"
	// PreviousKeyEnv holds the key a file may still be on during rotation.
	PreviousKeyEnv = "BINLOG_SERVER_TASK_KEY_PREVIOUS"

	tempPrefix = FileName + ".tmp-"
	keyIDLabel = "binlog-server task records v1"
)

var (
	// ErrNoFile: there is no record file yet.
	ErrNoFile = errors.New("task record file does not exist")
	// ErrKeyMissing: no key is configured.
	ErrKeyMissing = errors.New("no task key configured (" + KeyEnv + " or standalone.task_key_file)")
	// ErrKeyInvalid: a key source exists but cannot be used.
	ErrKeyInvalid = errors.New("task key is not usable")
	// ErrWrongKey: the file was written with a key that is neither the key nor the previous key.
	ErrWrongKey = errors.New("task record file was written with a different key")
	// ErrCorrupt: the file is not a readable envelope, or authentication failed.
	ErrCorrupt = errors.New("task record file is corrupt")
)

// Key is a 32-byte AES-256 key and its public id.
type Key struct {
	b  []byte
	id string
}

// ID is the first 8 bytes of SHA-256(label || key), hex. It names the key
// in the file header and in logs; it is not the key.
func (k *Key) ID() string {
	if k == nil {
		return ""
	}
	return k.id
}

func newKey(b []byte) *Key {
	h := sha256.Sum256(append([]byte(keyIDLabel), b...))
	return &Key{b: append([]byte(nil), b...), id: hex.EncodeToString(h[:8])}
}

// ParseKey decodes base64 (std or URL, padded or not) or 64 hex characters
// that decode to exactly 32 bytes.
func ParseKey(text string) (*Key, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrKeyMissing
	}
	if len(text) == 64 {
		if b, err := hex.DecodeString(text); err == nil {
			return newKey(b), nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(text); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("%w: key decodes to %d bytes, want 32", ErrKeyInvalid, len(b))
			}
			return newKey(b), nil
		}
	}
	return nil, fmt.Errorf("%w: key is neither base64 nor 64 hex characters", ErrKeyInvalid)
}

// LoadKey returns the key from env text (wins when set) or from a key file.
// The file must be regular and have no group or other permission bits.
// Neither set is ErrKeyMissing. The key text is never part of an error.
func LoadKey(envText, file string) (*Key, error) {
	if strings.TrimSpace(envText) != "" {
		return ParseKey(envText)
	}
	file = strings.TrimSpace(file)
	if file == "" {
		return nil, ErrKeyMissing
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("%w: key file %s: %v", ErrKeyInvalid, file, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: key file %s is not a regular file", ErrKeyInvalid, file)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: key file %s has mode %04o; it must be 0600 or 0400", ErrKeyInvalid, file, perm)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%w: key file %s: %v", ErrKeyInvalid, file, err)
	}
	k, err := ParseKey(string(raw))
	if err != nil {
		if errors.Is(err, ErrKeyMissing) {
			return nil, fmt.Errorf("%w: key file %s is empty", ErrKeyInvalid, file)
		}
		return nil, fmt.Errorf("key file %s: %w", file, err)
	}
	return k, nil
}

// GenerateKeyFile writes a new random key, base64, to path with mode 0600.
// An existing path is refused.
func GenerateKeyFile(path string) (*Key, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(b) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return newKey(b), nil
}

type envelope struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func aad(format string, version int, keyID string) []byte {
	return []byte(format + "\n" + strconv.Itoa(version) + "\n" + keyID)
}

// Store reads and writes {data_dir}/.tasks.enc.
type Store struct {
	dir  string
	key  *Key
	prev *Key
}

// Open returns the store for dataDir. key may be nil (no key configured):
// Load then reports ErrKeyMissing when a file exists, and Save refuses.
func Open(dataDir string, key, previous *Key) *Store {
	return &Store{dir: dataDir, key: key, prev: previous}
}

// Path is the record file.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

// KeyID is the current key's id ("" without a key).
func (s *Store) KeyID() string { return s.key.ID() }

// HasKey reports a usable current key.
func (s *Store) HasKey() bool { return s != nil && s.key != nil }

// Exists reports that a record file is present.
func (s *Store) Exists() bool {
	_, err := os.Lstat(s.Path())
	return err == nil
}

// LoadInfo describes a successful Load.
type LoadInfo struct {
	// KeyID is the id of the key the file was written with.
	KeyID string
	// Previous: the file was on the previous key; the caller saves it again
	// so it moves to the current key.
	Previous bool
}

// Load returns the plaintext. Errors: ErrNoFile, ErrKeyMissing (file present,
// no key), ErrWrongKey, ErrCorrupt (wrapping the reason). It never changes
// the file.
func (s *Store) Load() ([]byte, LoadInfo, error) {
	raw, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, LoadInfo{}, ErrNoFile
	}
	if err != nil {
		return nil, LoadInfo{}, fmt.Errorf("%w: read %s: %v", ErrCorrupt, s.Path(), err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s is not a task record envelope: %v", ErrCorrupt, s.Path(), err)
	}
	if env.Format != Format {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s has format %q, want %q", ErrCorrupt, s.Path(), env.Format, Format)
	}
	if env.Version != Version {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s has version %d; this binary reads version %d only", ErrCorrupt, s.Path(), env.Version, Version)
	}
	if s.key == nil {
		return nil, LoadInfo{KeyID: env.KeyID}, ErrKeyMissing
	}
	key, previous := s.key, false
	switch env.KeyID {
	case s.key.id:
	case s.prev.ID():
		key, previous = s.prev, true
	default:
		return nil, LoadInfo{KeyID: env.KeyID}, fmt.Errorf("%w: file key id %s, configured key id %s", ErrWrongKey, env.KeyID, s.key.id)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s nonce: %v", ErrCorrupt, s.Path(), err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s ciphertext: %v", ErrCorrupt, s.Path(), err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, LoadInfo{}, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s nonce is %d bytes", ErrCorrupt, s.Path(), len(nonce))
	}
	plain, err := gcm.Open(nil, nonce, ct, aad(env.Format, env.Version, env.KeyID))
	if err != nil {
		return nil, LoadInfo{}, fmt.Errorf("%w: %s failed authentication (the file was changed or damaged)", ErrCorrupt, s.Path())
	}
	return plain, LoadInfo{KeyID: env.KeyID, Previous: previous}, nil
}

// Save encrypts plain with the current key and replaces the file atomically.
func (s *Store) Save(plain []byte) error {
	if s.key == nil {
		return ErrKeyMissing
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	env := envelope{Format: Format, Version: Version, KeyID: s.key.id, Nonce: base64.StdEncoding.EncodeToString(nonce)}
	env.Ciphertext = base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, aad(env.Format, env.Version, env.KeyID)))
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.dir, s.Path(), append(raw, '\n'))
}

// RemoveStaleTemps deletes temp files a crash left behind.
func (s *Store) RemoveStaleTemps() error {
	matches, err := filepath.Glob(filepath.Join(s.dir, tempPrefix+"*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func newGCM(k *Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyInvalid, err)
	}
	return cipher.NewGCM(block)
}

func writeAtomic(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(dir, tempPrefix+hex.EncodeToString(suffix))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
