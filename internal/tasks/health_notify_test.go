// Package tasks provides module-level functionality for tasks.
// input: an evaluator with a webhook pointed at httptest, and a CRITICAL signal
// output: one delivery on the transition to unhealthy, a retry inside that delivery, no second POST while the verdict stays, and a failed Post left pending until a later pass delivers it
// pos: F2 steady-state webhook test
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Store: store, NodeID: "n", Instance: "i",
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

// TestWebhookKeepsUndeliveredAcrossFailedPost is the retention guard: a Post
// that fails leaves the batch pending, and the next pass delivers it once the
// endpoint answers. Clearing undelivered on that error drops the batch.
func TestWebhookKeepsUndeliveredAcrossFailedPost(t *testing.T) {
	var mu sync.Mutex
	fail := true
	var n int
	var delivered []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		mu.Lock()
		n++
		down := fail
		if !down {
			delivered = body
		}
		mu.Unlock()
		if down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, _ := evalTask(t, s, data)
	store := healthstore.NewFileStore(data + "/.health.json")
	e := NewEvaluator(s, EvaluatorConfig{
		Store: store, NodeID: "n", Instance: "i",
		Sweep: time.Minute, Debounce: time.Millisecond,
		Notify: notify.Webhook{URL: srv.URL, Timeout: time.Second, Attempts: 1},
	})
	ctx := context.Background()
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	e.Signal(task.ID, FindingTaskPersistFailed, "", false, "meta down")
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if n != 1 {
		mu.Unlock()
		t.Fatalf("posts while the endpoint is down = %d, want 1", n)
	}
	fail = false
	mu.Unlock()

	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Fatalf("posts after the endpoint recovered = %d, want 2 (the pending batch)", n)
	}
	if !strings.Contains(string(delivered), task.ID) {
		t.Fatalf("recovered body %q, want the pending event for %s", delivered, task.ID)
	}
}
