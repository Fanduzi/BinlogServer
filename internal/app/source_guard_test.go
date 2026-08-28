// Package app provides module-level functionality for app.
// input: metadata DSN configuration, HTTP task requests, and injected metadata storage
// output: application-level regression coverage for metadata/source isolation
// pos: runtime wiring test for the task scheduler metadata endpoint policy
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"binlog_server/internal/config"
)

func TestApp_RejectsTaskUsingMetadataEndpoint(t *testing.T) {
	store := &fakeAppMetaStore{}
	originalStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) { return store, nil }
	defer func() { newAppMetaStoreForRun = originalStoreFactory }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := New(config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "meta:secret@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true",
		Mode:       "standalone",
		ListenAddr: "127.0.0.1:0",
	})
	go func() { _ = a.Run(ctx) }()
	waitReady(t, a)

	resp := postJSON(t, "http://"+a.Addr()+"/api/tasks", `{
		"name":"unsafe-source",
		"cluster_key":"unsafe-source",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret"}
	}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected create status 400, got %d body=%s", resp.StatusCode, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "metadata") {
		t.Fatalf("expected operator-facing metadata conflict, got body=%s", string(resp.Body))
	}
}
