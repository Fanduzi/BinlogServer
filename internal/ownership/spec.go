// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Params and the migration facts the caller already collected
// output: effective stale_after, node_gone_after, and RTO; migration and side-kill predicates
// pos: formulas from ADR 0011 §2, §3.4, and §5 that are not themselves transitions
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

func (p Params) fill() Params {
	d := DefaultParams()
	if p.Storage == "" {
		p.Storage = d.Storage
	}
	if p.LeaseTTL <= 0 {
		p.LeaseTTL = d.LeaseTTL
	}
	if p.ClaimInterval <= 0 {
		p.ClaimInterval = d.ClaimInterval
	}
	if p.RefusalRetryCap == 0 {
		p.RefusalRetryCap = d.RefusalRetryCap
	}
	if p.Sweep <= 0 {
		p.Sweep = d.Sweep
	}
	if p.LockWait <= 0 {
		p.LockWait = d.LockWait
	}
	if p.IdleKill <= 0 {
		p.IdleKill = d.IdleKill
	}
	if p.StartToReady <= 0 {
		p.StartToReady = d.StartToReady
	}
	if p.NoProgressAfter <= 0 {
		p.NoProgressAfter = d.NoProgressAfter
	}
	if p.SideKill == "" {
		p.SideKill = d.SideKill
	}
	return p
}

// RTOBound is lease_ttl + max(idle_kill, lock_wait) + claim_interval + start_to_ready.
// side_kill is not in the default formula.
func RTOBound(p Params) time.Duration {
	p = p.fill()
	idle := p.IdleKill
	if p.LockWait > idle {
		idle = p.LockWait
	}
	return p.LeaseTTL + idle + p.ClaimInterval + p.StartToReady
}

// RTOStart is the last successful Renew, or the Acquire time when this epoch
// has not renewed.
func RTOStart(l Lease) time.Time {
	if !l.RenewedAt.IsZero() {
		return l.RenewedAt
	}
	return l.AcquiredAt
}

// EffectiveStaleAfter is max(2×sweep+lock_wait, min(S, RTO)). S is 5×sweep when
// the configured stale_after is 0. Defaults: 125s, which is longer than one 60s sweep.
func EffectiveStaleAfter(p Params) time.Duration {
	p = p.fill()
	s := p.StaleAfter
	if s <= 0 {
		s = 5 * p.Sweep
	}
	rto := RTOBound(p)
	if rto < s {
		s = rto
	}
	floor := 2*p.Sweep + p.LockWait
	if floor > s {
		return floor
	}
	return s
}

// EffectiveNodeGone is the configured value, or max(3×sweep, lease_ttl+2×sweep)
// when the configuration is 0.
func EffectiveNodeGone(p Params) time.Duration {
	p = p.fill()
	if p.NodeGoneAfter > 0 {
		return p.NodeGoneAfter
	}
	a := 3 * p.Sweep
	b := p.LeaseTTL + 2*p.Sweep
	if b > a {
		return b
	}
	return a
}

// NodeGoneRejectsStart is true only when node_gone_after is set to a positive
// value below lease_ttl+2×sweep. Zero is derived, not rejected.
func NodeGoneRejectsStart(p Params) bool {
	p = p.fill()
	if p.NodeGoneAfter <= 0 {
		return false
	}
	return p.NodeGoneAfter < p.LeaseTTL+2*p.Sweep
}

// RefusalCapRejectsStart is true when health.refusal_retry_cap_sec is negative.
func RefusalCapRejectsStart(p Params) bool { return p.RefusalRetryCap < 0 }

// RefusalDelay is min(cap, claim_interval × 2^(attempts-1)). It never returns
// a NULL deadline. attempts is the count after this failure.
func RefusalDelay(attempts int, claim, cap time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if claim <= 0 {
		claim = 2 * time.Second
	}
	if cap <= 0 {
		cap = 600 * time.Second
	}
	shift := attempts - 1
	if shift > 20 {
		return cap
	}
	d := claim << shift
	if d <= 0 || d > cap {
		return cap
	}
	return d
}

// Precheck is the 000011 gate. Any true failure skips every DDL statement.
type Precheck struct {
	LiveRegistration bool
	LongTransaction  bool
	LogBin           bool
	SuperOrTrust     bool
}

// MigrationAllowed is false when a registration is still live, a transaction
// is older than idle_kill, or binlog is on and the account cannot create the
// trigger and the procedure.
func MigrationAllowed(p Precheck) bool {
	if p.LiveRegistration || p.LongTransaction {
		return false
	}
	if p.LogBin && !p.SuperOrTrust {
		return false
	}
	return true
}

// SessionLockWaitTimeout is the DDL session cap set after the precheck.
const SessionLockWaitTimeout = 10 * time.Second

// BackfillChain is INSERT IGNORE of an unverified chain row. verified_at stays
// NULL. A row that already exists is returned unchanged.
func BackfillChain(existing Chain, haveExisting bool, owner string, epoch int64) (Chain, bool) {
	if haveExisting {
		return existing, false
	}
	if owner == "" {
		return Chain{}, false
	}
	return Chain{Owner: owner, VerifiedEpoch: epoch}, true
}

// GenerationBumped is the backup_tasks trigger predicate.
func GenerationBumped(oldGen, newGen int64) bool { return newGen == oldGen+1 }

// SideKillInput is what the caller knows before it would arm side_kill.
// The client never sends KILL. MariaDB has no data_locks, so a canary that
// does not already have a waiter cannot see the row lock and stays off.
type SideKillInput struct {
	Mode               string
	Direct             bool
	Procedure          bool
	CanaryPassed       bool
	Flavor             string
	MariaDBWaiter      bool
	ProcessPrivilege   bool
	DataLocksPrivilege bool
}

// KillPlan is the side_kill decision. ClientSendsKill is always false.
type KillPlan struct {
	Mode            string
	CallProcedure   bool
	ClientSendsKill bool
}

// PlanSideKill defaults to off. auto calls the procedure only for a direct
// connection whose canary passed and whose privileges are present. MariaDB
// additionally needs a waiter, because its lock view only shows waited locks.
func PlanSideKill(in SideKillInput) KillPlan {
	mode := in.Mode
	if mode == "" {
		mode = "off"
	}
	plan := KillPlan{Mode: mode}
	if mode != "auto" || !in.Direct || !in.Procedure || !in.CanaryPassed || !in.ProcessPrivilege {
		return plan
	}
	if in.Flavor == "mariadb" {
		if !in.MariaDBWaiter {
			return plan
		}
	} else if !in.DataLocksPrivilege {
		return plan
	}
	plan.CallProcedure = true
	return plan
}
