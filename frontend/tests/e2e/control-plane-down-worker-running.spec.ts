// input: shared frontend mock route registration with the control-plane-down-worker-running scenario
// output: regression coverage for worker-continues-running view during control-plane disruption
// pos: Playwright-backed scenario test for control-plane resilience visibility
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('control-plane down scenario still shows worker-owned running task context', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'control-plane-down-worker-running' })
  await page.goto('/')

  await expect(page.getByText('5 个 Worker')).toBeVisible()
  await page.getByTestId('view-nav-workers').click()
  await expect(page.locator('.cluster-worker-item', { hasText: 'worker-hot-standby' })).toBeVisible()
  await page.getByTestId('view-nav-sources').click()
  await expect(page.locator('.source-cell', { hasText: 'prod-mysql-order-01.internal:3306' })).toBeVisible()
  await page.getByTestId('view-nav-tasks').click()
  await expect(page.getByTestId('task-row-601')).toBeVisible()
  await expect(page.getByTestId('task-row-612')).toBeVisible()
  await page.getByTestId('task-detail-trigger-601').click()
  await expect(page.getByTestId('task-drawer-worker')).toContainText('worker-hot-standby')
  await expect(page.getByTestId('task-drawer-checkpoint')).toContainText('mysql-bin.000188:92001')
})
