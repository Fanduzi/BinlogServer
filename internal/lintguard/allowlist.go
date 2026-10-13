// Package lintguard finds ignored errors in the backup data path (ADR 0009 phase 0).
// input: reviewed ignored-error sites
// output: allowlist, keyed by Site.Key(), with a count and the reason each site is fine
// pos: the reviewed baseline for TestNoNewIgnoredErrors; "follow-up:" marks phase 1 gaps
// note: if this file changes, update this header and module README.md.
package lintguard

// Allowed is a reviewed ignored-error site: how many there are and why each is fine.
type Allowed struct {
	Count  int
	Reason string
}

const (
	cleanupOnErrorPath = "close/remove while already returning another error; that error is the one reported"
	persistReported    = "persistTaskLocked reports TASK_PERSIST_FAILED and retries (S2, #336); the log is the extra line"
	gtidUpdateParsed   = "the GTID text came from a set that already parsed; Update cannot fail on it"
)

// allowlist is keyed by Site.Key() ("file:func:kind").
var allowlist = map[string]Allowed{
	"internal/binlog/ledger.go:unionDurable:blank-call":                                    {1, gtidUpdateParsed},
	"internal/replication/gtid_checkpoint.go:executedGTID.union:blank-call":                {1, gtidUpdateParsed},
	"internal/replication/mysql_runner.go:MySQLRunner.materializeUploaded:blank-call":      {3, cleanupOnErrorPath + "; the takeover temp file is also swept by endTakeoverTemp"},
	"internal/replication/mysql_runner.go:MySQLRunner.openBinlogWriterIn:blank-call":       {4, cleanupOnErrorPath},
	"internal/replication/mysql_runner.go:MySQLRunner.openBinlogWriterIn:log-only":         {3, cleanupOnErrorPath},
	"internal/replication/mysql_runner.go:defaultServerID:blank-call":                      {1, "hash.Hash.Write never returns an error"},
	"internal/replication/source_switch.go:clearResumeGTID:blank-call":                     {1, "follow-up: a resume-GTID file that cannot be removed is read again on the next start; should report a finding"},
	"internal/tasks/legacy_failback.go:fetchLegacySegment:blank-call":                      {1, cleanupOnErrorPath},
	"internal/tasks/legacy_failback.go:syncDir:blank-call":                                 {1, "best-effort directory fsync; some filesystems reject fsync on a directory"},
	"internal/tasks/replay_archive.go:Scheduler.openSealedSuccessor:blank-call":            {1, "read-side close of an object body being skipped; no backup state changes"},
	"internal/tasks/replay_archive.go:closeReplayMembers:blank-call":                       {1, "read-side close of replay members; no backup state changes"},
	"internal/tasks/replay_archive.go:materializeReplayTar:blank-call":                     {3, cleanupOnErrorPath + "; scratch replay tar only"},
	"internal/tasks/scheduler_dump_cleanup.go:Scheduler.killLeftover:log-only":             {1, "the pending KILL marker stays and is retried every pass; marker write failures are DUMP_CLEANUP_PERSIST_FAILED (S3)"},
	"internal/tasks/scheduler_dump_cleanup.go:Scheduler.noteDumpCleanup:log-only":          {1, persistReported},
	"internal/tasks/scheduler_dump_cleanup.go:Scheduler.retryPendingDumpCleanups:log-only": {1, "the pass runs again on the next tick; a stale list only delays a KILL"},
	"internal/tasks/scheduler_lifecycle.go:Scheduler.claimOwnedIdleTasks:blank-call":       {1, "follow-up: StopTask after an invalid source config; its persist failure is reported by persistTaskLocked, other errors are not"},
	"internal/tasks/scheduler_lifecycle.go:Scheduler.completeIdleStop:blank-call":          {1, "settleForeignStop persists through persistTaskLocked (reported) and is retried on the next lifecycle pass"},
	"internal/tasks/scheduler_lifecycle.go:Scheduler.releaseTaskLease:log-only":            {1, "follow-up: an unreleased lease expires by TTL, but the takeover waits for it; worth a WARNING finding"},
	"internal/tasks/scheduler_lifecycle.go:Scheduler.runTask:blank-call":                   {1, "convergeTask errors land on the task (state/last_error) and are retried by the lifecycle loop"},
	"internal/tasks/scheduler_lifecycle.go:Scheduler.settleForeignStop:log-only":           {1, persistReported},
	"internal/tasks/scheduler_task_ops.go:Scheduler.DeleteTask:log-only":                   {1, "follow-up: the task row is gone; an unreleased lease expires by TTL"},
	"internal/tasks/scheduler_task_ops.go:Scheduler.UpdateTask:blank-call":                 {1, "convergeTask errors land on the task (state/last_error) and are retried by the lifecycle loop"},
	"internal/tasks/scheduler_transitions.go:logTransitionPersistError:log-only":           {1, persistReported},
	"internal/tasks/segment_download.go:Scheduler.openLocalTaskSegment:blank-call":         {2, cleanupOnErrorPath + "; read-only file"},
	"internal/tasks/segment_download.go:Scheduler.openUploadedTaskSegment:blank-call":      {1, cleanupOnErrorPath + "; read-only object"},
	"internal/upload/s3_uploader.go:S3Uploader.OpenObject:blank-call":                      {2, cleanupOnErrorPath + "; read-only object"},
}
