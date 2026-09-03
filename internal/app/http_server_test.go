// Package app provides module-level functionality for app.
// input: HTTP timeout config, PRODUCTION environment values, and control-plane listen_addr
// output: HTTP timeout plus production/non-loopback auth fail-closed regression coverage
// pos: application composition layer that wires modules into runnable service modes
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"net/http"
	"strings"
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

func TestIsLoopbackListenAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:18080", "[::1]:8080", "127.0.0.1:0"}
	for _, addr := range loopback {
		if !isLoopbackListenAddr(addr) {
			t.Fatalf("expected loopback listen %q", addr)
		}
	}
	nonLoopback := []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.10:8080", ""}
	for _, addr := range nonLoopback {
		if isLoopbackListenAddr(addr) {
			t.Fatalf("expected non-loopback listen %q", addr)
		}
	}
}

func TestApp_NonLoopbackListenRejectsDisabledAuth(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080"} {
		a := New(config.Config{ListenAddr: addr})
		err := a.Run(context.Background())
		if err == nil {
			t.Fatalf("expected App.Run to reject non-loopback listen %q with auth disabled", addr)
		}
		if !strings.Contains(err.Error(), "listen_addr is not loopback") {
			t.Fatalf("unexpected error for %q: %v", addr, err)
		}
		if a.Addr() != "" {
			t.Fatalf("non-loopback listen %q bound a listener without auth: %q", addr, a.Addr())
		}
	}
}

func TestApp_LoopbackListenAllowsDisabledAuth(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:18080", "[::1]:0"} {
		if err := validateControlPlaneAuth(addr, config.APIAuthConfig{}, false); err != nil {
			t.Fatalf("loopback listen %q should allow disabled auth: %v", addr, err)
		}
	}

	a := New(config.Config{ListenAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Fatal("app did not exit after cancel")
		}
	}()

	select {
	case <-a.Ready():
	case err := <-errCh:
		t.Fatalf("loopback listen with auth disabled failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("app did not become ready in time")
	}
}

func TestValidateControlPlaneAuth_NonLoopbackRequiresProtectFlags(t *testing.T) {
	auth := config.APIAuthConfig{
		Enabled: true, Mode: "bearer", BearerToken: "test-token",
	}
	if err := validateControlPlaneAuth(":8080", auth, false); err == nil {
		t.Fatal("expected non-loopback listen to require protect_api and protect_metrics")
	}
	auth.ProtectAPI = true
	auth.ProtectMetrics = true
	if err := validateControlPlaneAuth(":8080", auth, false); err != nil {
		t.Fatalf("expected fully protected non-loopback auth to pass: %v", err)
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
