// input: migration files, META_DSN/env flags, golang-migrate driver dependencies
// output: schema migration commands (up/down/version/force/goto) execution results
// pos: database schema migration CLI for operational change management
// note: if this file changes, update this header and module AGENTS.md.
package main

import "testing"

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
