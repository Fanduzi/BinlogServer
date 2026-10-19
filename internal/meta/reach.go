// Package meta provides module-level functionality for meta.
// input: the metadata store connection pool and errors returned by its queries
// output: PingMeta (one bounded COM_PING on the pool) and MetaUnreachable, which tells a store that does not answer (timeout, refused or dropped connection, client errors 2002/2003/2006/2013) from an SQL error the store returned (read-only 1290, permission, missing table)
// pos: tasks.MetaProbe for META_UNREACHABLE (ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md §25, #427)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

// PingMeta checks that the metadata store answers; ctx bounds it.
func (s *MySQLTaskStore) PingMeta(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("no metadata store")
	}
	return s.db.PingContext(ctx)
}

// MetaUnreachable reports whether err means the store did not answer.
func (s *MySQLTaskStore) MetaUnreachable(err error) bool { return IsUnreachable(err) }

// IsUnreachable: err is a timeout or a connection-level failure, not an SQL
// error the server returned. A read-only server (1290) or a missing grant
// answered, so it is reachable; those have their own finding kinds.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var me *mysqlDriver.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 2002, 2003, 2006, 2013:
			return true
		}
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysqlDriver.ErrInvalidConn) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
