// Package replication provides module-level functionality for replication.
// input: a task's source config and a caller context
// output: a go-mysql client connection with bounded dial, read and write, and source.tls (ADR 0012); preferred falls back only when that dial reports the server has no TLS
// pos: the only way this module opens a SQL connection to the source (see source_conn_test.go)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"binlog_server/internal/mysqltls"
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

// mysqlServerRefusedTLS is the full sentence go-mysql returns after it has
// read the greeting and the capability flags omit CLIENT_SSL. A shorter
// substring would also match unrelated handshake text.
const mysqlServerRefusedTLS = "the MySQL Server does not support TLS required by the client"

// connectSource opens a SQL connection to the source. Every source
// connection in this module must use it; a test fails on any other call.
// preferred dials with TLS on that connection. It dials plaintext only when
// go-mysql reports the server has no CLIENT_SSL. A greeting that cannot be
// read, and any later handshake, auth, or certificate error, is returned.
// The Connect call stays in this function.
func connectSource(ctx context.Context, source tasks.SourceConfig) (*sqlclient.Conn, error) {
	tlsCfg, err := mysqltls.ClientConfig("source", source.TLS, source.Host)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	dial := func(cfg *tls.Config) (*sqlclient.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
		defer cancel()
		return sqlclient.ConnectWithContext(dialCtx, addr, source.User, source.Password, "", sourceConnectTimeout, func(c *sqlclient.Conn) error {
			c.ReadTimeout = sourceIOTimeout
			c.WriteTimeout = sourceIOTimeout
			if cfg != nil {
				c.SetTLSConfig(cfg)
			}
			return nil
		})
	}
	conn, err := dial(tlsCfg)
	if err != nil && tlsCfg != nil && mysqltls.Preferred(source.TLS) && strings.Contains(err.Error(), mysqlServerRefusedTLS) {
		return dial(nil)
	}
	return conn, err
}

// sourceSyncerTLS is the dump's TLS config. preferred probes once through
// connectSource and clears TLS only when that connection is plaintext.
func sourceSyncerTLS(ctx context.Context, source tasks.SourceConfig) (*tls.Config, error) {
	tlsCfg, err := mysqltls.ClientConfig("source", source.TLS, source.Host)
	if err != nil || tlsCfg == nil || !mysqltls.Preferred(source.TLS) {
		return tlsCfg, err
	}
	conn, err := connectSource(ctx, source)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	res, err := conn.Execute("SHOW STATUS LIKE 'Ssl_cipher'")
	if err != nil {
		return nil, err
	}
	cipher, err := res.GetString(0, 1)
	if err != nil {
		return nil, err
	}
	if cipher == "" {
		return nil, nil
	}
	return tlsCfg, nil
}
