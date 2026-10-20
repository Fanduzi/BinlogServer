// Package ownership is the pure task-ownership transition from ADR 0011.
// input: the event enum in ADR 0011 §3.2, plus the clock the caller passes to Step
// output: Event and Cause, the only inputs Step reads besides State
// pos: names of every transition the ADR lists
// note: if this file changes, update this header and module README.md.
package ownership

// Kind is one event name from ADR 0011 §3.2. Renew, Sweep, Lock, EvalDirFailed,
// and LegacyUpdate are the clocks and fences that section assumes but does not
// give their own row: renew extends the lease, sweep derives time-based
// findings, Lock is the in-flight FOR UPDATE, EvalDirFailed is a directory
// read that must not write chain, and LegacyUpdate is an old binary's
// backup_tasks write that does not bump row_generation.
type Kind string

const (
	EventOperatorStart   Kind = "OperatorStart"
	EventOperatorStop    Kind = "OperatorStop"
	EventOperatorDelete  Kind = "OperatorDelete"
	EventStopConfirmed   Kind = "StopConfirmed"
	EventStopExpired     Kind = "StopExpired"
	EventAcquire         Kind = "Acquire"
	EventLeaseExpired    Kind = "LeaseExpired"
	EventDumpReady       Kind = "DumpReady"
	EventRetryableError  Kind = "RetryableError"
	EventTerminalError   Kind = "TerminalError"
	EventTailUnreadable  Kind = "TailUnreadable"
	EventEpochLost       Kind = "EpochLost"
	EventChainVerified   Kind = "ChainVerified"
	EventChainOwnerGone  Kind = "ChainOwnerGone"
	EventChainOwnerBack  Kind = "ChainOwnerBack"
	EventMetaLockTimeout Kind = "MetaLockTimeout"
	EventRenew           Kind = "Renew"
	EventSweep           Kind = "Sweep"
	EventLock            Kind = "Lock"
	EventEvalDirFailed   Kind = "EvalDirFailed"
	EventLegacyUpdate    Kind = "LegacyUpdate"
)

// Cause classifies a runner failure. TerminalError accepts only the closed
// allowlist. Everything else is RetryableError or EpochLost.
type Cause string

const (
	CauseSourceAuth         Cause = "SOURCE_AUTH"
	CauseLogBinOff          Cause = "LOG_BIN_OFF"
	CauseUnclassified       Cause = "UNCLASSIFIED"
	CauseENOENT             Cause = "ENOENT"
	CauseENOSPC             Cause = "ENOSPC"
	CauseEROFS              Cause = "EROFS"
	CauseWriteFenced        Cause = "WRITE_FENCED"
	CauseLeaseUnreadable    Cause = "LEASE_UNREADABLE"
	CauseLockWait           Cause = "1205"
	CauseIdleKilled         Cause = "1927"
	CauseSegmentNotOnWorker Cause = "SEGMENT_NOT_ON_WORKER"
)

// Event is one proposed transition. Now is not a field; Step receives it.
type Event struct {
	Kind          Kind
	Node          string
	Epoch         int64
	RowGeneration int64
	ChangesSpec   bool
	Cause         Cause
	ReadOK        bool
	MissingSealed bool
	Unreadable    bool
	Incomplete    bool
	MetaDown      bool
	// LostRace is the operator transaction that, under the lease lock, saw desired STOP.
	LostRace bool
	// LeaseUnreadable means the decider could not read task_leases and must not
	// assume the lease is still its own.
	LeaseUnreadable bool
}

// Held fills the epoch fence from the lease the caller believes it holds.
func Held(s State, kind Kind, node string) Event {
	return Event{Kind: kind, Node: node, Epoch: s.Lease.Epoch, RowGeneration: s.RowGeneration}
}
