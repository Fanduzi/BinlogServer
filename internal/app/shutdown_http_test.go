// Package app provides module-level functionality for app.
// input: an HTTP server built by buildHTTPServer, a raw connection that never sends a request, and one slow in-flight request
// output: shutdownHTTP returns within 2s while the in-flight request still receives its full body; a connection that becomes StateNew after closeIdle has started does not keep Server.Shutdown busy for the HTTP budget; a connection dialed after shutting is stored is closed by ConnState before the idle snapshot, which a read inside that gap observes
// pos: regression coverage for the HTTP shutdown that closes StateNew and StateIdle without cutting StateActive
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"binlog_server/internal/config"
)

func shutdownTestServer(t *testing.T, handler http.Handler) (*http.Server, func(), net.Listener) {
	t.Helper()
	srv, closeIdle := buildHTTPServer(handler, config.HTTPServerTimeoutConfig{
		ReadHeaderTimeoutSec: 30,
		ReadTimeoutSec:       30,
		WriteTimeoutSec:      30,
		IdleTimeoutSec:       30,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, closeIdle, ln
}

// TestShutdownHTTP_NeverRequestedConnAndInFlight is the shutdown regression:
// a raw connection that never sends a request must not consume the 5s HTTP
// budget, and a request that is already being served must finish.
func TestShutdownHTTP_NeverRequestedConnAndInFlight(t *testing.T) {
	entered := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(800 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	srv, closeIdle, ln := shutdownTestServer(t, mux)

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	respCh := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respCh <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			respCh <- err
			return
		}
		if string(body) != "done" {
			respCh <- errBody(body)
			return
		}
		respCh <- nil
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("slow handler did not start")
	}

	start := time.Now()
	shutdownHTTP(srv, closeIdle)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %s with a never-requested conn open", d)
	}
	select {
	case err := <-respCh:
		if err != nil {
			t.Fatalf("in-flight request cut: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish")
	}
}

type errBody []byte

func (e errBody) Error() string { return "body " + string(e) }

// TestShutdownHTTP_StateNewArrivingDuringShutdown closes the idle set first,
// then dials. The ConnState hook must close that StateNew connection.
// Server.Shutdown is called directly so a second idle snapshot cannot hide
// a hook that ignored it.
func TestShutdownHTTP_StateNewArrivingDuringShutdown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {})
	srv, closeIdle, ln := shutdownTestServer(t, mux)
	closeIdle()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, _ = raw.Read(buf)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("connection accepted during shutdown blocked Shutdown for %s", d)
	}
}

// TestShutdownHTTP_ShuttingIsSetBeforeTheSnapshot dials inside the gap
// after shutting is stored and before the snapshot. ConnState must already
// have closed that connection: a read in the hook that times out means the
// hook argument was true while the flag was still false, so the later
// snapshot was what closed it. The hook must also have observed shutting.
func TestShutdownHTTP_ShuttingIsSetBeforeTheSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) {})
	srv, closeIdle, ln := shutdownTestServer(t, mux)
	var raw net.Conn
	connTrackSnap = func(shutting bool) {
		if !shutting {
			t.Errorf("shutting was false at the snapshot")
		}
		var err error
		raw, err = net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		_ = raw.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 1)
		_, err = raw.Read(buf)
		var ne net.Error
		if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
			t.Errorf("ConnState left the new connection open: %v", err)
		}
	}
	t.Cleanup(func() { connTrackSnap = nil })
	closeIdle()
	if raw == nil {
		t.Fatal("snapshot hook did not run")
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	if _, err := raw.Read(buf); err == nil {
		t.Fatal("connection accepted before the snapshot was left open")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("connection accepted before the snapshot blocked Shutdown for %s", d)
	}
}
