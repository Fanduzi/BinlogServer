// Package replication proves preferred keeps one socket.
// input: a local TCP server that sends one MySQL greeting and then closes
// output: one accept when the greeting offers TLS, one accept and no CLIENT_SSL when it does not, and one accept plus an error when the greeting cannot be read
// pos: unit coverage for ADR 0012 preferred, including a handshake error that is not "does not support TLS"
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"binlog_server/internal/mysqltls"
	"binlog_server/internal/tasks"
)

func TestPreferredFallsBackOnlyWhenGreetingOmitsClientSSL(t *testing.T) {
	t.Run("offers", func(t *testing.T) {
		srv := greet(t, true)
		dialPreferred(t, srv.addr)
		if got := srv.accepts.Load(); got != 1 {
			t.Fatalf("accepts %d, want 1 (TLS on the greeting socket, no plaintext redial)", got)
		}
		if srv.sawSSL.Load() == 0 {
			t.Fatal("TLS-capable greeting was dialed without CLIENT_SSL")
		}
	})
	t.Run("omits", func(t *testing.T) {
		srv := greet(t, false)
		dialPreferred(t, srv.addr)
		if got := srv.accepts.Load(); got != 1 {
			t.Fatalf("accepts %d, want 1 (plaintext continues on the greeting socket)", got)
		}
		if srv.sawSSL.Load() != 0 {
			t.Fatal("plaintext fallback still set CLIENT_SSL")
		}
	})
	t.Run("read error", func(t *testing.T) {
		srv := greetAndClose(t)
		dialPreferred(t, srv.addr)
		if got := srv.accepts.Load(); got != 1 {
			t.Fatalf("accepts %d, want 1 (a greeting read error is not a plaintext fallback)", got)
		}
	})
}

type greetServer struct {
	addr    string
	accepts atomic.Int32
	sawSSL  atomic.Int32
}

func greet(t *testing.T, offers bool) *greetServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &greetServer{addr: ln.Addr().String()}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			srv.accepts.Add(1)
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				payload := mysqlGreeting(offers)
				hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}
				_, _ = c.Write(append(hdr, payload...))
				buf := make([]byte, 128)
				nr, _ := c.Read(buf)
				if clientOffersSSL(buf[:nr]) {
					srv.sawSSL.Store(1)
				}
			}(c)
		}
	}()
	t.Cleanup(func() { wg.Wait() })
	return srv
}

// greetAndClose accepts and closes with no bytes, so the greeting read fails.
func greetAndClose(t *testing.T) *greetServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &greetServer{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			srv.accepts.Add(1)
			c.Close()
		}
	}()
	return srv
}

func dialPreferred(t *testing.T, addr string) {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	src := tasks.SourceConfig{
		Host: host, Port: uint16(port), User: "u", Password: "wrong",
		TLS: mysqltls.Config{Mode: "preferred"},
	}
	conn, err := connectSource(context.Background(), src)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("preferred dial succeeded against a greeting-only server")
	}
}

func mysqlGreeting(offers bool) []byte {
	// A v10 greeting go-mysql will parse far enough to write a client handshake.
	// CLIENT_SSL is the only bit preferred uses to decide plaintext.
	const (
		protocol41       = uint32(1 << 9)
		clientSSL        = uint32(1 << 11)
		secureConnection = uint32(1 << 15)
		pluginAuth       = uint32(1 << 19)
	)
	flags := protocol41 | secureConnection | pluginAuth
	if offers {
		flags |= clientSSL
	}
	var b []byte
	b = append(b, 10)
	b = append(b, []byte("8.0.36")...)
	b = append(b, 0)
	b = append(b, 1, 0, 0, 0)
	b = append(b, bytesRepeat(8)...)
	b = append(b, 0)
	b = append(b, byte(flags), byte(flags>>8))
	b = append(b, 33)   // charset
	b = append(b, 0, 0) // status
	b = append(b, byte(flags>>16), byte(flags>>24))
	b = append(b, 21) // auth plugin data length
	b = append(b, bytesRepeat(10)...)
	b = append(b, bytesRepeat(12)...)
	b = append(b, 0) // auth-plugin-data terminator the parser skips
	b = append(b, []byte("mysql_native_password")...)
	b = append(b, 0)
	return b
}

func bytesRepeat(n int) []byte {
	return make([]byte, n)
}

func clientOffersSSL(buf []byte) bool {
	if len(buf) == 0 {
		return false
	}
	if buf[0] == 0x16 {
		return true
	}
	if len(buf) < 8 {
		return false
	}
	flags := uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16 | uint32(buf[7])<<24
	return flags&(1<<11) != 0
}
