// Package tasks provides module-level functionality for tasks.
// input: (P2-3 stub)
// output: (P2-3 stub)
// pos: ADR 0009 phase 2 evaluator
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

type EvaluatorMode string

const (
	EvaluatorOff    EvaluatorMode = "off"
	EvaluatorShadow EvaluatorMode = "shadow"
	EvaluatorOn     EvaluatorMode = "on"
)

func ParseEvaluatorMode(s string) (EvaluatorMode, error) { return EvaluatorOff, nil }

type EvalReason string

const (
	ReasonSeal   EvalReason = "SEAL"
	ReasonUpload EvalReason = "UPLOAD"
)

type EvaluatorConfig struct {
	Mode             EvaluatorMode
	Store            healthstore.Store
	Sweep, Debounce  time.Duration
	Workers          int
	Cluster          bool
	NodeID, Instance string
	Now              func() time.Time
}

type EvaluatorStats struct{ Evaluations, Applied, Errors, Shadowed, Pending int64 }

type Evaluator struct{}

func NewEvaluator(s *Scheduler, cfg EvaluatorConfig) *Evaluator { return &Evaluator{} }
func (e *Evaluator) MarkDirty(taskID string, r EvalReason)      {}
func (e *Evaluator) EvaluateNow(ctx context.Context, taskID string) (health.TaskHealth, error) {
	return health.TaskHealth{}, nil
}
func (e *Evaluator) SweepNow(ctx context.Context) error { return nil }
func (e *Evaluator) Run(ctx context.Context)            {}
func (e *Evaluator) Stats() EvaluatorStats              { return EvaluatorStats{} }

type RehashStats struct{ Bytes, Segments, Mismatches, Unavailable int64 }
type Rehasher struct{}

func NewRehasher(e *Evaluator, bytesPerSec int64) *Rehasher { return &Rehasher{} }
func (r *Rehasher) Step(ctx context.Context) (int64, error) { return 0, nil }
func (r *Rehasher) Run(ctx context.Context)                 {}
func (r *Rehasher) Stats() RehashStats                      { return RehashStats{} }
func LoadOrCreateNodeID(dataDir string) (string, error)     { return "", nil }
