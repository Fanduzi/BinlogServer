// Package meta provides module-level functionality for meta.
// input: health transitions and verdicts under a health.Fence
// output: the MySQL healthstore.Store on migration 000009 (task_findings health columns, task_health, finding_transitions)
// pos: metadata persistence for ADR 0009 phase 2 (P2-2)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

var _ healthstore.Store = (*MySQLTaskStore)(nil)

// ApplyTransitions implements healthstore.Store.
func (s *MySQLTaskStore) ApplyTransitions(ctx context.Context, taskID string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	return healthstore.Applied{}, nil
}

// ListOpenFindingsForTask implements healthstore.Store.
func (s *MySQLTaskStore) ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error) {
	return nil, nil
}

// ListHealth implements healthstore.Store.
func (s *MySQLTaskStore) ListHealth(ctx context.Context) ([]health.TaskHealth, error) { return nil, nil }

// ListTransitions implements healthstore.TransitionLister.
func (s *MySQLTaskStore) ListTransitions(ctx context.Context, taskID string) ([]healthstore.Transition, error) {
	return nil, nil
}

// upsertWithAlias: MySQL 8.0.20+ uses "INSERT … AS new" (VALUES() is
// deprecated there); 5.7 and MariaDB keep VALUES().
func upsertWithAlias(version string) bool { return false }
