// Package replication provides module-level functionality for replication.
// input: flavor/log_bin/identity fixtures plus wrapped source-network and stream-read errors
// output: source identity plus permanent/retryable operator-error classification assertions
// pos: unit tests for MariaDB/MySQL source probe semantics
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
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

func TestClassifySourceError_UnreachableNetwork(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ETIMEDOUT, syscall.ECONNREFUSED, syscall.EHOSTUNREACH} {
		err := classifySourceError(&net.OpError{Op: "dial", Net: "tcp", Err: errno})
		var sourceErr *tasks.RetryableSourceError
		if !errors.As(err, &sourceErr) || sourceErr.Code != tasks.CodeSourceUnreachable {
			t.Fatalf("%v: expected SOURCE_UNREACHABLE, got %v", errno, err)
		}
		if tasks.IsPermanent(err) {
			t.Fatalf("%v: unreachable source must remain retryable", errno)
		}
	}

	diskErr := &os.PathError{Op: "write", Path: "/data/binlog", Err: syscall.ENOSPC}
	if got := classifySourceError(diskErr); got != diskErr {
		t.Fatalf("local disk error must not be classified as source unreachable: %v", got)
	}
}

func TestClassifySourceError_StreamDisconnect(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("GetEvent: read packet header: %w", io.EOF),
		fmt.Errorf("GetEvent: %w", fmt.Errorf("read packet body: %w", io.ErrUnexpectedEOF)),
	} {
		classified := classifySourceError(err)
		var sourceErr *tasks.RetryableSourceError
		if !errors.As(classified, &sourceErr) || sourceErr.Code != tasks.CodeSourceUnreachable {
			t.Fatalf("expected SOURCE_UNREACHABLE for wrapped stream disconnect, got %v", classified)
		}
	}
}
