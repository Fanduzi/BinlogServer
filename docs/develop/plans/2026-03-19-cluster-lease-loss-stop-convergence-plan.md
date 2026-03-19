# Cluster Lease-Loss Stop Convergence Plan

**Goal:** Harden cluster task execution so a running task converges to stop within a bounded time after losing lease ownership, while preserving the current grace-period behavior for transient metadata failures.

**Scope:** `internal/tasks` only.

**Primary files likely involved:**
- `internal/tasks/scheduler_cluster_lease.go`
- `internal/tasks/scheduler_lifecycle.go`
- `internal/tasks/lease_test.go`
- `internal/tasks/scheduler*_test.go` or new focused test file(s)

**Out of scope:**
- worker registration ownership conflicts
- full double-execution prevention across the whole cluster stack
- meta schema / SQL model changes
- API behavior or response format changes
- new deployment modes or role semantics

## Problem Statement

Current cluster correctness depends on the scheduler stopping promptly after lease loss. The dangerous failure mode is not a short degraded interval; it is a task continuing to run after lease ownership is no longer safe. This plan focuses on locking down that convergence path with tests first, then applying the minimum fix if any path is found to drift.

## Target Behavior

A running task in cluster mode must follow these rules:

1. Temporary renew failures may enter a degraded state during the configured grace window.
2. If lease safety is restored within grace, the task may continue running.
3. If grace expires without recovery, the scheduler must trigger fail-safe stop.
4. If lease ownership verification shows the task no longer owns the lease, the scheduler must trigger fail-safe stop without continuing normal execution.
5. After fail-safe stop begins, the task must not continue to report replication progress, append task events as if still healthy, or keep running indefinitely.
6. Fail-safe stop must be idempotent.

## Phase Breakdown

### Phase 1: Behavior Inventory

1. Read the current lease renew / degraded / stop path in:
   - `scheduler_cluster_lease.go`
   - `scheduler_lifecycle.go`
2. Identify the exact state transitions and stop triggers currently implemented.
3. Record the current stop-convergence mechanism:
   - renew failure path
   - grace timeout path
   - ownership-loss path
   - runner cancellation path

**Deliverable:** short inventory summary in the implementation notes / final delivery.

### Phase 2: Tests First

Add focused tests that lock the intended behavior before changing implementation.

Required coverage:

1. `renew` failure enters degraded state but does not immediately stop within grace.
2. degraded state exceeding grace triggers fail-safe stop.
3. ownership verification failure triggers fail-safe stop.
4. fail-safe stop is executed once even if multiple lease-loss signals arrive.
5. after stop is triggered, the task no longer continues normal progress reporting.
6. recovery within grace allows continued execution without false stop.

Testing guidance:

- Prefer deterministic fake lease manager / fake runner control over timing-heavy sleeps.
- Keep assertions centered on scheduler behavior, not on implementation-specific log text.
- Do not write brittle tests around third-party goroutine internals.

### Phase 3: Minimum Fixes Only

If Phase 2 exposes behavior gaps, apply the minimum code changes needed to make tests pass.

Allowed implementation adjustments:

1. Tighten degraded-state transition conditions.
2. Tighten grace-expiry handling.
3. Tighten fail-safe stop idempotency.
4. Ensure stop path prevents further normal task activity after lease loss.

Not allowed in this phase:

- architecture reshaping
- broad scheduler refactor
- API changes
- lease store contract changes

### Phase 4: Documentation Sync

If code or test changes alter the clearly documented cluster lease semantics, update:

- `internal/tasks/README.md`

Only document actual behavior that was validated by tests.

## Acceptance Criteria

The change is acceptable only if all of the following are true:

1. A task does not continue running indefinitely after lease loss.
2. Grace-window semantics remain intact for transient renew failures.
3. Ownership-loss path is explicitly covered by tests.
4. Fail-safe stop is idempotent under repeated lease-loss signals.
5. No API or schema behavior changes are introduced.
6. `internal/tasks` focused tests clearly describe the guarded lease-loss scenarios.

## Verification Gate

Minimum required verification:

1. `go test ./internal/tasks -count=1`
2. `go test ./...`
3. `go test -race ./internal/tasks ./internal/api ./internal/replication`
4. `go vet ./...`
5. `make e2e-quick`

## Risk Notes

1. The highest risk is silently allowing task activity to continue after lease loss.
2. The second risk is over-correcting and killing healthy tasks during short metadata blips.
3. Timing-sensitive tests can become flaky; prefer controllable fakes and bounded synchronization.

## Delivery Expectations

The implementation delivery should include:

1. test-gap summary
2. commit hash(es)
3. changed files summary
4. explicit list of newly added lease-loss tests
5. note whether implementation changes were needed beyond tests
6. verification results
7. rollback command

