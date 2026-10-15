package taskrecords

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T, fill byte) *Key {
	t.Helper()
	k, err := ParseKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestStore_RoundTripIsEncryptedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, testKey(t, 1), nil)
	plain := []byte(`{"tasks":[{"id":"1","source":{"password":"s3cr3t-pw"}}]}`)
	if err := s.Save(plain); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %04o, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(s.Path())
	if bytes.Contains(raw, []byte("s3cr3t-pw")) || bytes.Contains(raw, []byte("tasks")) {
		t.Fatalf("plaintext visible in file: %s", raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Format != Format || env.Version != Version || env.KeyID != s.KeyID() {
		t.Fatalf("envelope %+v err %v", env, err)
	}
	got, li, err := s.Load()
	if err != nil || !bytes.Equal(got, plain) || li.Previous {
		t.Fatalf("load %q %+v %v", got, li, err)
	}
	// A second save uses a fresh nonce.
	if err := s.Save(plain); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(s.Path())
	if bytes.Equal(raw, raw2) {
		t.Fatal("two saves produced identical bytes (nonce reused)")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, tempPrefix+"*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestStore_NoFile(t *testing.T) {
	if _, _, err := Open(t.TempDir(), testKey(t, 1), nil).Load(); !errors.Is(err, ErrNoFile) {
		t.Fatalf("err %v, want ErrNoFile", err)
	}
	if _, _, err := Open(t.TempDir(), nil, nil).Load(); !errors.Is(err, ErrNoFile) {
		t.Fatalf("no key, no file: err %v, want ErrNoFile", err)
	}
}

func TestStore_MissingKeyWithFile(t *testing.T) {
	dir := t.TempDir()
	if err := Open(dir, testKey(t, 1), nil).Save([]byte("x")); err != nil {
		t.Fatal(err)
	}
	s := Open(dir, nil, nil)
	if _, _, err := s.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("err %v, want ErrKeyMissing", err)
	}
	if err := s.Save([]byte("y")); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("save without key: %v", err)
	}
}

func TestStore_WrongKeyLeavesFile(t *testing.T) {
	dir := t.TempDir()
	if err := Open(dir, testKey(t, 1), nil).Save([]byte("x")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, FileName))
	_, _, err := Open(dir, testKey(t, 2), nil).Load()
	if !errors.Is(err, ErrWrongKey) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("err %v, want ErrWrongKey", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, FileName))
	if !bytes.Equal(before, after) {
		t.Fatal("Load changed the file")
	}
}

func TestStore_PreviousKeyRotation(t *testing.T) {
	dir := t.TempDir()
	old, cur := testKey(t, 1), testKey(t, 2)
	if err := Open(dir, old, nil).Save([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	s := Open(dir, cur, old)
	got, li, err := s.Load()
	if err != nil || string(got) != "payload" || !li.Previous || li.KeyID != old.ID() {
		t.Fatalf("load %q %+v %v", got, li, err)
	}
	if err := s.Save(got); err != nil {
		t.Fatal(err)
	}
	got, li, err = Open(dir, cur, nil).Load()
	if err != nil || string(got) != "payload" || li.Previous || li.KeyID != cur.ID() {
		t.Fatalf("after rotation %q %+v %v", got, li, err)
	}
	if _, _, err := Open(dir, old, nil).Load(); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("old key after rotation: %v", err)
	}
}

func TestStore_CorruptAndTampered(t *testing.T) {
	cases := map[string]func(env *envelope, raw []byte) []byte{
		"not json":  func(_ *envelope, _ []byte) []byte { return []byte("{garbage") },
		"truncated": func(_ *envelope, raw []byte) []byte { return raw[:len(raw)/2] },
		"flipped ciphertext": func(env *envelope, _ []byte) []byte {
			ct, _ := base64.StdEncoding.DecodeString(env.Ciphertext)
			ct[len(ct)/2] ^= 0xff
			env.Ciphertext = base64.StdEncoding.EncodeToString(ct)
			b, _ := json.Marshal(env)
			return b
		},
		"unknown version": func(env *envelope, _ []byte) []byte {
			env.Version = 2
			b, _ := json.Marshal(env)
			return b
		},
		"other format": func(env *envelope, _ []byte) []byte {
			env.Format = "something-else"
			b, _ := json.Marshal(env)
			return b
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := Open(dir, testKey(t, 1), nil)
			if err := s.Save([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(s.Path())
			var env envelope
			_ = json.Unmarshal(raw, &env)
			bad := mutate(&env, raw)
			if err := os.WriteFile(s.Path(), bad, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Load(); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err %v, want ErrCorrupt", err)
			}
			after, _ := os.ReadFile(s.Path())
			if !bytes.Equal(after, bad) {
				t.Fatal("Load changed a corrupt file")
			}
		})
	}
}

func TestStore_HeaderIsAuthenticated(t *testing.T) {
	// Swapping the key id to the previous key's id must not decrypt.
	dir := t.TempDir()
	old, cur := testKey(t, 1), testKey(t, 2)
	s := Open(dir, cur, old)
	if err := s.Save([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.Path())
	var env envelope
	_ = json.Unmarshal(raw, &env)
	env.KeyID = old.ID()
	b, _ := json.Marshal(env)
	_ = os.WriteFile(s.Path(), b, 0o600)
	if _, _, err := s.Load(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err %v, want ErrCorrupt", err)
	}
}

func TestStore_RemoveStaleTemps(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, tempPrefix+"abc")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, ".health.json")
	_ = os.WriteFile(other, []byte("{}"), 0o600)
	if err := Open(dir, nil, nil).RemoveStaleTemps(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp not removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated file removed")
	}
}

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	forms := []string{
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		hex.EncodeToString(raw),
		"  " + base64.StdEncoding.EncodeToString(raw) + "\n",
	}
	var id string
	for _, f := range forms {
		k, err := ParseKey(f)
		if err != nil {
			t.Fatalf("%q: %v", f, err)
		}
		if id != "" && k.ID() != id {
			t.Fatalf("same key, different id")
		}
		id = k.ID()
	}
	if len(id) != 16 {
		t.Fatalf("id %q", id)
	}
	if _, err := ParseKey(""); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("empty: %v", err)
	}
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))
	if _, err := ParseKey(short); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("16 bytes: %v", err)
	}
	if _, err := ParseKey("not a key!!"); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("garbage: %v", err)
	}
	// The key text never appears in an error.
	_, err := ParseKey(short)
	if strings.Contains(err.Error(), short) {
		t.Fatal("error contains the key text")
	}
}

func TestLoadKey_FileModeAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.key")
	k, err := GenerateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("generated mode %04o", info.Mode().Perm())
	}
	if _, err := GenerateKeyFile(path); err == nil {
		t.Fatal("GenerateKeyFile overwrote an existing file")
	}
	got, err := LoadKey("", path)
	if err != nil || got.ID() != k.ID() {
		t.Fatalf("load %v %v", got, err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey("", path); err != nil {
		t.Fatalf("0400: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey("", path); !errors.Is(err, ErrKeyInvalid) || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("0644: %v", err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatal("LoadKey changed the key file mode")
	}
	env := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	e, err := LoadKey(env, path)
	if err != nil || e.ID() == k.ID() {
		t.Fatalf("env must win: %v", err)
	}
	if _, err := LoadKey("", ""); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("none: %v", err)
	}
	if _, err := LoadKey("", filepath.Join(dir, "absent")); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("absent file: %v", err)
	}
	empty := filepath.Join(dir, "empty.key")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, err := LoadKey("", empty); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("empty file: %v", err)
	}
}
