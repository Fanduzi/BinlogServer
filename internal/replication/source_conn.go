// Package replication provides module-level functionality for replication.
// input: a task's source config and a caller context
// output: a go-mysql client connection with bounded dial, read and write, and source.tls (ADR 0012); preferred falls back only when the greeting omits CLIENT_SSL
// pos: the only way this module opens a SQL connection to the source (see source_conn_test.go)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
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

// connectSource opens a SQL connection to the source. Every source
// connection in this module must use it; a test fails on any other call.
// preferred reads the server greeting and dials plaintext only when that
// greeting's capability flags omit CLIENT_SSL. A later handshake, auth, or
// certificate error does not dial again. The Connect call stays in this function.
func connectSource(ctx context.Context, source tasks.SourceConfig) (*sqlclient.Conn, error) {
	tlsCfg, err := mysqltls.ClientConfig("source", source.TLS, source.Host)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	if mysqltls.Preferred(source.TLS) {
		offers, err := serverOffersTLS(ctx, addr)
		if err != nil {
			return nil, err
		}
		if !offers {
			tlsCfg = nil
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dialCtx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
	defer cancel()
	return sqlclient.ConnectWithContext(dialCtx, addr, source.User, source.Password, "", sourceConnectTimeout, func(c *sqlclient.Conn) error {
		c.ReadTimeout = sourceIOTimeout
		c.WriteTimeout = sourceIOTimeout
		if tlsCfg != nil {
			c.SetTLSConfig(tlsCfg)
		}
		return nil
	})
}

// serverOffersTLS reads one server greeting and reports CLIENT_SSL.
// A dial or read error is returned; it is not treated as "no TLS".
func serverOffersTLS(ctx context.Context, addr string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dialCtx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	deadline, ok := dialCtx.Deadline()
	if !ok {
		deadline = time.Now().Add(sourceConnectTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return false, err
	}
	payload, err := readMySQLPacket(conn)
	if err != nil {
		return false, err
	}
	return mysqltls.GreetingOffersTLS(payload), nil
}

func readMySQLPacket(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n <= 0 || n > 1<<20 {
		return nil, fmt.Errorf("mysql greeting length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
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
