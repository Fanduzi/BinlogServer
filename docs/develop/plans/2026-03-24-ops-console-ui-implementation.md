# Ops Console UI Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Rework the existing Binlog Server frontend into a more operator-friendly console that prioritizes alerts, simplifies task scanning, and moves operational actions into the task detail drawer.

**Architecture:** Evolve the current single-page Vue + Element Plus UI incrementally instead of redesigning from scratch. Keep the existing API layer and page structure, but rebalance the information hierarchy in `App.vue`, then extract a few focused UI sections only if needed after the first-pass behavior is working.

**Tech Stack:** Vue 3, Element Plus, Axios, Vite, embedded static UI build under `internal/ui/static`

---

## Implementation Notes

- Current frontend source is concentrated in `frontend/src/App.vue`, `frontend/src/api.js`, and `frontend/src/utils/auth.js`.
- The embedded server UI is built from the Vite app into `internal/ui/static`.
- Keep changes YAGNI and incremental: improve layout, hierarchy, copy, and interaction flow before doing any component extraction.
- Follow TDD where feasible for logic extracted into small helpers. Do not invent a test harness if the repo does not already have frontend tests configured.
- If implementation introduces reusable computed helpers, prefer pure functions for easy testing.

---

### Task 1: Audit current frontend build and test entrypoints

**Files:**
- Modify: none
- Check: `frontend/package.json`
- Check: `frontend/vite.config.js`
- Check: `frontend/src/App.vue`

**Step 1: Read frontend package metadata**

Read `frontend/package.json` and verify available commands for dev, build, lint, or tests.

**Step 2: Confirm UI build path**

Read `frontend/vite.config.js` and confirm how production assets are emitted and how they sync into the backend-embedded UI.

**Step 3: Confirm single-page ownership**

Read `frontend/src/App.vue` and identify the sections that map to:
- header actions
- KPI cards
- left filters / source lookup
- task table
- detail drawer
- settings / auth interactions

**Step 4: Document constraints in working notes**

Record whether frontend tests exist. If none exist, proceed with focused manual verification plus build validation instead of creating a new test stack.

**Step 5: Commit**

```bash
git add docs/plans/2026-03-24-ops-console-ui-implementation.md
git commit -m "docs: add ops console UI implementation plan"
```

---

### Task 2: Reorder KPI hierarchy for operator-first scanning

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Write the failing expectation as working notes**

Capture the desired behavior:
- `异常`, `失败`, and `延迟` appear first
- alert-oriented cards are more visually prominent than neutral metrics
- clicking a high-priority KPI applies a matching task filter if existing state structure permits

**Step 2: Run current UI to confirm the problem**

Run:
```bash
cd frontend && npm run dev
```

Expected:
- KPI cards render, but operationally urgent metrics are not sufficiently prioritized.

**Step 3: Implement minimal KPI reorder and styling changes**

In `frontend/src/App.vue`:
- reorder the metrics so high-severity cards render first
- add a severity-based card class or mapping for high-priority cards
- keep neutral cards visually quieter
- if filter state is already centralized, wire KPI click handlers to set corresponding filters

**Step 4: Verify behavior manually**

Check:
- high-priority KPI cards are first in visual order
- alert cards are more prominent
- KPI click-to-filter works if implemented

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: prioritize alert metrics in dashboard"
```

---

### Task 3: Simplify task table row actions

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Define the desired failure state**

Current issue:
- each row exposes too many actions at once
- scanning rows is noisy
- users must visually parse buttons before status

**Step 2: Run UI and inspect one task row**

Expected:
- multiple action buttons compete with task status and lag information.

**Step 3: Implement minimal action reduction**

In `frontend/src/App.vue`:
- keep a single visible primary row action, preferably `详情`
- allow row click to open the drawer if that behavior already fits existing patterns
- move secondary actions such as start, stop, edit, delete, retry upload into the detail drawer or an overflow control

**Step 4: Verify behavior manually**

Check:
- row density is lower
- important columns are easier to scan
- users can still reach all actions from detail view

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: streamline task table actions"
```

---

### Task 4: Promote detail drawer to the primary operations surface

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Define the desired behavior**

The drawer should:
- show task identity and current state immediately
- group replication, checkpoint, lease, file, and event data clearly
- expose the main operations in one predictable location

**Step 2: Confirm current drawer structure**

Inspect the existing detail drawer in `frontend/src/App.vue` and identify where:
- task header is rendered
- buttons are rendered
- replication/checkpoint/lease/files/events are rendered

**Step 3: Implement minimal structural improvement**

In `frontend/src/App.vue`:
- add a clear drawer header summary with task name and status
- move task operations into the drawer header or top action section
- ensure dangerous actions are visually separated from routine actions
- reorganize detail cards into a more operator-friendly order:
  1. current state summary
  2. replication / checkpoint
  3. lease / worker context
  4. file/upload state
  5. event timeline / run history

**Step 4: Verify behavior manually**

Check:
- a user can open one drawer and understand task health quickly
- the main actions are available without hunting through the page
- the table no longer needs to carry all operations

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: make task drawer the primary control surface"
```

---

### Task 5: Reprioritize the left rail for everyday operations

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Define the desired behavior**

The left rail should prioritize daily operational filters over lower-frequency lookup tools.

**Step 2: Identify current state bindings**

Read the filter-related state and handlers in `frontend/src/App.vue` and map:
- task filters
- source lookup input/actions
- cluster or health filters

**Step 3: Implement minimal left-rail restructuring**

In `frontend/src/App.vue`:
- place common filters first
- demote source lookup into a collapsible or visually secondary section
- keep the most common operational controls visible without scrolling
- if health / lag / status filters already exist, make them more prominent

**Step 4: Verify behavior manually**

Check:
- status / health / lag filters are easy to reach first
- source lookup remains available but no longer competes with main workflows

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: prioritize operational filters in sidebar"
```

---

### Task 6: Standardize status semantics and operator copy

**Files:**
- Modify: `frontend/src/App.vue`
- Modify: `frontend/src/api.js`
- Possibly modify: `frontend/src/README.md`
- Test: manual verification in local UI

**Step 1: Write the failing expectation**

Problems to fix:
- mixed Chinese / English terminology creates polish drift
- status emphasis may rely too much on color
- auth-required dialog feels inconsistent with the rest of the app

**Step 2: Identify all operator-facing copy**

In `frontend/src/App.vue` and `frontend/src/api.js`, list:
- KPI labels
- card titles
- table headers
- drawer section titles
- button labels
- auth-required text

**Step 3: Implement minimal copy and semantics cleanup**

In `frontend/src/App.vue`:
- unify section titles and labels into one Chinese operator-facing style
- ensure status tags include clear wording and not just color differences
- add icon + text combinations where useful for warning and failure states

In `frontend/src/api.js`:
- replace abrupt browser-native auth messaging flow if possible within current architecture, or at minimum align the message copy with the in-app settings flow
- keep auth-required behavior safe and predictable

**Step 4: Verify behavior manually**

Check:
- labels are consistent across cards, table, drawer, and settings
- warning and failure states are recognizable without relying only on color
- auth-required guidance feels like part of the same product

**Step 5: Commit**

```bash
git add frontend/src/App.vue frontend/src/api.js frontend/src/README.md
git commit -m "refactor: unify operator copy and status semantics"
```

---

### Task 7: Extract pure helper logic only if duplication appears

**Files:**
- Create: `frontend/src/utils/dashboard.js` (only if needed)
- Modify: `frontend/src/App.vue`
- Test: `frontend/src/utils/dashboard.test.js` (only if a frontend test runner already exists)

**Step 1: Look for repeated severity / ordering logic**

If `App.vue` accumulates duplicate logic for:
- KPI ordering
- severity mapping
- row priority sorting
- status labels

then extract that logic into a pure helper file.

**Step 2: Write the failing test**

Only if the repo already supports frontend tests, add a small test such as:

```javascript
import { sortDashboardMetrics } from "./dashboard";

it("places alert metrics before neutral metrics", () => {
  const result = sortDashboardMetrics(["总任务", "异常", "延迟"]);
  expect(result).toEqual(["异常", "延迟", "总任务"]);
});
```

**Step 3: Run test to verify it fails**

Run the exact frontend test command discovered in Task 1.
Expected: FAIL before implementation.

**Step 4: Write minimal implementation**

Example shape:

```javascript
export function metricPriority(key) {
  const priorities = {
    异常: 0,
    失败: 1,
    延迟: 2,
    运行中: 3,
    总任务: 4,
    正常: 5,
  };
  return priorities[key] ?? 99;
}
```

**Step 5: Run tests to verify they pass**

Run the exact frontend test command.
Expected: PASS.

**Step 6: Commit**

```bash
git add frontend/src/App.vue frontend/src/utils/dashboard.js frontend/src/utils/dashboard.test.js
git commit -m "refactor: extract dashboard priority helpers"
```

If no frontend test runner exists, skip test-file creation and keep the logic inline unless duplication becomes meaningful.

---

### Task 8: Build the frontend and sync embedded assets

**Files:**
- Modify: generated files under `internal/ui/static/*`
- Check: `internal/ui/ui.go`

**Step 1: Run the production frontend build**

Run:
```bash
make ui-build
```

Expected:
- frontend bundles build successfully
- generated assets update under `internal/ui/static`

**Step 2: Verify embedded output changed as expected**

Check that updated static assets exist in `internal/ui/static` and that no unrelated generated artifacts were changed.

**Step 3: Smoke-check backend-served UI if practical**

Run:
```bash
go run ./cmd/binlog-server --config ./config.yaml
```

Then open the UI and confirm the new frontend renders through the embedded path.

**Step 4: Commit**

```bash
git add frontend/src/App.vue frontend/src/api.js frontend/src/utils/dashboard.js internal/ui/static internal/ui/ui.go
git commit -m "feat: ship operator-focused console UI improvements"
```

---

### Task 9: Run verification before handoff

**Files:**
- Modify: none
- Check: working tree diff

**Step 1: Run targeted validation**

Run the smallest relevant checks first:

```bash
make ui-build
```

If frontend tests exist, also run the exact frontend test command discovered earlier.

**Step 2: Run Go tests only if UI build path or embed wiring was touched unexpectedly**

Run:
```bash
go test ./...
```

Only if needed to validate embed/build integration or if backend files changed.

**Step 3: Review changed scope**

Run GitNexus change detection and confirm only expected frontend/UI embed flows changed.

**Step 4: Review diff manually**

Check that:
- KPI hierarchy changed as intended
- table actions were reduced
- drawer became the main operations surface
- sidebar prioritization reflects operator workflows
- copy and semantics are consistent

**Step 5: Commit or prepare for PR review**

```bash
git status
git diff -- frontend/src/App.vue frontend/src/api.js internal/ui/static
```

---

## Expected Final Scope

Most likely touched files:
- `frontend/src/App.vue`
- `frontend/src/api.js`
- optionally `frontend/src/utils/dashboard.js`
- optionally `frontend/src/README.md`
- generated build output under `internal/ui/static`

## Risks to Watch

- `frontend/src/App.vue` is large; avoid broad refactors before confirming behavior.
- Do not break existing create/edit/start/stop flows while reducing row actions.
- Be careful that KPI click-to-filter behavior does not conflict with existing filter state.
- Keep auth handling secure when adjusting the unauthorized-message UX.

## Handoff Reminder

Use `superpowers:verification-before-completion` before claiming the work is done.
