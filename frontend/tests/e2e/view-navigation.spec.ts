// input: shared frontend mock route registration and left-menu multi-view interactions
// output: regression coverage ensuring view switching reduces cross-section scrolling burden
// pos: Playwright scenario test for functional navigation split without UI style coupling
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('left menu switches between overview/tasks/sources/alerts views', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'control-plane-down-worker-running' })
  await page.goto('/')

  await expect(page.getByText('集群视图')).toBeVisible()
  await expect(page.locator('.source-cell', { hasText: 'prod-mysql-order-01.internal:3306' })).toBeVisible()
  await expect(page.getByTestId('task-row-601')).toHaveCount(0)

  await page.getByTestId('view-nav-collapse').click()
  await expect(page.getByTestId('view-nav-overview')).toHaveClass(/nav-item--active/)
  await page.getByTestId('view-nav-collapse').click()

  await page.getByTestId('view-nav-workers').click()
  await expect(page.locator('.cluster-worker-item', { hasText: 'worker-hot-standby' })).toBeVisible()

  await page.getByTestId('view-nav-tasks').click()
  await expect(page.getByTestId('task-row-601')).toBeVisible()
  await expect(page.locator('.cluster-worker-item')).toHaveCount(0)
  await expect(page.locator('.source-cell')).toHaveCount(0)

  await page.getByTestId('view-nav-sources').click()
  await expect(page.locator('.source-cell', { hasText: 'prod-mysql-order-01.internal:3306' })).toBeVisible()
  await expect(page.getByTestId('task-row-601')).toHaveCount(0)

  await page.getByTestId('view-nav-alerts').click()
  await expect(page.locator('.table-card .panel-title', { hasText: '异常与告警' })).toBeVisible()
  await expect(page.getByTestId('filter-scope-note')).toContainText('任务状态由服务端全局筛选')
})
