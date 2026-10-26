// Package tasks provides module-level functionality for tasks.
// input: a written HealthOf verdict and that pass's finding transitions; HealthNotifier from config
// output: one webhook batch on a health change or a CRITICAL finding OPEN, CLOSE, or ESCALATE; nothing on a steady pass; an undelivered batch retried on the next pass
// pos: F2 notifier on the evaluator write path; shadow and off do not post
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"log"
	"sync"

	"binlog_server/internal/health"
	"binlog_server/internal/notify"
)

// HealthNotifier posts one batch. notify.Webhook is the implementation.
type HealthNotifier interface {
	Post(ctx context.Context, events []notify.Event) error
}

// notifyState dedups delivery for one process.
// ponytail: one mutex around the POST; split per task if webhook latency stalls evaluation.
type notifyState struct {
	mu          sync.Mutex
	told        map[string]health.Status
	undelivered []notify.Event
}

func (e *Evaluator) publish(ctx context.Context, h health.TaskHealth, ts []health.Transition) {
	if e == nil || e.cfg.Mode != EvaluatorOn || e.cfg.Notify == nil || h.TaskID == "" || e.notices == nil {
		return
	}
	n := e.notices
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.told == nil {
		n.told = map[string]health.Status{}
	}
	var events []notify.Event
	prev := n.told[h.TaskID]
	if shouldTellHealth(prev, h.Status) {
		events = append(events, notify.Event{
			Type:     "health",
			TaskID:   h.TaskID,
			Status:   string(h.Status),
			Previous: string(prev),
			At:       h.EvaluatedAt,
		})
	}
	seen := map[string]bool{}
	for _, t := range ts {
		if t.Instance.Severity != health.Critical {
			continue
		}
		if t.Op != health.OpOpen && t.Op != health.OpClose && t.Op != health.OpEscalate {
			continue
		}
		key := t.TaskID + "\x00" + string(t.Kind) + "\x00" + t.Subject + "\x00" + string(t.Op)
		if seen[key] {
			continue
		}
		seen[key] = true
		events = append(events, notify.Event{
			Type:     "finding",
			TaskID:   t.TaskID,
			Kind:     string(t.Kind),
			Severity: string(t.Instance.Severity),
			Op:       string(t.Op),
			Subject:  t.Subject,
			Detail:   clipDetail(t.Instance.Detail),
			At:       t.At,
		})
	}
	if len(events) == 0 && len(n.undelivered) == 0 {
		n.told[h.TaskID] = h.Status
		return
	}
	batch := append(append([]notify.Event{}, n.undelivered...), events...)
	if err := e.cfg.Notify.Post(ctx, batch); err != nil {
		n.undelivered = batch
		n.told[h.TaskID] = h.Status
		log.Printf("health webhook task=%s: %v", h.TaskID, err)
		return
	}
	n.undelivered = nil
	n.told[h.TaskID] = h.Status
}

func shouldTellHealth(prev, cur health.Status) bool {
	if cur == "" {
		return false
	}
	if prev == "" {
		return cur != health.StatusHealthy
	}
	return prev != cur
}

func clipDetail(s string) string {
	if len(s) > 1024 {
		return s[:1024]
	}
	return s
}
