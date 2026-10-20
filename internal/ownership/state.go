// Package ownership is the pure task-ownership transition from ADR 0011.
// input: none (no scheduler, database, or clock)
// output: State, the task row, lease, chain, refusal table, finding set, and the applied-event trace
// pos: value types for phase, desired, chain, refusal, and findings
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// Phase is the observation column. Desired is separate.
type Phase string

const (
	PhaseCreated       Phase = "CREATED"
	PhaseStarting      Phase = "STARTING"
	PhaseRunning       Phase = "RUNNING"
	PhaseLeaseDegraded Phase = "LEASE_DEGRADED"
	PhaseRetryBackoff  Phase = "RETRY_BACKOFF"
	PhaseFailed        Phase = "FAILED"
	PhaseStopping      Phase = "STOPPING"
	PhaseStopped       Phase = "STOPPED"
)

// Desired is operator intent. Only OperatorStop and OperatorDelete write STOP.
type Desired string

const (
	DesiredRun  Desired = "RUN"
	DesiredStop Desired = "STOP"
)

// Storage is cluster.storage. The zero value is not used; DefaultParams uses per_node.
type Storage string

const (
	StoragePerNode Storage = "per_node"
	StorageShared  Storage = "shared"
)

// Failed reasons. FAILED is one of these, never an unclassified runner error.
const (
	ReasonSourceAuth     = "SOURCE_AUTH"
	ReasonLogBinOff      = "LOG_BIN_OFF"
	ReasonChainOwnerGone = "CHAIN_OWNER_GONE"
	ReasonLegacyUnknown  = "LEGACY_UNKNOWN"
)

// Refusal reasons stored on task_node_refusal.
const (
	RefusalTail      = "TailUnreadable"
	RefusalEpochLost = "EpochLost"
)

// RetryBudget is the SOURCE_UNREACHABLE budget already on the row (0005).
// Past it, the error leaves the retry allowlist and is EpochLost, not FAILED.
const RetryBudget = 10

// Finding names derived from facts. META_LOCK_STALLED is written on _cluster.
const (
	FindingTail         = "SEGMENT_NOT_ON_WORKER"
	FindingMissing      = "SEGMENT_MISSING"
	FindingUnreadable   = "SEGMENT_UNREADABLE"
	FindingNoProgress   = "TASK_NO_PROGRESS"
	FindingFailed       = "TASK_FAILED"
	FindingLockStalled  = "META_LOCK_STALLED"
	FindingEpochLost    = "EPOCH_LOST"
	FindingMixedVersion = "MIXED_VERSION"
)

const (
	SeverityCritical = "CRITICAL"
	SeverityWarning  = "WARNING"
)

// Params is the cluster configuration the transition reads. Zero durations
// take DefaultParams. NodeGoneAfter 0 and StaleAfter 0 keep the ADR's
// derived effective values.
type Params struct {
	Storage         Storage
	LeaseTTL        time.Duration
	ClaimInterval   time.Duration
	RefusalRetryCap time.Duration
	Sweep           time.Duration
	LockWait        time.Duration
	IdleKill        time.Duration
	StartToReady    time.Duration
	NodeGoneAfter   time.Duration
	NoProgressAfter time.Duration
	StaleAfter      time.Duration
	SideKill        string
	Uploads         bool
}

// DefaultParams matches the published defaults (TTL 15s, RTO bound 32s).
func DefaultParams() Params {
	return Params{
		Storage:         StoragePerNode,
		LeaseTTL:        15 * time.Second,
		ClaimInterval:   2 * time.Second,
		RefusalRetryCap: 600 * time.Second,
		Sweep:           60 * time.Second,
		LockWait:        5 * time.Second,
		IdleKill:        5 * time.Second,
		StartToReady:    10 * time.Second,
		NoProgressAfter: 120 * time.Second,
		SideKill:        "off",
	}
}

// Lease is the task_leases row. Epoch stays after Release; owner empty means released.
type Lease struct {
	Present    bool
	Owner      string
	Epoch      int64
	ExpireAt   time.Time
	AcquiredAt time.Time
	RenewedAt  time.Time
}

// Chain is task_chain. VerifiedAt zero means the backfill candidate (NULL).
type Chain struct {
	Owner         string
	VerifiedAt    time.Time
	VerifiedEpoch int64
	MissingSealed bool
	Unreadable    bool
}

// Verified is verified_at non-NULL. An unverified owner is not the chain owner
// for TailUnreadable.
func (c Chain) Verified() bool { return !c.VerifiedAt.IsZero() && c.Owner != "" }

// Refusal is one task_node_refusal row. OpenEnded is refuse_until NULL.
type Refusal struct {
	Reason    string
	Attempts  int
	Until     time.Time
	OpenEnded bool
}

// Finding is one open finding. OnCluster is the _cluster row (no task_leases lock).
type Finding struct {
	Name      string
	Subject   string
	Severity  string
	OnCluster bool
}

// Key identifies a finding across owner changes. Severity is not part of the key.
func (f Finding) Key() string { return f.Name + "\x00" + f.Subject }

// Node is one cluster member's local facts. They are not a second task row.
type Node struct {
	ID          string
	CanRead     bool
	Stopped     bool
	HeartbeatAt time.Time
}

// Lock is the task_leases row lock, when someone sits inside FOR UPDATE.
type Lock struct {
	Held   bool
	HeldBy string
	Since  time.Time
}

// State is one task plus the records the transition is allowed to see.
// phase, desired, chain, refusal, and findings are the ADR 0011 names.
type State struct {
	TaskID  string
	Phase   Phase
	Desired Desired

	SpecRevision      int64
	RowGeneration     int64
	RunID             string
	LastError         string
	RetryAttempt      int
	RetryUntil        time.Time
	FailedReason      string
	FailedAt          time.Time
	FailedSpec        int64
	TailSince         time.Time
	PendingDelete     bool
	StoppedByOperator bool

	Lease    Lease
	Chain    Chain
	Refusal  map[string]Refusal
	Findings []Finding

	Nodes []Node
	Lock  Lock

	UnownedSince    time.Time
	ProgressAt      time.Time
	NoProgress      bool
	EvaluatedAt     time.Time
	LastSweep       time.Time
	HealthStatus    string
	HealthBlocked   bool
	Partitioned     bool
	MetaLockStalled bool
	LockObserver    string
	LockRecoveries  int
	MixedVersion    bool
	Deleted         bool

	// RegistrationLive is ignored by Acquire. Claim does not read it (0001, #463).
	RegistrationLive map[string]bool

	// Trace is the applied-event log. Step appends one entry per applied
	// transition and never reads it. The checker replays it for invariant 7.
	Trace []TraceEvent

	Params Params
}

// TraceEvent is one applied transition, plus the row columns the checker
// needs to replay lease and retry clocks. ProgressAt, UnownedSince, and
// NoProgress are deliberately absent: those are the fields under test.
type TraceEvent struct {
	Kind         Kind
	At           time.Time
	Phase        Phase
	Desired      Desired
	Deleted      bool
	Owner        string
	Expire       time.Time
	Acquired     time.Time
	Present      bool
	RetryAttempt int
	RetryUntil   time.Time
}

// New is CREATED, desired RUN, no owner. Claim does not pull CREATED.
func New() State {
	return State{
		TaskID:       "task",
		Phase:        PhaseCreated,
		Desired:      DesiredRun,
		HealthStatus: "HEALTHY",
		Lease:        Lease{Present: true},
		Refusal:      map[string]Refusal{},
		Params:       DefaultParams(),
	}
}
