// Package health provides module-level functionality for health.
// input: one task's facts at one moment (task state, assurance chain verdicts, hot-path signals, store capability, waivers)
// output: the findings that should be open now (Desire), the transitions from the stored open set to that (Reconcile), and the task's health (HealthOf)
// pos: the single lifecycle owner of findings (ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md); pure, no I/O, imports only assurance
// note: if this file changes, update this header and module README.md.
package health

import (
	"time"

	"binlog_server/internal/assurance"
)

// Kind is a closed set; every Kind has a KindSpec in DefaultRegistry.
type Kind string

// Severity orders CRITICAL > WARNING > INFO.
type Severity string

const (
	Critical Severity = "CRITICAL"
	Warning  Severity = "WARNING"
	Info     Severity = "INFO"
)

func (s Severity) rank() int {
	switch s {
	case Critical:
		return 3
	case Warning:
		return 2
	case Info:
		return 1
	}
	return 0
}

// Scope is the input source a kind is judged from. A pass only closes
// findings of a scope whose inputs were complete.
type Scope string

const (
	ScopeChainLocal   Scope = "chain.local"
	ScopeChainObjects Scope = "chain.objects"
	ScopeTaskState    Scope = "task.state"
	ScopeSignals      Scope = "signals"
	ScopeStore        Scope = "store"
	ScopeVerify       Scope = "verify"
)

// Key identifies a finding: the same key is the same finding.
type Key struct {
	TaskID  string
	Kind    Kind
	Subject string
}

// Instance is one finding that should be open.
type Instance struct {
	Key
	Severity Severity
	Detail   string
	Evidence string
	Segments []string
	// FactAt is when the fact happened (signal time); zero = Facts.Now.
	FactAt time.Time
}

// ClearRule says when an open finding of a kind closes by itself.
type ClearRule struct {
	Invariant string
	// Holds reports the condition no longer holds for key. nil = absent
	// from Desire is enough.
	Holds    func(f *Facts, key Key) bool
	Text     string
	HoldDown time.Duration
	// Reason, when set, names the close (for example HANDED_OFF); "" = EVAL.
	Reason func(f *Facts, key Key) string
}

// KindSpec is a kind's registry entry.
type KindSpec struct {
	Kind     Kind
	Severity Severity
	// Scopes: every scope the kind is judged from. A finding closes only
	// when all of them are complete; Derive runs when the first is.
	Scopes        []Scope
	Clear         ClearRule
	Derive        func(f *Facts) []Instance
	SegmentScoped bool
	// Managed: the evaluator owns open and close. An unmanaged kind keeps
	// its legacy Report/Resolve lifecycle and Reconcile never touches it.
	Managed  bool
	Version  int
	Waivable bool
}

// Registry maps every kind to its spec.
type Registry map[Kind]KindSpec

// TaskFacts is the task row as cross-checked against the lease table.
type TaskFacts struct {
	State, Desired string
	Owner          string
	Epoch          int64
	RunHere        bool
	StateSince     time.Time
	LastError      string
	// FailedCurrent: the task is FAILED for the operator's current ask (its
	// failed spec revision is the current one). FAILED sets desired STOP, so
	// desired alone cannot tell a failed run from a deliberate stop.
	FailedCurrent bool
	// OwnedElsewhere: another node owns the row (it drives the transition).
	OwnedElsewhere bool
	// Unadopted: standalone with no task store, and the task is only a
	// directory under data_dir that holds segments; no record of the task
	// exists in this process, so nothing replicates it until adopt (#385).
	Unadopted bool
	// Liveness is the task's source liveness clock (nil: this node keeps
	// none for the task, so DUMP_STALLED and SOURCE_UNREACHABLE are not
	// derived here).
	Liveness *Liveness
}

// Liveness is the one source liveness clock (§18): when a byte (event or
// heartbeat) was last read from the source at the socket, by the dump or a
// probe, and the latest failed connection attempt. It keeps running
// through STARTING and RETRY_BACKOFF. LastRead is never before the time the
// clock started (process start, or the task's latest Start).
type Liveness struct {
	LastRead, LastFailure time.Time
	FailureDetail         string
}

// ChainFacts is assurance's view of the chain.
type ChainFacts struct {
	Violations []assurance.Violation
	Verdict    *assurance.Violation
}

// SignalKey names one hot-path signal.
type SignalKey struct {
	Kind    Kind
	Subject string
}

// SignalState is what this process has observed of a signal since it
// started. Seqs come from one process-wide monotonic counter (no wall
// clock); a zero SuccessSeq means no success since start.
type SignalState struct {
	FailureSeq uint64
	SuccessSeq uint64
	Failures   int64
	Detail     string
	FactAt     time.Time
	// Epoch is the task lease epoch the latest failure belongs to (0 =
	// unknown); a failure of an older epoch no longer speaks for the task.
	Epoch int64
}

// Waiver exempts one exact piece of evidence (a canonical GTID range).
type Waiver struct {
	Key
	Evidence string
	Reason   string
	Actor    string
	At       time.Time
}

// Mute suppresses notifications only. Kind "*" is task-wide.
type Mute struct {
	TaskID, Kind, Subject string
	Reason, Actor         string
	StartedAt, Until      time.Time
}

// StoreFacts is what the metadata store can do.
type StoreFacts struct {
	SchemaVersion   int
	SealFactsAbsent bool
	ReadOnly        bool
}

// Facts is everything known about one task at one moment. A nil source is
// unavailable this pass; Complete says which scopes may close findings.
type Facts struct {
	TaskID   string
	Now      time.Time
	Task     *TaskFacts
	Chain    *ChainFacts
	Signals  map[SignalKey]SignalState
	Store    *StoreFacts
	Waivers  []Waiver
	Complete map[Scope]bool
	// AbsentSince is when the evaluator (on its own injected clock) first saw
	// each open finding's condition absent; hold-down runs from it, never from
	// a stored wall-clock time. A missing entry means "just now".
	AbsentSince map[Key]time.Time
	// Open is the stored open set's keys (handoff rules read it).
	Open map[Key]bool
	// Probes are the alternative success proofs of §5.5 that held this
	// pass, by open key (a signal that rarely succeeds can still close).
	Probes map[Key]bool
	// StaleSignals are open signal rows with no failure seen by this
	// process and no update for health.signal_stale_after: shown as INFO.
	StaleSignals map[Key]bool
	// UnreachableAfter is the SOURCE_UNREACHABLE threshold for this task
	// (health.unreachable_after or the task's override); 0 = 90s.
	UnreachableAfter time.Duration
	// StalledAfter is the TASK_TRANSITION_STALLED threshold; 0 = 60s.
	StalledAfter time.Duration
	// DumpStalledAfter is the DUMP_STALLED silence threshold; 0 = 45s.
	DumpStalledAfter time.Duration
}

// Finding is a stored finding row as the evaluator sees it.
type Finding struct {
	TaskID     string
	Kind       Kind
	Subject    string
	Severity   Severity
	Detail     string
	Evidence   string
	Segments   []string
	FirstSeen  time.Time
	LastSeen   time.Time
	Generation int64
}

// Key is the finding's key.
func (f Finding) Key() Key { return Key{f.TaskID, f.Kind, f.Subject} }

// Op is a transition type.
type Op string

const (
	OpOpen      Op = "OPEN"
	OpUpdate    Op = "UPDATE"
	OpEscalate  Op = "ESCALATE"
	OpClose     Op = "CLOSE"
	OpDowngrade Op = "DOWNGRADE"
)

// Transition reasons.
const (
	ReasonEval      = "EVAL"
	ReasonWaived    = "WAIVED"
	ReasonUpgrade   = "UPGRADE"
	ReasonHandedOff = "HANDED_OFF"
)

// Transition is one change to a finding. Generation is allocated by the
// store inside its write transaction and is zero here.
type Transition struct {
	Key
	Op          Op
	Reason      string
	At          time.Time
	FactAt      time.Time
	Generation  int64
	KindVersion int
	Instance    Instance
}

// Desired is the set of findings that should be open, by key.
type Desired map[Key]Instance

// Status is a task's health.
type Status string

const (
	StatusHealthy   Status = "HEALTHY"
	StatusDegraded  Status = "DEGRADED"
	StatusUnhealthy Status = "UNHEALTHY"
	StatusUnknown   Status = "UNKNOWN"
)

// TaskHealth is the health verdict for one task.
type TaskHealth struct {
	TaskID      string
	Status      Status
	Reasons     []Key
	Unknown     []Scope
	EvaluatedAt time.Time
}

// Finding kinds (§6). The first sixteen are the v0.5.66 kinds.
const (
	TaskPersistFailed        Kind = "TASK_PERSIST_FAILED"
	MetadataWriteBlocked     Kind = "METADATA_WRITE_BLOCKED"
	EventPersistFailed       Kind = "EVENT_PERSIST_FAILED"
	EventReadFailed          Kind = "EVENT_READ_FAILED"
	DumpCleanupPersistFailed Kind = "DUMP_CLEANUP_PERSIST_FAILED"
	DiskListFailed           Kind = "DISK_LIST_FAILED"
	UploadUnverified         Kind = "UPLOAD_UNVERIFIED"
	UploadRecordFailed       Kind = "UPLOAD_RECORD_FAILED"
	RetentionRecordFailed    Kind = "RETENTION_RECORD_FAILED"
	RetryLoopFailed          Kind = "RETRY_LOOP_FAILED"
	DumpStalled              Kind = "DUMP_STALLED"
	OpenTailCutFailed        Kind = "OPEN_TAIL_CUT_FAILED"
	MetricSourceFailed       Kind = "METRIC_SOURCE_FAILED"
	TransitionStalled        Kind = "TASK_TRANSITION_STALLED"
	SegmentUnreadable        Kind = "SEGMENT_UNREADABLE"
	SegmentOverlap           Kind = "SEGMENT_OVERLAP"

	SegmentEmptyLeftover Kind = "SEGMENT_EMPTY_LEFTOVER"
	SegmentDamaged       Kind = "SEGMENT_DAMAGED"
	ChainIndexGap        Kind = "CHAIN_INDEX_GAP"
	GTIDHole             Kind = "GTID_HOLE"
	LegacyFailback       Kind = "LEGACY_FAILBACK"
	CheckpointAhead      Kind = "CHECKPOINT_AHEAD"
	SegmentMissing       Kind = "SEGMENT_MISSING"
	OrphanRow            Kind = "ORPHAN_ROW"
	UploadLate           Kind = "UPLOAD_LATE"
	TaskFailed           Kind = "TASK_FAILED"
	SourceUnreachable    Kind = "SOURCE_UNREACHABLE"
	TaskUnadopted        Kind = "TASK_UNADOPTED"
	SealFactsAbsent      Kind = "SEAL_FACTS_ABSENT"
	HealthStale          Kind = "HEALTH_STALE"
	HealthStoreDegraded  Kind = "HEALTH_STORE_DEGRADED"
)

// Produced lists the scopes some evaluator produces in this phase. HealthOf
// never reports UNKNOWN for a scope that is not produced (chain.objects and
// verify arrive with the phase 3 Verifier).
var Produced = map[Scope]bool{ScopeChainLocal: true, ScopeTaskState: true, ScopeSignals: true, ScopeStore: true}
