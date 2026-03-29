# Ops Console Visual Polish Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Refine the BinlogServer ops console into a more professional and restrained management UI without changing its core feature structure or API contracts.

**Architecture:** Keep the existing Vue 3 + Element Plus single-page console centered in `frontend/src/App.vue`. Implement the work as a visual polish pass in small, reviewable steps: first stabilize the visual token layer, then rebalance the page hierarchy, then refine high-frequency surfaces like the task table and detail drawer. Do not introduce new UI libraries or restructure the backend/frontend contract.

**Tech Stack:** Vue 3, Element Plus, Vite, Axios, vue-i18n, embedded frontend build into `internal/ui/static`

---

## Implementation Notes

- The current frontend is concentrated in `frontend/src/App.vue`, `frontend/src/api.js`, `frontend/src/main.js`, `frontend/src/locales/en.json`, and `frontend/src/locales/zh-CN.json`.
- `frontend/package.json` has `dev`, `build`, `preview`, and Playwright E2E scripts, but no unit-test or lint script. Do not invent a new frontend test stack in this task.
- `frontend/vite.config.js` shows the app is built with Vite and emitted under `/ui/` in production, so visual regressions should be validated with `npm run build` and targeted manual/E2E verification.
- Keep the change focused on visual polish. Avoid opportunistic refactors, API changes, or new feature work.
- If new copy is introduced, update both locale files instead of hardcoding strings.

---

### Task 1: Audit the current visual surfaces and define the token targets

**Files:**
- Modify: `docs/develop/plans/2026-03-28-ops-console-visual-polish-implementation.md`
- Check: `frontend/src/App.vue`
- Check: `frontend/src/locales/en.json`
- Check: `frontend/src/locales/zh-CN.json`

**Step 1: Read the page structure in `App.vue`**

Identify the exact sections that control:
- hero/header area
- KPI cards
- left navigation
- left filters / lookup cards
- task table
- detail drawer
- settings dialog
- scoped style tokens near the bottom of the file

**Step 2: Record the style clusters to touch**

In your working notes, list the selectors that will need to change first:
- `.page-shell`
- `.hero`
- `.metric-grid` / `.metric-card`
- `.nav-pane` / `.nav-item`
- `.panel-card`
- `.table-card`
- `.detail-panel`
- shared `:deep(...)` overrides for Element Plus controls

**Step 3: Verify localization scope**

Check whether any visible copy added by the polish work will require locale updates. If yes, note the exact keys to add in both locale files.

**Step 4: Confirm validation mode**

Record that the repo currently supports frontend build validation and Playwright E2E scripts, but no lightweight unit-test harness for view-only polish work.

**Step 5: Commit planning notes**

```bash
git add docs/develop/plans/2026-03-28-ops-console-visual-polish-implementation.md
git commit -m "docs: add ops console visual polish implementation plan"
```

---

### Task 2: Write the failing expectation for the visual baseline pass

**Files:**
- Modify: `frontend/src/App.vue`

**Step 1: Document the failing expectation in code comments or working notes**

Capture the desired outcomes before editing:
- background visuals are quieter
- cards feel flatter and more systematic
- hover states are restrained
- neutral areas rely on borders and spacing more than gradients and shadows

**Step 2: Run the current frontend locally**

Run:
```bash
cd frontend && npm run dev
```

Expected:
- the page renders successfully
- the background texture and card effects still feel slightly decorative and not yet fully “professional restrained”

**Step 3: Take a before snapshot for comparison**

Use the browser and capture the current first screen so you can compare after the token pass.

**Step 4: Do not implement yet**

Stop after confirming the current UI state and expected differences.

**Step 5: Commit the checkpoint**

```bash
git add frontend/src/App.vue
git commit -m "test: capture visual baseline expectations for ops console polish"
```

---

### Task 3: Implement the neutral visual token cleanup

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual visual verification in local UI

**Step 1: Reduce background decoration**

In `frontend/src/App.vue`, update the `.page-shell` rules to:
- simplify the background treatment
- reduce or remove visible decorative texture intensity
- keep enough separation between page background and white cards

**Step 2: Normalize neutral colors**

Update the CSS custom properties under `.page-shell` so that:
- background, surface, surface-soft, line, line-strong, text, and sub colors form a restrained neutral system
- neutral contrast is stable across cards, filters, table containers, and drawer panels

**Step 3: Flatten card styling**

Adjust `.panel-card`, `.metric-card`, `.detail-panel`, `.cluster-stat-cell`, `.source-cell`, and related surfaces so they rely more on border and subtle depth, less on noticeable lift or gradients.

**Step 4: Restrain hover behavior**

Update hover states for cards and nav items so they feel precise and calm:
- smaller lift or no lift
- lighter shadow increase
- cleaner border emphasis

**Step 5: Run a production build**

Run:
```bash
cd frontend && npm run build
```

Expected:
- PASS
- Vite build completes without CSS or Vue compilation errors

**Step 6: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: refine neutral visual tokens for ops console"
```

---

### Task 4: Rework the hero into a restrained console header

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Confirm the current hero structure**

Read the hero template section and identify:
- title
- description
- global action buttons

**Step 2: Reduce visual dominance of the hero**

In `frontend/src/App.vue`:
- lower the visual height of the hero area
- reduce title scale if needed
- make the description more concise in styling, not necessarily in content
- ensure action buttons align like a control bar, not a marketing CTA strip

**Step 3: Standardize button presence**

Update the shared button styling so:
- primary action remains clear
- secondary actions feel quieter and more system-like
- the button row looks consistent with the rest of the console

**Step 4: Verify the first-screen hierarchy**

Check locally that the first visual read is now:
1. page identity
2. actionable controls
3. KPI signal area

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: make ops console header more restrained"
```

---

### Task 5: Tighten KPI card hierarchy for a professional dashboard feel

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Review the current KPI order and classes**

Locate the KPI card template and confirm which cards are already marked as danger, warning, success, or neutral.

**Step 2: Make high-priority KPIs the only loud surfaces**

Update KPI card styling so:
- abnormal / failed / delayed remain clearly emphasized
- running / total / normal become visually quieter
- card title, icon, and number spacing are consistent across all six cards

**Step 3: Remove unnecessary visual variation**

Reduce extra gradients, inconsistent icon emphasis, and over-strong contrast on non-alert cards.

**Step 4: Verify quick scanning**

Check locally that the KPI strip now reads as:
- urgent signals first
- neutral monitoring second
- one consistent card language throughout

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: tighten ops KPI visual hierarchy"
```

---

### Task 6: Make the left navigation and utility rail quieter

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Confirm the nav pane and utility pane selectors**

Identify the CSS and template sections for:
- `.nav-pane`
- `.nav-item`
- `.nav-badge`
- `.left-pane`
- `.panel-card--secondary`

**Step 2: Reduce nav visual weight**

Adjust the navigation so that:
- the background is calmer
- active state is clear but not loud
- item density feels tool-like and professional
- badges remain readable but visually subordinate

**Step 3: Demote secondary utility surfaces**

Adjust left-side filter and source lookup cards so the main operational filter surface reads as primary and the lower-frequency utility surface reads as secondary.

**Step 4: Verify sidebar-to-content balance**

Check locally that the left rail supports navigation and filtering without competing with the main content area.

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: quiet the ops console navigation rail"
```

---

### Task 7: Polish the task table into a stable data surface

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Confirm the current table surface rules**

Review the selectors around `.table-card`, `.replication-cell`, `.action-row`, and the Element Plus table overrides.

**Step 2: Reduce table noise**

In `frontend/src/App.vue`:
- make the header row cleaner and more restrained
- normalize row density and cell padding if needed
- ensure hover states are subtle and precise
- keep action controls visually quieter than status and delay information

**Step 3: Stabilize status expression**

Ensure tags, icons, and auxiliary reason indicators all feel like one system rather than isolated accents.

**Step 4: Verify fast scan behavior**

Check locally that the eye naturally lands on:
- task name
- state
- lease risk
- replication status
- delay
before it lands on the action column

**Step 5: Run a production build**

Run:
```bash
cd frontend && npm run build
```

Expected:
- PASS

**Step 6: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: polish task table visual density"
```

---

### Task 8: Promote the detail drawer visually into the primary control surface

**Files:**
- Modify: `frontend/src/App.vue`
- Test: manual verification in local UI

**Step 1: Review the drawer structure**

Confirm the drawer currently contains:
- hero/header summary
- replication section
- basic info section
- lease/worker section
- files/upload section
- run history
- event timeline

**Step 2: Strengthen the top summary band**

Adjust the drawer hero and summary panel styling so:
- task identity is immediate
- current health and status are easy to parse
- action buttons feel grouped and intentional
- destructive actions remain clearly separated from routine controls

**Step 3: Harmonize section cards**

Update `.detail-panel`, `.detail-item`, `.detail-grid`, and related rules so the drawer reads as one coherent system with clear section hierarchy.

**Step 4: Verify task-handling flow**

Check locally that after opening a task drawer, a user can quickly understand:
- what task this is
- whether it is healthy
- what the next available action is
without being visually overwhelmed

**Step 5: Commit**

```bash
git add frontend/src/App.vue
git commit -m "feat: refine task drawer as primary control surface"
```

---

### Task 9: Align settings and shared control styles with the new visual language

**Files:**
- Modify: `frontend/src/App.vue`
- Modify: `frontend/src/locales/en.json` (if new labels or helper text are introduced)
- Modify: `frontend/src/locales/zh-CN.json` (if new labels or helper text are introduced)
- Test: manual verification in local UI

**Step 1: Review shared control overrides**

Inspect the `:deep(...)` overrides for:
- buttons
- inputs
- selects
- number inputs
- tags
- cards

**Step 2: Unify controls**

Adjust these shared styles so:
- controls match the restrained neutral system
- focus states remain accessible and clearly visible
- secondary buttons are consistent with the rest of the page
- settings modal feels like a system dialog, not a separate mini-design

**Step 3: Update locale files if needed**

If the visual polish introduces new visible labels, helper copy, or status wording, add matching keys to both locale files instead of hardcoding strings.

**Step 4: Verify accessibility basics**

Check locally that:
- focus states are still visible
- icon-only or low-text controls remain understandable
- contrast remains acceptable for primary text and state tags

**Step 5: Commit**

```bash
git add frontend/src/App.vue frontend/src/locales/en.json frontend/src/locales/zh-CN.json
git commit -m "feat: unify shared controls for ops console polish"
```

---

### Task 10: Run final verification and capture scope

**Files:**
- Modify: none
- Check: `frontend/src/App.vue`
- Check: `frontend/src/locales/en.json`
- Check: `frontend/src/locales/zh-CN.json`

**Step 1: Run production build**

Run:
```bash
cd frontend && npm run build
```

Expected:
- PASS

**Step 2: Run focused E2E smoke if available for the changed flows**

Run:
```bash
cd frontend && npm run test:e2e
```

Expected:
- PASS if the environment is ready
- If local browser/env requirements block the run, record the blocker rather than inventing a workaround

**Step 3: Manually verify the high-value paths**

Check:
- overview first screen
- KPI readability
- nav collapse / expand
- task list scanning
- opening detail drawer
- settings dialog
- locale switch still works

**Step 4: Review changed scope before handoff**

Run the required change-scope check for the repo before any future commit flow:
- `gitnexus_detect_changes({scope: "all", repo: "BinlogServer"})`

Confirm that the changes are limited to the expected frontend polish surface.

**Step 5: Commit**

```bash
git add frontend/src/App.vue frontend/src/locales/en.json frontend/src/locales/zh-CN.json
git commit -m "feat: polish ops console visual quality"
```
