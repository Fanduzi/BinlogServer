// Package replication provides module-level functionality for replication.
// input: a task's source config and a caller context
// output: a go-mysql client connection with bounded dial, read and write
// pos: the only way this module opens a SQL connection to the source (see source_conn_test.go)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"fmt"
	"time"

	"binlog_server/internal/tasks"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
)

// sourceConnectTimeout bounds the dial and handshake; sourceIOTimeout
// bounds each read and write after it. A black-holed source costs at most
// these, never a hang (#338 QA P1).
var (
	sourceConnectTimeout = 5 * time.Second
	sourceIOTimeout      = 5 * time.Second
)

// connectSource opens a SQL connection to the source. Every source
// connection in this module must use it; a test fails on any other call.
func connectSource(ctx context.Context, source tasks.SourceConfig) (*sqlclient.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
	defer cancel()
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	return sqlclient.ConnectWithContext(ctx, addr, source.User, source.Password, "", sourceConnectTimeout, func(c *sqlclient.Conn) error {
		c.ReadTimeout = sourceIOTimeout
		c.WriteTimeout = sourceIOTimeout
		return nil
	})
}
