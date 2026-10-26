// Package notify posts health and finding events to one generic webhook.
// input: a batch of events, a URL, an optional token, timeout, attempt count and backoff
// output: one JSON POST; X-BinlogServer-Token when a token is set; X-BinlogServer-Delivery is the SHA-256 of the body; non-2xx and transport errors retry with exponential backoff; redirects are not followed
// pos: ADR 0009 decision 1, the only outbound notifier
// note: if this file changes, update this header and module README.md.
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Event is one health transition or one CRITICAL finding transition.
type Event struct {
	Type     string    `json:"type"`
	TaskID   string    `json:"task_id"`
	Status   string    `json:"status,omitempty"`
	Previous string    `json:"previous,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Severity string    `json:"severity,omitempty"`
	Op       string    `json:"op,omitempty"`
	Subject  string    `json:"subject,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
}

type payload struct {
	Version int     `json:"version"`
	Events  []Event `json:"events"`
}

// Webhook is a JSON POST. An empty URL posts nothing.
type Webhook struct {
	URL      string
	Token    string
	Timeout  time.Duration
	Attempts int
	Backoff  time.Duration
	Client   *http.Client
}

// Post delivers events. A 2xx returns nil. Other 4xx (except 429) are not retried.
func (w Webhook) Post(ctx context.Context, events []Event) error {
	if w.URL == "" || len(events) == 0 {
		return nil
	}
	body, err := json.Marshal(payload{Version: 1, Events: events})
	if err != nil {
		return err
	}
	attempts := w.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	backoff := w.Backoff
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := w.Client
	if client == nil {
		client = &http.Client{CheckRedirect: refuseRedirect}
	}
	sum := sha256.Sum256(body)
	delivery := hex.EncodeToString(sum[:])
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			backoff *= 2
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-BinlogServer-Delivery", delivery)
		if w.Token != "" {
			req.Header.Set("X-BinlogServer-Token", w.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			last = err
			continue
		}
		drain(resp.Body)
		cancel()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		last = fmt.Errorf("webhook status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return last
		}
	}
	return last
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func drain(body io.ReadCloser) {
	defer body.Close()
	buf := make([]byte, 512)
	for {
		_, err := body.Read(buf)
		if err != nil {
			return
		}
	}
}
