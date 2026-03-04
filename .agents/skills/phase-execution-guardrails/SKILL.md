---
name: phase-execution-guardrails
description: Repository-level guardrails for execution quality, verification, and delivery completeness
---

# Phase Execution Guardrails

## When to use

Use this skill when dispatching implementation, refactor, bugfix, reliability, or integration tasks to an execution agent.
This is a general delivery guardrail skill, not limited to a specific phase plan.

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
3. `go vet ./...`
4. `make e2e-quick`

## Dispatch template

Paste this at the top of any execution dispatch prompt:

```text
Apply repository skill: phase-execution-guardrails.
Enforce all constraints, verification gates, and delivery artifacts defined in:
.agents/skills/phase-execution-guardrails/SKILL.md
```
