// input: frontend api.js module plus dev env overrides and localStorage/window shims
// output: regression coverage for opt-in Vite dev mock mode through API helpers
// pos: Playwright-backed regression test for frontend API mock dispatch behavior
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from '@playwright/test'

function createStorage() {
  const store = new Map<string, string>()
  return {
    getItem(key: string) {
      return store.has(key) ? store.get(key)! : null
    },
    setItem(key: string, value: string) {
      store.set(key, value)
    },
    removeItem(key: string) {
      store.delete(key)
    },
  }
}

test('api helpers return mock dashboard data when dev mock mode is enabled', async () => {
  ;(globalThis as any).localStorage = createStorage()
  ;(globalThis as any).window = {
    dispatchEvent() {},
  }
  ;(globalThis as any).__BINLOG_DEV_ENV__ = {
    VITE_USE_MOCK: 'true',
    VITE_MOCK_SCENARIO: 'healthy',
  }

  const moduleUrl = `${new URL('../../src/api.js', import.meta.url).href}?t=${Date.now()}`
  const { getDashboard } = await import(moduleUrl)
  const dashboard = await getDashboard()

  expect(dashboard.summary.total).toBe(1)
  expect(dashboard.tasks).toHaveLength(1)
})
