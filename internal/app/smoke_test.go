package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestApp_ClusterControlPlaneRole(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role: "control-plane",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() == "" {
		t.Fatal("expected control-plane role to expose api listener")
	}
	assertHTTPStatus(t, "http://"+a.Addr()+"/healthz", http.StatusOK)

	createBody := `{
		"name":"cluster-a",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl"}
	}`
	createResp := postJSON(t, "http://"+a.Addr()+"/api/tasks", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected create status 201, got %d body=%s", createResp.StatusCode, string(createResp.Body))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createResp.Body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	startResp := postJSON(t, "http://"+a.Addr()+"/api/tasks/"+created.ID+"/start", "")
	if startResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected start status 400 in control-plane role, got %d body=%s", startResp.StatusCode, string(startResp.Body))
	}

	cancel()
	waitRunExit(t, errCh)
}

func TestApp_ClusterWorkerRole(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role: "worker",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() != "" {
		t.Fatalf("expected worker role to skip control-plane listener, got addr=%s", a.Addr())
	}

	cancel()
	waitRunExit(t, errCh)
}

func TestApp_ClusterAllInOneRole(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role: "all-in-one",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() == "" {
		t.Fatal("expected all-in-one role to expose api listener")
	}
	assertHTTPStatus(t, "http://"+a.Addr()+"/healthz", http.StatusOK)

	createBody := `{
		"name":"cluster-a",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl"}
	}`
	createResp := postJSON(t, "http://"+a.Addr()+"/api/tasks", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected create status 201, got %d body=%s", createResp.StatusCode, string(createResp.Body))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createResp.Body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	startResp := postJSON(t, "http://"+a.Addr()+"/api/tasks/"+created.ID+"/start", "")
	if startResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected start status 204 in all-in-one role, got %d body=%s", startResp.StatusCode, string(startResp.Body))
	}

	cancel()
	waitRunExit(t, errCh)
}

func waitReady(t *testing.T, a *App) {
	t.Helper()
	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not become ready in time")
	}
}

func waitRunExit(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("app returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app did not shut down in time")
	}
}

func assertHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected %d, got %d body=%s", want, resp.StatusCode, string(body))
	}
}

type httpResult struct {
	StatusCode int
	Body       []byte
}

func postJSON(t *testing.T, url string, body string) httpResult {
	t.Helper()
	reqBody := bytes.NewBufferString(body)
	resp, err := http.Post(url, "application/json", reqBody)
	if err != nil {
		t.Fatalf("post %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return httpResult{
		StatusCode: resp.StatusCode,
		Body:       data,
	}
}
