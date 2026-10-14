# 0346: fencing the binlog dump against the task lease

Status: implemented in the PR that closes #346.

## What failed

The control-loop e2e "dump fence" case (#255) kept seeing `max_dumps=2`. CI
log of job 115727555795, task 3:

1. Worker B owns the task at epoch 1. Its old dump (conn 193) sits behind a
   held TCP path, so `killLeftover` fails with i/o timeouts for about 13 s.
2. B's lease expires during those KILL attempts. Nothing renews it: the renew
   loop only runs while a dump runs, and a same-owner resume does not call
   Acquire. A same-owner Acquire on an unexpired row does not extend it either.
3. 07:17:19.914 the KILL is confirmed. 07:17:19.930 B opens a dump on an
   expired epoch-1 lease.
4. 07:17:20.556 worker A claims the expired lease at epoch 2 and opens a
   second dump at 07:17:20.574. Two dumps.
5. 07:17:24.9 B's renew fails ("lease lost"). `failSafeStopLocked` persists
   STOPPING and then STOPPED over A's row. A reads STOPPED and cancels its own
   dump at 07:17:26. The task ends STOPPED with no dump.

So there are two defects: a dump opened without proof of a live lease, and a
stale worker that wrote the shared row after it lost the lease.

## Rules

R1. A worker opens a dump only after it has proved, after any leftover KILL,
    that it holds an unexpired lease at its epoch. The proof is
    `Verify(owner, epoch)` (unexpired) followed by `Renew(owner, epoch)`,
    which extends the TTL from now. KILL time therefore counts against the
    lease: if KILL ran past the TTL, Verify fails and no dump opens.
    Renew is a compare-and-set on (owner, epoch), so the lease cannot change
    hands between the two calls without Renew failing.
R2. If the proof fails: if the lease is free or expired and still ours, the
    worker reclaims it through Acquire and keeps the epoch only when nobody
    else took it. If another worker holds it, or the epoch moved, Start
    returns `ErrLeaseNotAcquired`, opens nothing, and writes nothing to the
    task row; the in-memory copy drops ownership.
R3. A worker whose Renew reports the lease lost cancels its dump on that tick
    (at most one renew interval) and yields in memory only, like
    `ErrLeaseHandoff`: no STOPPING/STOPPED/FAILED write, no release of a
    newer epoch. The row belongs to the new owner.
R4. The epoch is the fencing token. Writes that matter already check it:
    seal-time `Verify` in the runner (`ErrLeaseEpochMismatch`), the
    pending-dump marker's epoch (`heldDumpAbandoned`), and Release/Renew,
    which are conditional on (owner, epoch). R3 removes the one unconditional
    task-row write on the lease-lost path. Making every `UpsertTask`
    conditional on epoch is a larger schema-level change and is left as a
    follow-up; it is not needed for #346.

## Not changed

- The KILL itself is not bounded by a timer here. R1 makes a long KILL safe:
  it only costs a retry.
- Lease semantics in `task_leases` and `MemoryLease` are unchanged.

## Tests

`internal/tasks/dump_lease_fence_test.go`, deterministic with a test clock:

- KILL longer than the TTL while worker A claims epoch 2: B opens no dump
  (same-owner resume and Acquire reclaim).
- KILL that uses most of the TTL: the lease is renewed before the dump, so A
  cannot claim it right after.
- Lease lost while the dump runs: B stops the dump and A's row is untouched.

The control-loop e2e runs 20 times in a loop in CI for this PR.
