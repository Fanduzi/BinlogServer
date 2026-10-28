// Package main provides module-level functionality for main.
// input: migration files, META_DSN/env flags, a --config meta.tls block, golang-migrate driver dependencies
// output: schema migration commands (up/down/version/force/goto) execution results; proof that --config applies meta.tls and that disabled leaves an existing tls=, that crossing version 11 is detected from the current and target schema, and that only a dirty version 11 is recovered without a manual force
// pos: database schema migration CLI for operational change management
// note: if this file changes, update this header and module README.md.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
)

func TestNormalizeDSN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "raw dsn",
			in:   "user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true",
			want: "mysql://user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true",
		},
		{
			name: "already prefixed",
			in:   "mysql://user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true",
			want: "mysql://user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true",
		},
		{
			name: "trim spaces",
			in:   "  user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true  ",
			want: "mysql://user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeDSN(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeDSN(%q)=%q, want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGuardDestructive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		opts    migrateOptions
		wantErr bool
	}{
		{
			name:    "dev env allows",
			opts:    migrateOptions{env: "dev", allowDown: false},
			wantErr: false,
		},
		{
			name:    "prod env blocks by default",
			opts:    migrateOptions{env: "prod", allowDown: false},
			wantErr: true,
		},
		{
			name:    "production env allows with override",
			opts:    migrateOptions{env: "production", allowDown: true},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := guardDestructive(&tc.opts)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCrossingOwnershipMigration(t *testing.T) {
	t.Parallel()
	if !crossingOwnershipMigration(0, migrate.ErrNilVersion, ^uint(0)) {
		t.Fatal("up from an empty schema must check")
	}
	if !crossingOwnershipMigration(10, nil, 11) {
		t.Fatal("goto 11 from 10 must check")
	}
	if crossingOwnershipMigration(11, nil, ^uint(0)) {
		t.Fatal("up already at 11 must not check")
	}
	if crossingOwnershipMigration(0, migrate.ErrNilVersion, 10) {
		t.Fatal("goto below 11 must not check")
	}
	if crossingOwnershipMigration(11, nil, 12) {
		t.Fatal("a schema already at 11 must not check again")
	}
	if crossingOwnershipMigration(10, errors.New("read failed"), ^uint(0)) {
		t.Fatal("an unreadable version must not block on the precheck")
	}
}

func TestOwnershipDirtyRecovery(t *testing.T) {
	t.Parallel()
	force, ok := ownershipDirtyRecovery(migrate.ErrDirty{Version: 11})
	if !ok || force != 10 {
		t.Fatalf("dirty 11 force=%d ok=%v, want 10", force, ok)
	}
	if _, ok := ownershipDirtyRecovery(migrate.ErrDirty{Version: 10}); ok {
		t.Fatal("a dirty version other than 11 must still be a manual force")
	}
	if _, ok := ownershipDirtyRecovery(migrate.ErrNoChange); ok {
		t.Fatal("a clean up must not force")
	}
}

func TestIsTruthy(t *testing.T) {
	t.Parallel()

	truthy := []string{"1", "true", "TRUE", "yes", "Y", "on"}
	for _, in := range truthy {
		if !isTruthy(in) {
			t.Fatalf("expected true for %q", in)
		}
	}

	falsy := []string{"", "0", "false", "off", "no", "abc"}
	for _, in := range falsy {
		if isTruthy(in) {
			t.Fatalf("expected false for %q", in)
		}
	}
}

func TestEffectiveEnv(t *testing.T) {
	t.Run("prefer MIGRATE_ENV", func(t *testing.T) {
		t.Setenv("MIGRATE_ENV", "prod")
		t.Setenv("ENV", "dev")
		if got := effectiveEnv(); got != "prod" {
			t.Fatalf("effectiveEnv()=%q, want=prod", got)
		}
	})

	t.Run("fallback ENV", func(t *testing.T) {
		t.Setenv("MIGRATE_ENV", "")
		t.Setenv("ENV", "staging")
		if got := effectiveEnv(); got != "staging" {
			t.Fatalf("effectiveEnv()=%q, want=staging", got)
		}
	})

	t.Run("default dev", func(t *testing.T) {
		t.Setenv("MIGRATE_ENV", "")
		t.Setenv("ENV", "")
		if got := effectiveEnv(); got != "dev" {
			t.Fatalf("effectiveEnv()=%q, want=dev", got)
		}
	})
}

func TestResolveMigrateDSN(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	required := write("required.yaml", "meta_dsn: \"user:pass@tcp(127.0.0.1:3306)/meta?parseTime=true\"\nmeta:\n  tls:\n    mode: required\n")
	got, err := resolveMigrateDSN(required, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mysql.ParseDSN(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TLSConfig != "binlog-meta" {
		t.Fatalf("tls=%q, want binlog-meta", parsed.TLSConfig)
	}
	_, err = resolveMigrateDSN(required, "other:pass@tcp(127.0.0.1:3306)/meta?parseTime=true&tls=skip-verify")
	if err == nil || !strings.Contains(err.Error(), "already sets tls=") {
		t.Fatalf("existing tls= error = %v", err)
	}
	disabled := write("disabled.yaml", "meta_dsn: \"file:pass@tcp(127.0.0.1:3306)/meta?parseTime=true\"\nmeta:\n  tls:\n    mode: disabled\n")
	const override = "other:pass@tcp(127.0.0.1:3306)/meta?parseTime=true&tls=skip-verify"
	got, err = resolveMigrateDSN(disabled, override)
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("disabled rewrote the DSN: %s", got)
	}
	_, err = resolveMigrateDSN("", "")
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("missing dsn error = %v", err)
	}
}
