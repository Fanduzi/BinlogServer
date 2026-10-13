// input: hash-route view state with shared mock routes in Playwright
// output: regression coverage for shareable URL deep-linking to specific operations views
// pos: frontend E2E route-state test for /#/tasks /#/sources /#/alerts navigation
// note: if this file changes, update this header and console/tests/e2e/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('hash URLs can deep-link to tasks/sources/alerts views', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'control-plane-down-worker-running' })

  await page.goto('/#/tasks')
  await expect(page.getByTestId('view-nav-tasks')).toHaveAttribute('data-active', 'true')
  await expect(page.getByTestId('task-row-601')).toBeVisible()
  await expect(page.locator('[data-testid^="worker-row-"]')).toHaveCount(0)

  await page.goto('/#/sources')
  await expect(page.getByTestId('view-nav-sources')).toHaveAttribute('data-active', 'true')
  await expect(page.getByTestId('source-cell').filter({ hasText: 'prod-mysql-order-01.internal:3306' })).toBeVisible()
  await expect(page.getByTestId('task-row-601')).toHaveCount(0)

  await page.goto('/#/alerts')
  await expect(page.getByTestId('view-nav-alerts')).toHaveAttribute('data-active', 'true')
  await expect(page.getByRole('main').getByText('异常与告警', { exact: true }).first()).toBeVisible()
  await expect(page.getByTestId('filter-scope-note')).toContainText('任务状态由服务端全局筛选')
})
