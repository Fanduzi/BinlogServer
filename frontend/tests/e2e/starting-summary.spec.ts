// input: STARTING dashboard mock scenario and rendered operations console
// output: regression coverage for separate starting/running KPI and source counts
// pos: Playwright E2E coverage for startup visibility at the first screen
// note: if this file changes, update this header and frontend/tests/e2e/README.md.

import { test, expect } from '@playwright/test'
import { createMockSession } from '../../src/mocks/mock-handler.js'
import { registerMockRoutes } from './fixtures/mock-routes'

test('starting tasks are visible without being counted as running', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'starting' })
  await page.goto('/')

  await expect(page.getByTestId('kpi-starting')).toBeVisible()
  await expect(page.getByTestId('kpi-starting-value')).toHaveText('1')
  await expect(page.getByTestId('kpi-running').locator('strong')).toHaveText('0')
  await expect(page.locator('.source-stats')).toContainText('启动 1')

  await page.getByTestId('kpi-starting').click()
  await expect(page.getByTestId('kpi-starting')).toHaveAttribute('data-active', 'true')
  await expect(page.getByTestId('filter-summary')).toContainText('1 个任务')
  await expect(page.getByTestId('task-row-150')).toBeVisible()
})

test('missing starting counter resets to zero after a nonzero response', async ({ page }) => {
  const session = createMockSession({ scenario: 'starting' })
  const dashboardResponses = [
    session.request({ method: 'GET', path: '/api/dashboard' }),
    session.request({ method: 'GET', path: '/api/dashboard' }),
  ]
  delete dashboardResponses[1].body.summary.starting
  let dashboardCall = 0

  await registerMockRoutes(page, { scenario: 'starting' })
  await page.route('**/api/dashboard', async (route) => {
    const response = dashboardResponses[Math.min(dashboardCall++, dashboardResponses.length - 1)]
    await route.fulfill({
      status: response.status,
      contentType: 'application/json',
      body: JSON.stringify(response.body),
    })
  })
  await page.goto('/')

  await expect(page.getByTestId('kpi-starting-value')).toHaveText('1')
  await page.getByRole('button', { name: '刷新' }).click()
  await expect(page.getByTestId('kpi-starting-value')).toHaveText('0')
})
