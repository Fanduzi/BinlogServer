// Package notify posts health and finding events to one generic webhook.
// input: an httptest server that records POSTs
// output: proof of a JSON delivery with the token header, a retry after 500, no retry after 400, and no token on a redirect to another host
// pos: F2 webhook tests
// note: if this file changes, update this header and module README.md.
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWebhookDeliversJSON(t *testing.T) {
	var mu sync.Mutex
	var got payload
	var token, delivery, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		token = r.Header.Get("X-BinlogServer-Token")
		delivery = r.Header.Get("X-BinlogServer-Delivery")
		ctype = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("json: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	at := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	err := (Webhook{URL: srv.URL, Token: "sekrit", Timeout: time.Second, Attempts: 1}).Post(context.Background(), []Event{{
		Type: "health", TaskID: "7", Status: "UNHEALTHY", Previous: "HEALTHY", At: at,
	}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if token != "sekrit" || delivery == "" || ctype != "application/json" {
		t.Fatalf("headers token=%q delivery=%q type=%q", token, delivery, ctype)
	}
	if got.Version != 1 || len(got.Events) != 1 || got.Events[0].TaskID != "7" || got.Events[0].Status != "UNHEALTHY" {
		t.Fatalf("payload %+v", got)
	}
}

func TestWebhookRetriesThenSucceeds(t *testing.T) {
	var n int
	var deliveries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		deliveries = append(deliveries, r.Header.Get("X-BinlogServer-Delivery"))
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := (Webhook{URL: srv.URL, Timeout: time.Second, Attempts: 3, Backoff: time.Millisecond}).Post(context.Background(), []Event{{
		Type: "finding", TaskID: "7", Kind: "METADATA_WRITE_BLOCKED", Severity: "CRITICAL", Op: "OPEN", At: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("posts = %d, want 2", n)
	}
	if deliveries[0] == "" || deliveries[0] != deliveries[1] {
		t.Fatalf("delivery ids %q", deliveries)
	}
}

func TestWebhookDoesNotRetryClientError(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	err := (Webhook{URL: srv.URL, Timeout: time.Second, Attempts: 3, Backoff: time.Millisecond}).Post(context.Background(), []Event{{
		Type: "health", TaskID: "1", Status: "UNHEALTHY", At: time.Now().UTC(),
	}})
	if err == nil || n != 1 {
		t.Fatalf("err=%v posts=%d", err, n)
	}
}

// TestWebhookDoesNotForwardTokenOnRedirect is the redirect guard: a 302 to
// another host must not carry X-BinlogServer-Token. The default client would
// follow the redirect and copy that header; CheckRedirect stops it.
func TestWebhookDoesNotForwardTokenOnRedirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var token, auth string
	var hits int
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		token = r.Header.Get("X-BinlogServer-Token")
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	other.Listener = ln
	other.Start()
	defer other.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer src.Close()

	_ = (Webhook{URL: src.URL, Token: "sekrit", Timeout: time.Second, Attempts: 1}).Post(context.Background(), []Event{{
		Type: "health", TaskID: "7", Status: "UNHEALTHY", At: time.Now().UTC(),
	}})
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 || token != "" || auth != "" {
		t.Fatalf("token forwarded to %s: hits=%d token=%q authorization=%q", other.URL, hits, token, auth)
	}
}
