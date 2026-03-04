---
name: task-delivery-guardrails
description: Repository-level guardrails for task delivery quality, verification, and completion artifacts
---

# Task Delivery Guardrails

## When to use

Use this skill when dispatching implementation, refactor, bugfix, reliability, or integration tasks to an execution agent.
This is a general delivery guardrail skill, not limited to a specific phase plan.

## Responsibility boundary

Use this file as the single source of truth for:
1. Non-negotiable constraints
2. Verification gate selection (Minimal/Standard/Full)
3. Required delivery artifacts
4. Rollback validation policy

`docs/develop/plans/*phase-prompts.md` should only define phase-specific work items and phase-specific acceptance additions.
Do not duplicate full guardrail rules in phase prompt docs.

## Non-negotiable constraints

1. By default, do not modify `docs/guide/*` unless the task explicitly requires guide updates.
2. For behavior-changing tasks, explicitly document intended behavior changes and compatibility impact.
   If behavior change is not part of task scope, keep external behavior unchanged:
   - API status code semantics
   - core error semantics
   - task state machine semantics
3. Use isolated execution (unless explicitly waived by reviewer):
   - dedicated branch (recommended: `feat/*`, `fix/*`, `refactor/*`, `hardening/*`)
   - dedicated worktree
4. Keep changes minimal and task-scoped. No “bonus” unrelated refactors.

## Required verification gates

Use tiered verification based on task scope:

Default to **Standard**.
Escalate to **Full** when touching cross-module, runtime-critical, or release-boundary paths.
Downgrading to **Minimal** requires explicit reviewer approval.

### Minimal (docs/script/small scoped changes)
1. Targeted tests for changed area (or `go test ./...` if unsure)
2. `go vet ./...` (or targeted `go vet` package set when full scan is too heavy)

### Standard (default for code changes)
1. `go test ./...`
2. `go vet ./...`

### Full (high-risk, cross-module, release-boundary)
1. `go test ./...`
2. `go test -race ./internal/tasks ./internal/api ./internal/replication`
3. `go test -race <affected-packages>` (append additional affected packages beyond the default set)
4. `go vet ./...`
5. `make e2e-quick`

If task touches `sqlc`, also require:
1. `make sqlc-generate`
2. `make sqlc-verify`

## Required delivery artifacts

Execution agent must provide:

1. Commit hashes (ordered)
2. `git show --stat --name-only <hash-range>` summary
3. Code-change summary + test-change summary
4. Config compatibility notes (if config changed)
5. Rollback command (`git revert <commit>`)
6. Unresolved items
7. Branch/worktree proof:
   - `git branch --show-current`
   - `git worktree list`

If task touches `sqlc`, also include:

8. `make sqlc-generate` result summary
9. `make sqlc-verify` result summary

## Rollback validation

Rollback verification is mandatory for high-risk or release-boundary changes.
For low-risk scoped tasks, rollback command is still required, but full rollback verification may be waived by reviewer.

When rollback verification is required, validate with:

1. `go test ./...`
2. `go test -race ./internal/tasks ./internal/api ./internal/replication`
3. `go test -race <affected-packages>` (append additional affected packages beyond the default set)
4. `go vet ./...`
5. `make e2e-quick`

## Review output contract

After each review, always provide two blocks:

1. `Review Verdict` (for reviewer/user)
2. `Worker Prompt` (copy-paste instructions for execution agent)

### Review Verdict format

1. Merge decision: `approved` or `changes_required`
2. Findings ordered by severity (`P0`, `P1`, `P2`) with file references
3. Risk/impact statement (behavioral, compatibility, operational)
4. Optional nits/non-blocking suggestions

### Worker Prompt format

Must include:
1. Scope and constraints (`only this stage`, `no unrelated refactor`)
2. Mandatory fix list with target files and expected behavior
3. Explicit forbidden changes (if needed)
4. Verification commands (selected by gate tier)
5. Delivery artifacts required by this skill

Template:

```text
Apply repository skill: task-delivery-guardrails.
Work only on the requested scope; do not perform unrelated refactors.

Objective:
- <one-sentence goal>

Mandatory fixes:
1) <file>: <required change and expected behavior>
2) <file>: <required change and expected behavior>

Forbidden changes:
- <if any>

Verification gate:
- <Minimal|Standard|Full> (reason: <short reason>)

Run and report:
- <commands from selected gate>

Delivery artifacts:
1) commit hashes (ordered)
2) git show --stat --name-only <hash-range>
3) code-change summary + test-change summary
4) config compatibility notes (if config changed)
5) rollback command (git revert <commit>)
6) unresolved items
7) branch/worktree proof (`git branch --show-current`, `git worktree list`)
```

## Dispatch template

Paste this at the top of any execution dispatch prompt:

```text
Apply repository skill: task-delivery-guardrails.
Enforce all constraints, verification gates, and delivery artifacts defined in:
.agents/skills/task-delivery-guardrails/SKILL.md
```
