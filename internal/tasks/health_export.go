// Package tasks provides module-level functionality for tasks.
// input: the evaluator's HealthOf verdict, the same pass's facts and open findings, and the task list this node is responsible for
// output: HealthSample values for /metrics (task_healthy, progress age on a desired-RUN task, upload lag, open CRITICAL count); a task this node does not evaluate is absent
// pos: F2 snapshot of the one evaluator verdict; it does not judge health itself
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"strings"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/health"
)

// HealthSample is one task's latest evaluator verdict and the SLO inputs
// from that same pass. Value is 1 only when HealthOf returned HEALTHY.
type HealthSample struct {
	TaskID       string
	Value        float64
	Reason       string
	ProgressAge  float64
	UploadLag    float64
	OpenCritical int
}

// HealthSamples returns one sample per task this node evaluates.
// A task with no pass yet is 0, reason unevaluated. A task another node
// owns is omitted. Nil when this process has no evaluator.
func (s *Scheduler) HealthSamples() []HealthSample {
	if s == nil {
		return nil
	}
	e := s.healthEval.Load()
	if e == nil {
		return nil
	}
	return e.HealthSamples()
}

// HealthSamples is the exportable snapshot. Caller may be a metrics scrape.
func (e *Evaluator) HealthSamples() []HealthSample {
	if e == nil {
		return nil
	}
	listed := e.responsible()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]HealthSample, 0, len(listed))
	for _, t := range listed {
		if s, ok := e.samples[t.ID]; ok {
			out = append(out, s)
			continue
		}
		out = append(out, HealthSample{TaskID: t.ID, Reason: "unevaluated"})
	}
	return out
}

func (e *Evaluator) noteVerdict(h health.TaskHealth, facts *health.Facts, open []health.Finding, readFailed bool) {
	if e == nil || h.TaskID == "" {
		return
	}
	s := sampleOf(h, facts, open, readFailed)
	e.mu.Lock()
	if e.samples == nil {
		e.samples = map[string]HealthSample{}
	}
	e.samples[h.TaskID] = s
	e.mu.Unlock()
}

func sampleOf(h health.TaskHealth, facts *health.Facts, open []health.Finding, readFailed bool) HealthSample {
	s := HealthSample{TaskID: h.TaskID, ProgressAge: progressAge(facts), OpenCritical: openCritical(open)}
	if facts != nil && facts.Chain != nil {
		s.UploadLag = facts.Chain.UploadLag.Seconds()
		if s.UploadLag < 0 {
			s.UploadLag = 0
		}
	}
	switch {
	case readFailed:
		s.Reason = "unread"
	case h.Status == health.StatusHealthy:
		s.Value, s.Reason = 1, "ok"
	case h.Status == health.StatusUnhealthy:
		s.Reason = "critical"
	case h.Status == health.StatusDegraded:
		s.Reason = "warning"
	case h.Status == health.StatusUnknown:
		s.Reason = "unknown"
	default:
		s.Reason = "unevaluated"
	}
	return s
}

func progressAge(f *health.Facts) float64 {
	// ADR 0009 §2.2 applies this clock to a desired-RUN task. A stopped task
	// keeps its last source byte; that age is not a stall.
	if f == nil || f.Task == nil || f.Task.Desired != "RUN" || f.Now.IsZero() {
		return 0
	}
	var last time.Time
	if f.Task.Liveness != nil {
		last = f.Task.Liveness.LastRead
	}
	if f.Task.Progress != nil && f.Task.Progress.LastAdvance.After(last) {
		last = f.Task.Progress.LastAdvance
	}
	if last.IsZero() {
		return 0
	}
	age := f.Now.Sub(last).Seconds()
	if age < 0 {
		return 0
	}
	return age
}

func openCritical(open []health.Finding) int {
	n := 0
	for _, f := range open {
		if f.Severity == health.Critical {
			n++
		}
	}
	return n
}

// uploadLagOf is the age of the oldest sealed row that is not UPLOADED with
// checksum match. A disk-only cache hit has no rows and reuses the cached age.
func uploadLagOf(storage bool, rows []assurance.Row, cached chainCacheEntry, diskHit bool, now time.Time) time.Duration {
	if !storage {
		return 0
	}
	if len(rows) == 0 && diskHit {
		return cached.uploadLag
	}
	return oldestUnuploaded(rows, now)
}

func oldestUnuploaded(rows []assurance.Row, now time.Time) time.Duration {
	var oldest time.Time
	for _, row := range rows {
		if !row.Sealed || row.SealedAt.IsZero() {
			continue
		}
		if strings.EqualFold(row.UploadState, "UPLOADED") && strings.EqualFold(row.Checksum, "match") {
			continue
		}
		if oldest.IsZero() || row.SealedAt.Before(oldest) {
			oldest = row.SealedAt
		}
	}
	if oldest.IsZero() || now.IsZero() {
		return 0
	}
	d := now.Sub(oldest)
	if d < 0 {
		return 0
	}
	return d
}
