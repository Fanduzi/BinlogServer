package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"binlog_server/internal/config"
)

func TestApp_StartAndServeHealth(t *testing.T) {
	cfg := config.Config{ListenAddr: "127.0.0.1:0"}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not become ready in time")
	}

	resp, err := http.Get("http://" + a.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("app returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app did not shut down in time")
	}
}
