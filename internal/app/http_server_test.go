// Package app provides module-level functionality for app.
// input: runtime config, scheduler/runner/meta store dependencies, process context
// output: application lifecycle control including startup, role wiring, and shutdown
// pos: application composition layer that wires modules into runnable service modes
// note: if this file changes, update this header and module README.md.
package app

import (
	"net/http"
	"testing"
	"time"

	"binlog_server/internal/config"
)

// TestBuildHTTPServerAppliesTimeouts 验证 HTTP server 会应用配置超时参数。
func TestBuildHTTPServerAppliesTimeouts(t *testing.T) {
	handler := http.NewServeMux()
	timeoutCfg := config.HTTPServerTimeoutConfig{
		ReadHeaderTimeoutSec: 7,
		ReadTimeoutSec:       17,
		WriteTimeoutSec:      27,
		IdleTimeoutSec:       37,
	}

	server := buildHTTPServer(handler, timeoutCfg)
	if server.ReadHeaderTimeout != 7*time.Second {
		t.Fatalf("expected ReadHeaderTimeout=7s, got %s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 17*time.Second {
		t.Fatalf("expected ReadTimeout=17s, got %s", server.ReadTimeout)
	}
	if server.WriteTimeout != 27*time.Second {
		t.Fatalf("expected WriteTimeout=27s, got %s", server.WriteTimeout)
	}
	if server.IdleTimeout != 37*time.Second {
		t.Fatalf("expected IdleTimeout=37s, got %s", server.IdleTimeout)
	}
}
