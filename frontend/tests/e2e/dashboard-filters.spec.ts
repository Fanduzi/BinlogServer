import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('dashboard KPI filters update active state and support keyboard', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'anomaly' })
  await page.goto('/')

  const abnormalKpi = page.getByTestId('kpi-abnormal')
  await abnormalKpi.click()
  await expect(abnormalKpi).toHaveAttribute('data-active', 'true')
  await expect(page.getByTestId('filter-summary')).toContainText('1 个任务')
  await expect(page.getByTestId('task-row-201')).toBeVisible()

  const delayedKpi = page.getByTestId('kpi-delayed')
  await delayedKpi.focus()
  await page.keyboard.press('Enter')
  await expect(delayedKpi).toHaveAttribute('data-active', 'true')
  await expect(page.getByTestId('task-row-202')).toBeVisible()

  const failedKpi = page.getByTestId('kpi-failed')
  await failedKpi.focus()
  await page.keyboard.press('Space')
  await expect(failedKpi).toHaveAttribute('data-active', 'true')
  await expect(page.getByTestId('task-row-203')).toBeVisible()
})
