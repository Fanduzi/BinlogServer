// Package tasks provides module-level functionality for tasks.
// input: an evaluator with a webhook pointed at httptest, and a CRITICAL signal
// output: one delivery on the transition to unhealthy, a retry inside that delivery, and no second POST while the verdict stays
// pos: F2 steady-state webhook test
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/healthstore"
	"binlog_server/internal/notify"
)

func TestWebhookSteadyStateDoesNotRepeat(t *testing.T) {
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		posts := n
		mu.Unlock()
		_, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		if posts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, _ := evalTask(t, s, data)
	store := healthstore.NewFileStore(data + "/.health.json")
	hook := notify.Webhook{URL: srv.URL, Token: "t", Timeout: time.Second, Attempts: 3, Backoff: time.Millisecond}
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorOn, Store: store, NodeID: "n", Instance: "i",
		Sweep: time.Minute, Debounce: time.Millisecond, Notify: hook,
	})
	ctx := context.Background()
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if n != 0 {
		mu.Unlock()
		t.Fatalf("healthy baseline posted %d", n)
	}
	mu.Unlock()

	e.Signal(task.ID, FindingTaskPersistFailed, "", false, "meta down")
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if n != 2 {
		mu.Unlock()
		t.Fatalf("posts after critical = %d, want 2 (one failed attempt, then success)", n)
	}
	mu.Unlock()

	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Fatalf("steady state posted again, posts=%d", n)
	}
}

func TestShadowDoesNotPost(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, _ := evalTask(t, s, data)
	store := healthstore.NewFileStore(data + "/.health.json")
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorShadow, Store: store, NodeID: "n", Instance: "i",
		Sweep: time.Minute, Debounce: time.Millisecond,
		Notify: notify.Webhook{URL: srv.URL, Timeout: time.Second, Attempts: 1},
	})
	e.Signal(task.ID, FindingTaskPersistFailed, "", false, "meta down")
	if _, err := e.EvaluateNow(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("shadow posted %d", n)
	}
}
