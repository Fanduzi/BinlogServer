// Package replication provides module-level functionality for replication.
// input: flavor/log_bin/identity fixtures and go-mysql style error strings
// output: source identity resolution and permanent-error classification assertions
// pos: unit tests for MariaDB/MySQL source probe semantics
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"strings"
	"testing"

	"binlog_server/internal/tasks"
)

func TestResolveSourceIdentity_MariaDBUsesServerID(t *testing.T) {
	id, err := resolveSourceIdentity("mariadb", "ON", "", "1", "0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "mariadb:1:0" {
		t.Fatalf("unexpected identity %q", id)
	}
}

func TestResolveSourceIdentity_LogBinOffIsPermanent(t *testing.T) {
	_, err := resolveSourceIdentity("mariadb", "OFF", "", "1", "0")
	if !tasks.IsPermanent(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	var pe *tasks.PermanentError
	if !errors.As(err, &pe) || pe.Code != tasks.CodeSourceLogBinOff {
		t.Fatalf("expected SOURCE_LOG_BIN_OFF, got %v", err)
	}
	if !strings.Contains(err.Error(), "log_bin is off") {
		t.Fatalf("expected log_bin off message, got %v", err)
	}
}

func TestResolveSourceIdentity_MySQLRequiresServerUUID(t *testing.T) {
	_, err := resolveSourceIdentity("mysql", "ON", "", "1", "0")
	if !tasks.IsPermanent(err) {
		t.Fatalf("expected permanent empty server_uuid, got %v", err)
	}
	id, err := resolveSourceIdentity("mysql", "1", "abc-uuid", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "abc-uuid" {
		t.Fatalf("unexpected identity %q", id)
	}
}

func TestClassifySourceError_AccessDenied(t *testing.T) {
	err := classifySourceError(errors.New("handleAuthResult: ERROR 1045 (28000): Access denied for user 'repl'@'127.0.0.1'"))
	if !tasks.IsPermanent(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	var pe *tasks.PermanentError
	if !errors.As(err, &pe) || pe.Code != tasks.CodeSourceAccessDenied {
		t.Fatalf("expected SOURCE_ACCESS_DENIED, got %v", err)
	}
}
