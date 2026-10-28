// Package replication provides module-level functionality for replication.
// input: a task's source config and a caller context
// output: a go-mysql client connection with bounded dial, read and write, and source.tls (ADR 0012); preferred reads CLIENT_SSL on that connection and continues plaintext there when the greeting omits it
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
// preferred reads the greeting on the socket it keeps. CLIENT_SSL continues
// as TLS on that socket. No CLIENT_SSL continues as plaintext on that socket:
// closing it and dialing again is an aborted connect, and concurrent tasks
// trip max_connect_errors before any success can reset the host cache.
// A greeting that cannot be read is returned. Auth and certificate errors
// are returned from that one socket. The Connect call stays in this function.
func connectSource(ctx context.Context, source tasks.SourceConfig) (*sqlclient.Conn, error) {
	tlsCfg, err := mysqltls.ClientConfig("source", source.TLS, source.Host)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	dial := func(conn net.Conn, cfg *tls.Config) (*sqlclient.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
		defer cancel()
		dialer := (&net.Dialer{Timeout: sourceConnectTimeout}).DialContext
		if conn != nil {
			dialer = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
		}
		return sqlclient.ConnectWithDialer(dialCtx, "tcp", addr, source.User, source.Password, "", dialer, func(c *sqlclient.Conn) error {
			c.ReadTimeout = sourceIOTimeout
			c.WriteTimeout = sourceIOTimeout
			if cfg != nil {
				c.SetTLSConfig(cfg)
			}
			return nil
		})
	}
	if tlsCfg == nil || !mysqltls.Preferred(source.TLS) {
		return dial(nil, tlsCfg)
	}
	conn, payload, err := peekMySQLGreeting(ctx, addr)
	if err != nil {
		return nil, err
	}
	if !mysqltls.GreetingOffersTLS(payload) {
		tlsCfg = nil
	}
	return dial(conn, tlsCfg)
}

// prefixConn replays bytes already read from a MySQL socket, then the rest
// of the connection. preferred uses it so the handshake continues on the
// socket that sent the greeting.
type prefixConn struct {
	net.Conn
	buf []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.buf) == 0 {
		return c.Conn.Read(p)
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// peekMySQLGreeting dials and reads one protocol packet. The returned conn
// replays that packet. A short read closes the socket and returns the error.
func peekMySQLGreeting(ctx context.Context, addr string) (net.Conn, []byte, error) {
	dialCtx, cancel := context.WithTimeout(ctx, sourceConnectTimeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if err := raw.SetReadDeadline(time.Now().Add(sourceConnectTimeout)); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(raw, hdr); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n <= 0 || n > 1<<20 {
		_ = raw.Close()
		return nil, nil, fmt.Errorf("source greeting length %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(raw, payload); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	if err := raw.SetReadDeadline(time.Time{}); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	rawPacket := append(append([]byte{}, hdr...), payload...)
	return &prefixConn{Conn: raw, buf: rawPacket}, payload, nil
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
	defer func() { _ = conn.Close() }()
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
