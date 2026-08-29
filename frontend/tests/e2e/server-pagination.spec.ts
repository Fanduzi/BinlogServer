// input: shared pagination mock scenario, browser dashboard requests, and task list controls
// output: regression coverage for server-side page transitions, state filtering, global totals, and current-page filter scope
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

test('current-page filters disclose scope and find matches on later server pages', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'pagination' })
  await page.goto('/#/tasks')

  await expect(page.getByTestId('filter-scope-note')).toContainText('任务状态由服务端全局筛选')
  await page.getByPlaceholder('按当前页任务 ID/名称搜索').fill('later-page-only-match')
  await expect(page.getByTestId('task-table-empty')).toBeVisible()
  await expect(page.getByTestId('filter-summary')).toContainText('当前页匹配：0')
  await expect(page.getByTestId('task-filter-count')).toContainText('当前页匹配：0')
  await expect(page.getByTestId('task-filter-count')).toContainText('全局总数：25')

  await page.locator('.el-pagination .number').filter({ hasText: '2' }).click()
  await expect(page.getByText('later-page-only-match')).toBeVisible()
  await expect(page.getByTestId('filter-summary')).toContainText('当前页匹配：1')
  await expect(page.getByTestId('task-filter-count')).toContainText('当前页匹配：1')
  await expect(page.getByTestId('task-filter-count')).toContainText('全局总数：25')
})
