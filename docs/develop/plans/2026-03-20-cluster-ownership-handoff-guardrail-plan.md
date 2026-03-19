# Cluster Ownership Handoff Guardrail Plan

**Goal:** Strengthen `internal/tasks` so ownership handoff does not leave the old owner in a long-lived healthy-running posture after lease transfer, and add tests that guard against practical double-execution drift at the scheduler layer.

**Scope:** `internal/tasks` only.

**Primary files likely involved:**
- `internal/tasks/scheduler_cluster_lease.go`
- `internal/tasks/scheduler_lifecycle.go`
- `internal/tasks/scheduler_observability.go`
- `internal/tasks/lease_test.go`
- `internal/tasks/README.md`

**Out of scope:**
- worker registration conflicts
- full cluster-wide split-brain prevention across `meta` / `app` / `api`
- lease SQL contract changes
- scheduler architecture refactor
- API or schema changes

## Problem Statement

The previous lease-loss stop-convergence hardening locked one side of the safety boundary: once a task knows it lost lease, it must converge to stop. The next dangerous gap is ownership handoff: when a new owner acquires lease, the old owner must not continue to present a healthy active execution posture for longer than the bounded stop-convergence window.

This ticket is not a full split-brain redesign. It is a scheduler-layer guardrail ticket focused on preventing practical drift after ownership handoff.

## Target Behavior

A task execution in cluster mode should satisfy the following during ownership handoff:

1. Once lease renew reports ownership loss, the old owner must enter fail-safe stop promptly.
2. The old owner must not continue normal progress/report semantics after ownership loss is known.
3. The old owner must not remain in a long-lived `RUNNING` state once lease ownership is gone.
4. The stop path triggered by ownership handoff must remain idempotent under repeated signals.
5. The scheduler-layer tests must make it hard to regress into “both sides appear healthy for too long,” even if this ticket does not simulate the full multi-process cluster.

## Phase Breakdown

### Phase 1: Ownership-Handoff Inventory

Inventory the current scheduler-layer ownership-handoff path:

1. How `renewLeaseLoop` treats `(false, nil)` renew results.
2. How `failSafeStopLocked` marks the task and cancels the runner.
3. When the task reaches final `STOPPED` and clears runtime ownership fields.
4. Which observability surfaces can still change during the stop window.

**Deliverable:** short inventory summary in implementation notes.

### Phase 2: Tests First

Add focused tests that simulate the old owner side of an ownership handoff.

Required coverage:

1. ownership-loss signal forces transition out of steady healthy-running posture.
2. old owner cannot keep updating replication progress after ownership loss is known.
3. ownership-loss stop path remains idempotent under repeated renew-loss signals.
4. runtime ownership fields are cleared after final stop convergence.
5. no test should rely on fragile sleeps longer than necessary or on external multi-process orchestration.

Preferred testing style:

- Use fake lease manager behavior to simulate handoff.
- Use controlled runner stubs to hold or release execution deliberately.
- Assert scheduler-visible state and progress surfaces, not internal log text.

### Phase 3: Minimum Fixes Only

If tests expose gaps, apply the minimum changes required.

Allowed adjustments:

1. tighten ownership-loss handling in lease renew loop
2. tighten scheduler observability gating after ownership loss / stop entry
3. tighten final state convergence or ownership-field cleanup if tests expose drift

Not allowed:

- refactoring unrelated scheduler paths
- changing persistence contracts
- introducing new config knobs unless strictly necessary

### Phase 4: Documentation Sync

If validated behavior changes or becomes more explicit, update:

- `internal/tasks/README.md`

Only document semantics that are explicitly covered by tests.

## Acceptance Criteria

This ticket is acceptable only if all of the following are true:

1. Ownership-loss path is explicitly covered by tests beyond basic stop convergence.
2. Old-owner progress/report semantics are suppressed once ownership loss is known.
3. Final ownership fields are cleared after stop convergence.
4. No API, schema, or cross-module behavior change is introduced.
5. The ticket remains scheduler-layer in scope and does not sprawl into full cluster redesign.

## Verification Gate

Minimum required verification:

1. `go test ./internal/tasks -count=1`
2. `go test ./...`
3. `go test -race ./internal/tasks ./internal/api ./internal/replication`
4. `go vet ./...`
5. `make e2e-quick`

## Risk Notes

1. The main risk is under-testing the handoff window and leaving room for practical double-execution drift.
2. The opposite risk is over-tightening and blocking legitimate stop convergence cleanup.
3. Multi-process split-brain is out of scope here; keep the ticket honest about scheduler-layer guarantees only.

## Delivery Expectations

The implementation delivery should include:

1. ownership-handoff behavior inventory summary
2. commit hash(es)
3. changed files summary
4. explicit list of added ownership-handoff tests
5. note whether code changes were required beyond tests
6. verification results
7. rollback command

