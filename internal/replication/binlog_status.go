// Package replication provides module-level functionality for replication.
// input: a source SQL connection and the server VERSION() string
// output: the current binary-log file and position, using the statement that server still accepts
// pos: the only place that chooses SHOW MASTER STATUS, SHOW BINARY LOG STATUS, or SHOW BINLOG STATUS
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"strconv"
	"strings"
	"sync"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
)

// Statements that return the current binary log file and position.
// MySQL 8.4 removed SHOW MASTER STATUS. SHOW BINARY LOG STATUS is the
// replacement (the only form on 8.4 and later). MariaDB's synonym is
// SHOW BINLOG STATUS, which MySQL does not implement.
const (
	stmtVersion         = "SELECT VERSION()"
	stmtMasterStatus    = "SHOW MASTER STATUS"
	stmtBinaryLogStatus = "SHOW BINARY LOG STATUS"
	stmtBinlogStatus    = "SHOW BINLOG STATUS"
)

var errEmptyBinaryLogStatus = errors.New("empty binary log status")

// binaryLogStatusStatement returns the statement that yields file and
// position on this server. MariaDB keeps SHOW MASTER STATUS (its VERSION()
// string contains "MariaDB", including 10.x and 11.x). MySQL 8.4 and newer
// only accept SHOW BINARY LOG STATUS. Anything else, including an unreadable
// version, stays on SHOW MASTER STATUS; the caller retries the other names
// if that statement fails.
func binaryLogStatusStatement(version string) string {
	if isMariaDBVersion(version) || !mysqlSeriesAtLeast(version, 8, 4) {
		return stmtMasterStatus
	}
	return stmtBinaryLogStatus
}

// binaryLogStatusStatements is the probe order for version: the statement
// that should work, then the other known names. Duplicates are omitted.
func binaryLogStatusStatements(version string) []string {
	chosen := binaryLogStatusStatement(version)
	out := []string{chosen}
	for _, stmt := range []string{stmtBinaryLogStatus, stmtMasterStatus, stmtBinlogStatus} {
		if stmt != chosen {
			out = append(out, stmt)
		}
	}
	return out
}

func isMariaDBVersion(version string) bool {
	return strings.Contains(strings.ToLower(version), "mariadb")
}

// mysqlSeriesAtLeast reports whether the leading major.minor of a MySQL-style
// version is at least major.minor. "8.4.7-log" and "8.0.36-28" both parse.
// A string that does not start with major.minor is not at least that series.
func mysqlSeriesAtLeast(version string, major, minor int) bool {
	version = strings.TrimSpace(version)
	end := 0
	for end < len(version) {
		c := version[end]
		if (c < '0' || c > '9') && c != '.' {
			break
		}
		end++
	}
	parts := strings.Split(version[:end], ".")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if maj != major {
		return maj > major
	}
	return min >= minor
}

// statusSQL runs one statement and returns the first row as strings.
type statusSQL interface {
	queryRow(stmt string) ([]string, error)
}

type connStatusSQL struct {
	conn *sqlclient.Conn
}

func (c connStatusSQL) queryRow(stmt string) ([]string, error) {
	result, err := c.conn.Execute(stmt)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Resultset == nil || result.Resultset.RowNumber() == 0 {
		return nil, errEmptyBinaryLogStatus
	}
	if stmt == stmtVersion {
		value, err := result.GetString(0, 0)
		if err != nil {
			return nil, err
		}
		return []string{value}, nil
	}
	file, err := result.GetString(0, 0)
	if err != nil {
		return nil, err
	}
	pos, err := result.GetUint(0, 1)
	if err != nil {
		return nil, err
	}
	return []string{file, strconv.FormatUint(pos, 10)}, nil
}

// binaryLogStatusCache remembers which statement worked for a host:port so
// later probes (including idle at-tip checks) do not send a statement the
// server rejects.
type binaryLogStatusCache struct {
	mu   sync.Mutex
	stmt map[string]string
}

func (c *binaryLogStatusCache) cached(endpoint string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stmt[endpoint]
}

func (c *binaryLogStatusCache) remember(endpoint, stmt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stmt == nil {
		c.stmt = map[string]string{}
	}
	c.stmt[endpoint] = stmt
}

func (c *binaryLogStatusCache) forget(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stmt, endpoint)
}

// read returns the source binary log file and position.
// A remembered statement is used as-is. If it fails, or nothing is remembered,
// VERSION() picks the first statement and the other names are the fallback.
// The statement that returned a row is remembered for this endpoint.
func (c *binaryLogStatusCache) read(endpoint string, q statusSQL) (MasterStatus, error) {
	if endpoint == "" {
		endpoint = "*"
	}
	if stmt := c.cached(endpoint); stmt != "" {
		status, err := readBinaryLogStatus(q, stmt)
		if err == nil {
			return status, nil
		}
		c.forget(endpoint)
	}

	version := ""
	if row, err := q.queryRow(stmtVersion); err == nil && len(row) > 0 {
		version = row[0]
	}
	var last error
	for _, stmt := range binaryLogStatusStatements(version) {
		status, err := readBinaryLogStatus(q, stmt)
		if err != nil {
			last = err
			continue
		}
		c.remember(endpoint, stmt)
		return status, nil
	}
	if last == nil {
		last = errEmptyBinaryLogStatus
	}
	return MasterStatus{}, last
}

func readBinaryLogStatus(q statusSQL, stmt string) (MasterStatus, error) {
	cols, err := q.queryRow(stmt)
	if err != nil {
		return MasterStatus{}, err
	}
	if len(cols) < 2 || strings.TrimSpace(cols[0]) == "" {
		return MasterStatus{}, errEmptyBinaryLogStatus
	}
	pos, err := strconv.ParseUint(strings.TrimSpace(cols[1]), 10, 32)
	if err != nil {
		return MasterStatus{}, err
	}
	return MasterStatus{File: cols[0], Pos: uint32(pos)}, nil
}

// sourceBinlogStatus is the process-wide statement cache used by FetchMasterStatus.
var sourceBinlogStatus binaryLogStatusCache
