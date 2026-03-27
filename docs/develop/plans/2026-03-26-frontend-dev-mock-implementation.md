# Frontend Dev Mock Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in frontend development mock mode for Vite dev that reuses the existing Playwright mock scenarios and keeps the UI usable without a live backend.

**Architecture:** Extract the current Playwright-only mock routing into a shared scenario data module plus a pure request handler, then wire `frontend/src/api.js` to call that handler when `VITE_USE_MOCK=true`. Keep production and default dev behavior unchanged, and leave `App.vue` unaware of whether data came from real APIs or mocks.

**Tech Stack:** Vue 3, Vite, Axios, Playwright, shared frontend mock modules

---

## File Map

- Modify: `frontend/src/api.js`
- Create: `frontend/src/mocks/mock-data.ts` or `frontend/src/mocks/mock-data.js`
- Create: `frontend/src/mocks/mock-handler.ts` or `frontend/src/mocks/mock-handler.js`
- Modify: `frontend/tests/e2e/fixtures/mock-routes.ts`
- Possibly modify: `frontend/tests/e2e/fixtures/mock-data.ts` if kept as a thin re-export during migration
- Possibly modify: `frontend/README.md`
- Possibly create: `frontend/tests/e2e/dev-mock-smoke.spec.ts`

Implementation boundary:

- Do not add a UI mock toggle panel.
- Do not change production build behavior.
- Do not move API calls out of `frontend/src/api.js`.

---

### Task 1: Audit the current frontend mock assets and choose the shared module location

**Files:**
- Check: `frontend/tests/e2e/fixtures/mock-data.ts`
- Check: `frontend/tests/e2e/fixtures/mock-routes.ts`
- Check: `frontend/src/api.js`
- Modify: `docs/develop/plans/2026-03-26-frontend-dev-mock-implementation.md`

- [ ] **Step 1: Review existing fixture data**

Read `frontend/tests/e2e/fixtures/mock-data.ts` and list the currently supported scenarios and shapes:

- `empty`
- `healthy`
- `anomaly`
- `upload-failed`
- `auth-required`

- [ ] **Step 2: Review current route-only logic**

Read `frontend/tests/e2e/fixtures/mock-routes.ts` and identify which behaviors are pure data mapping versus Playwright-specific `route.fulfill()` integration.

- [ ] **Step 3: Confirm API entrypoints**

Read `frontend/src/api.js` and map the exported helper functions that will need mock support.

- [ ] **Step 4: Record the migration target**

Choose `frontend/src/mocks/` as the shared runtime location so both dev runtime and Playwright tests can import the same mock assets.

- [ ] **Step 5: Commit**

```bash
git add docs/develop/plans/2026-03-26-frontend-dev-mock-implementation.md
git commit -m "docs: add frontend dev mock implementation plan"
```

---

### Task 2: Write a failing test for the shared handler happy path

**Files:**
- Create or modify: `frontend/tests/e2e/fixtures/mock-routes.ts`
- Create: `frontend/src/mocks/mock-data.ts` or `frontend/src/mocks/mock-data.js`
- Create: `frontend/src/mocks/mock-handler.ts` or `frontend/src/mocks/mock-handler.js`
- Test: handler-focused frontend test file if a lightweight path exists, otherwise Playwright smoke spec

- [ ] **Step 1: Write the failing test**

Write a test that calls the future shared handler for:

- scenario: `healthy`
- request: `GET /api/dashboard`

Expected response:

- `status === 200`
- `body.summary.total === 1`
- `body.tasks.length === 1`

If no unit-test harness exists, create a minimal Playwright-facing test that imports the shared handler module directly instead of booting the whole UI.

- [ ] **Step 2: Run the test to verify it fails**

Run the narrowest possible command from `frontend/` for the new test.

Expected:

- FAIL because the shared handler module does not exist yet.

- [ ] **Step 3: Do not implement production code yet**

Confirm the failure is caused by the missing shared module, not by test syntax or tooling errors.

- [ ] **Step 4: Commit the red test**

```bash
git add frontend/tests
git commit -m "test: add failing test for shared frontend mock handler"
```

---

### Task 3: Extract shared mock data and handler with minimal parity

**Files:**
- Create: `frontend/src/mocks/mock-data.ts` or `frontend/src/mocks/mock-data.js`
- Create: `frontend/src/mocks/mock-handler.ts` or `frontend/src/mocks/mock-handler.js`
- Modify: `frontend/tests/e2e/fixtures/mock-data.ts`
- Modify: `frontend/tests/e2e/fixtures/mock-routes.ts`

- [ ] **Step 1: Move or re-export the scenario data**

Create a shared module under `frontend/src/mocks/` that exports:

- scenario names
- scenario data objects
- any helper builders needed to keep the file readable

Keep the E2E fixture file as either:

- a thin re-export, or
- deleted after all imports are updated

- [ ] **Step 2: Implement the minimal shared handler**

Create a pure function with an interface equivalent to:

```ts
export function handleMockRequest(input: {
  scenario: string
  method: string
  path: string
  query?: URLSearchParams
  body?: unknown
  state?: Record<string, unknown>
}): { status: number; body: unknown }
```

Required routing coverage for first green:

- `GET /api/dashboard`
- `GET /api/cluster/overview`
- `GET /api/workers`
- `GET /api/tasks/:id`
- `GET /api/tasks/:id/checkpoint`
- `GET /api/tasks/:id/replication`
- `GET /api/tasks/:id/lease`
- `GET /api/tasks/:id/runs`
- `GET /api/tasks/:id/events`
- `GET /api/tasks/:id/files`
- `POST /api/tasks/:id/files/retry-upload`
- `POST /api/tasks/:id/start`
- `POST /api/tasks/:id/stop`
- `DELETE /api/tasks/:id`

Unmocked routes must return an explicit error body.

- [ ] **Step 3: Update Playwright adapter**

Refactor `frontend/tests/e2e/fixtures/mock-routes.ts` so it:

- parses the request
- calls the shared handler
- fulfills the Playwright route with `{ status, body }`

Do not leave business routing duplicated in the Playwright layer.

- [ ] **Step 4: Run the red test again**

Expected:

- PASS for the `healthy` dashboard handler test.

- [ ] **Step 5: Run existing frontend E2E tests**

Run:

```bash
cd frontend && npm run test:e2e
```

Expected:

- Existing mock-backed tests still pass.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/mocks frontend/tests/e2e/fixtures
git commit -m "refactor: share frontend mock scenarios across dev and e2e"
```

---

### Task 4: Add a failing test for dev-mode API routing

**Files:**
- Modify: `frontend/src/api.js`
- Test: a focused frontend test or Playwright smoke spec

- [ ] **Step 1: Write the failing test**

Write a test for this behavior:

- when `VITE_USE_MOCK=true`
- and `VITE_MOCK_SCENARIO=healthy`
- calling `getDashboard()` returns mock data without requiring a backend server

The test should assert:

- one task is returned
- the response matches the `healthy` scenario

- [ ] **Step 2: Run the test to verify it fails**

Expected:

- FAIL because `api.js` still always uses Axios.

- [ ] **Step 3: Validate the failure reason**

Confirm the failure is the missing mock branch, not environment setup noise.

- [ ] **Step 4: Commit the red test**

```bash
git add frontend/tests frontend/src/api.js
git commit -m "test: add failing coverage for frontend dev mock api mode"
```

---

### Task 5: Wire `frontend/src/api.js` to the shared mock handler

**Files:**
- Modify: `frontend/src/api.js`
- Possibly create: `frontend/src/mocks/dev-client.ts` or `frontend/src/mocks/dev-client.js`

- [ ] **Step 1: Add the mock mode gate**

Read:

- `import.meta.env.VITE_USE_MOCK`
- `import.meta.env.VITE_MOCK_SCENARIO`

Rules:

- only enable mock mode when `VITE_USE_MOCK === "true"`
- default scenario to `healthy`

- [ ] **Step 2: Implement minimal API-layer mock dispatch**

For each existing API helper in `frontend/src/api.js`, preserve the exported function signature and route calls to:

- shared handler in mock mode
- Axios in normal mode

Keep 401 semantics aligned with the current response interceptor logic. If the mock handler returns `401`, the caller should still experience the same auth-required behavior.

- [ ] **Step 3: Handle request-local mutable state only where needed**

Implement only the minimal state needed for:

- `upload-failed` retry flow

Do not build a general in-memory backend.

- [ ] **Step 4: Run the dev-mode API test**

Expected:

- PASS with mock mode enabled

- [ ] **Step 5: Run frontend E2E regression**

Run:

```bash
cd frontend && npm run test:e2e
```

Expected:

- PASS

- [ ] **Step 6: Commit**

```bash
git add frontend/src/api.js frontend/src/mocks
git commit -m "feat: add opt-in frontend dev mock api mode"
```

---

### Task 6: Add explicit coverage for auth-required and retry-upload behavior

**Files:**
- Modify or create tests under: `frontend/tests/e2e`
- Possibly modify: `frontend/src/mocks/mock-handler.ts` or `frontend/src/mocks/mock-handler.js`

- [ ] **Step 1: Write a failing auth-required test**

Test behavior:

- in `auth-required` scenario
- `GET /api/dashboard` or related startup request returns `401`
- existing UI auth banner / settings guidance is triggered

- [ ] **Step 2: Write a failing retry-upload test if current coverage is insufficient**

Test behavior:

- in `upload-failed` scenario
- task files initially include `UPLOAD_FAILED`
- after `POST /retry-upload`, files are shown as `UPLOADED`

- [ ] **Step 3: Run tests to verify red**

Expected:

- fail for the specific behavior that is not yet preserved by the new shared handler

- [ ] **Step 4: Make the minimal handler adjustments**

Update shared mock state handling or response mapping only as needed to satisfy:

- `401` auth flow parity
- retry-upload file-state transition

- [ ] **Step 5: Run tests to verify green**

Run the narrow test commands first, then:

```bash
cd frontend && npm run test:e2e
```

Expected:

- PASS

- [ ] **Step 6: Commit**

```bash
git add frontend/tests/e2e frontend/src/mocks
git commit -m "test: preserve auth and retry-upload behavior in dev mock mode"
```

---

### Task 7: Document the developer workflow and verify local startup

**Files:**
- Modify: `frontend/README.md`
- Possibly modify: `frontend/src/README.md`

- [ ] **Step 1: Add mock-mode usage docs**

Document:

- how to start with real backend
- how to start with mock mode
- supported scenarios

Example command:

```bash
cd frontend
VITE_USE_MOCK=true VITE_MOCK_SCENARIO=healthy npm run dev
```

- [ ] **Step 2: Perform manual local verification**

Check these scenarios manually:

- `healthy`: home screen shows populated KPIs and worker list
- `empty`: empty-state placeholders render
- `auth-required`: auth-required banner/settings flow appears

- [ ] **Step 3: Run build validation**

Run:

```bash
cd frontend && npm run build
```

Expected:

- PASS

- [ ] **Step 4: Commit**

```bash
git add frontend/README.md frontend/src/README.md
git commit -m "docs: add frontend dev mock workflow"
```

---

## Self-Review Checklist

- Spec coverage:
  - shared mock source of truth: covered by Tasks 2-3
  - `api.js` dev-mode routing: covered by Tasks 4-5
  - auth / retry-upload parity: covered by Task 6
  - docs and local verification: covered by Task 7
- Placeholder scan:
  - no `TODO`/`TBD`
  - commands and files are explicit
- Type consistency:
  - `VITE_USE_MOCK`
  - `VITE_MOCK_SCENARIO`
  - shared handler under `frontend/src/mocks/`

## Execution Handoff

Plan complete and saved to `docs/develop/plans/2026-03-26-frontend-dev-mock-implementation.md`. Two execution options:

1. Subagent-Driven (recommended) - I dispatch a fresh subagent per task, review between tasks, fast iteration
2. Inline Execution - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
