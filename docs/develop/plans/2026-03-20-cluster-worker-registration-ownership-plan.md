# Cluster Worker Registration Ownership Plan

**Goal:** Harden worker-registration ownership semantics at the `internal/app` boundary so a cluster worker that loses registration ownership does not continue presenting itself as a valid active worker indefinitely, while preserving current role/mode behavior.

**Scope:** `internal/app` first, with minimal `internal/tasks` assertions only if needed for owner-identity consistency checks.

**Primary files likely involved:**
- `internal/app/app.go`
- `internal/app/smoke_test.go`
- `internal/app/README.md`
- optionally `internal/tasks/*` only if tests need a narrow owner-identity assertion

**Out of scope:**
- meta schema or SQL contract changes
- API behavior changes
- lease-store redesign
- full split-brain prevention across all cluster paths
- broad scheduler refactors

## Problem Statement

Task lease-loss handling is already guarded at the scheduler layer. The next adjacent boundary is worker registration ownership: the process-level cluster worker identity must not silently continue as a healthy active worker after registration ownership is lost or replaced.

This ticket is about process-level ownership semantics, not task-lease mechanics. The main risk is drift between:

1. worker registration ownership in `internal/app`
2. scheduler/task owner identity and heartbeat surfaces

## Target Behavior

A cluster worker process should satisfy the following:

1. if worker registration renew returns `ok=false`, the process must treat this as ownership loss
2. once registration ownership is lost, the process must not continue normal worker-serving posture indefinitely
3. role/mode semantics must remain unchanged for:
   - `cluster + worker`
   - `cluster + all-in-one`
4. worker identity presented through heartbeat / task-owner surfaces must remain coherent with the active process session
5. duplicate `worker_id` startup semantics must remain explicit and test-covered

## Phase Breakdown

### Phase 1: Registration Ownership Inventory

Inventory the current `internal/app` behavior:

1. initial acquire path for worker registration
2. renew loop and its `ok=false` ownership-loss handling
3. release path on shutdown
4. how `registrationOwnershipLost` affects `App.Run`
5. current role/mode tests for `worker` and `all-in-one`

**Deliverable:** short inventory summary in implementation notes.

### Phase 2: Tests First

Add focused tests around worker registration ownership behavior.

Required coverage:

1. duplicate `worker_id` startup is rejected when another active session already owns it
2. registration renew ownership loss is surfaced and causes the process path to stop behaving like a healthy worker
3. `cluster + worker` role remains control-plane-off even under registration-ownership scenarios
4. `cluster + all-in-one` role keeps current role semantics but does not ignore registration ownership loss
5. owner identity exposed to task/heartbeat surfaces remains coherent for the active session

Testing guidance:

- Prefer existing `internal/app/smoke_test.go` fake registration store hooks
- Do not expand into multi-process orchestration unless current test scaffolding already supports it cheaply
- Keep assertions on process-visible behavior, not on log text

### Phase 3: Minimum Fixes Only

If tests expose gaps, apply minimum code changes only.

Allowed adjustments:

1. tighten handling of `registrationOwnershipLost`
2. tighten stop/return path once ownership is lost
3. tighten owner-identity consistency checks between app wiring and scheduler startup, if tests expose mismatch

Not allowed:

- redesigning registration persistence
- changing meta store contracts
- adding unrelated cluster role behavior

### Phase 4: Documentation Sync

If behavior becomes more explicit, update:

- `internal/app/README.md`
- and only update `internal/tasks/README.md` if a validated owner-identity guarantee is documented there

## Acceptance Criteria

This ticket is acceptable only if all of the following are true:

1. duplicate `worker_id` semantics are explicitly covered by tests
2. registration ownership loss is explicitly covered by tests
3. role/mode semantics stay unchanged outside the intended ownership-loss boundary
4. no schema/API changes are introduced
5. the ticket remains centered on worker-registration ownership, not generalized cluster redesign

## Verification Gate

Minimum required verification:

1. `go test ./internal/app -count=1`
2. `go test ./...`
3. `go test -race ./internal/tasks ./internal/api ./internal/replication`
4. `go vet ./...`
5. `make e2e-quick`

## Risk Notes

1. The main risk is leaving worker registration ownership as a “soft warning” rather than an enforced process boundary.
2. The second risk is over-tightening and breaking valid `all-in-one` role behavior.
3. Keep this ticket honest: it is about app-level ownership semantics, not all possible double-execution paths.

## Delivery Expectations

The implementation delivery should include:

1. worker-registration ownership behavior inventory summary
2. commit hash(es)
3. changed files summary
4. explicit list of added tests
5. note whether code changes were needed beyond tests
6. verification results
7. rollback command

