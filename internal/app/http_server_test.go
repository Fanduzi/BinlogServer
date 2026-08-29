// Package app provides module-level functionality for app.
// input: HTTP timeout config and PRODUCTION environment values
// output: HTTP timeout plus production auth/boolean parsing regression coverage
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

// TestValidateProductionAuthFailsClosed verifies production cannot accidentally expose either protected surface.
func TestValidateProductionAuthFailsClosed(t *testing.T) {
	for _, auth := range []config.APIAuthConfig{
		{},
		{Enabled: true, ProtectAPI: true},
		{Enabled: true, ProtectMetrics: true},
		{Enabled: true, Mode: "bearer", BearerToken: "test-token", ProtectAPI: true, ProtectMetrics: true},
	} {
		err := validateProductionAuth(auth, true)
		if auth.Enabled && auth.ProtectAPI && auth.ProtectMetrics && auth.BearerToken != "" {
			if err != nil {
				t.Fatalf("expected protected production config to pass: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("expected production config %+v to fail closed", auth)
		}
	}
	if err := validateProductionAuth(config.APIAuthConfig{}, false); err != nil {
		t.Fatalf("development defaults must remain unchanged: %v", err)
	}
}

func TestProductionMode(t *testing.T) {
	for _, raw := range []string{"true", " TRUE ", "1"} {
		production, err := productionMode(raw)
		if err != nil || !production {
			t.Fatalf("expected %q to enable production, got production=%t err=%v", raw, production, err)
		}
	}
	for _, raw := range []string{"", " false ", "0"} {
		production, err := productionMode(raw)
		if err != nil || production {
			t.Fatalf("expected %q to disable production, got production=%t err=%v", raw, production, err)
		}
	}
	if _, err := productionMode("production"); err == nil {
		t.Fatal("expected malformed PRODUCTION value to fail")
	}
}
