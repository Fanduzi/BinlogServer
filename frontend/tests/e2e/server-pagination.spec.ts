// input: shared pagination mock scenario, browser dashboard requests, and task list controls
// output: regression coverage for server-side page transitions, state filtering, and global totals
// pos: Playwright E2E coverage for dashboard pagination contract consumption
// note: if this file changes, update this header and frontend/tests/e2e/README.md.

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('task list requests server pages and keeps the server total', async ({ page }) => {
  const dashboardRequests: URL[] = []
  page.on('request', (request) => {
    if (request.url().includes('/api/dashboard')) dashboardRequests.push(new URL(request.url()))
  })

  await registerMockRoutes(page, { scenario: 'pagination' })
  await page.goto('/#/tasks')

  await expect(page.getByTestId('task-row-001')).toBeVisible()
  await expect(page.getByTestId('task-row-021')).toHaveCount(0)
  await expect(page.locator('.el-pagination')).toContainText('25')
  await expect.poll(() => dashboardRequests.some((url) => url.searchParams.get('limit') === '20' && url.searchParams.get('offset') === '0')).toBe(true)

  await page.locator('.el-pagination .number').filter({ hasText: '2' }).click()
  await expect(page.getByTestId('task-row-021')).toBeVisible()
  await expect(page.getByTestId('task-row-001')).toHaveCount(0)
  await expect(page.locator('.el-pagination')).toContainText('25')
  await expect.poll(() => dashboardRequests.some((url) => url.searchParams.get('limit') === '20' && url.searchParams.get('offset') === '20')).toBe(true)

  await page.getByTestId('kpi-failed').click()
  await expect(page.getByTestId('task-row-002')).toBeVisible()
  await expect(page.getByTestId('task-row-001')).toHaveCount(0)
  await expect.poll(() => dashboardRequests.some((url) => url.searchParams.get('state') === 'FAILED' && url.searchParams.get('offset') === '0')).toBe(true)
  await expect(page.locator('.el-pagination')).toContainText('12')
})
