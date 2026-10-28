// Package config provides module-level functionality for config.
// input: cluster.storage and health.lease_ttl/sweep/node_gone_after/refusal_retry_cap values
// output: validation errors for a storage mode outside per_node/shared and for ownership bounds ADR 0011 requires before start
// pos: cluster claim validation, called from validateConfig in config.go
// note: if this file changes, update this header and module README.md.
package config

import (
	"fmt"
	"strings"
	"time"

	"binlog_server/internal/ownership"
)

func validateClusterStorage(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "per_node", "shared":
		return nil
	default:
		return fmt.Errorf("cluster.storage must be per_node or shared, got %q", mode)
	}
}

func validateOwnershipBounds(cfg Config) error {
	if cfg.Health.RefusalRetryCapSec < 0 {
		return fmt.Errorf("health.refusal_retry_cap_sec must not be negative, got %d", cfg.Health.RefusalRetryCapSec)
	}
	p := ownership.Params{
		LeaseTTL:        time.Duration(cfg.Cluster.LeaseTTLSec) * time.Second,
		Sweep:           time.Duration(cfg.Health.SweepIntervalSec) * time.Second,
		ClaimInterval:   2 * time.Second,
		NodeGoneAfter:   time.Duration(cfg.Health.NodeGoneAfterSec) * time.Second,
		RefusalRetryCap: time.Duration(cfg.Health.RefusalRetryCapSec) * time.Second,
	}
	if ownership.NodeGoneRejectsStart(p) {
		return fmt.Errorf("health.node_gone_after_sec %d is below lease_ttl + 2×sweep", cfg.Health.NodeGoneAfterSec)
	}
	if ownership.RefusalCapRejectsStart(p) {
		return fmt.Errorf("health.refusal_retry_cap_sec must not be negative")
	}
	return nil
}
