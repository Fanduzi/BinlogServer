import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('dashboard empty shows zero KPIs and empty states', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'empty' })
  await page.goto('/')

  await expect(page.getByTestId('kpi-abnormal-value')).toHaveText('0')
  await expect(page.getByTestId('kpi-failed-value')).toHaveText('0')
  await expect(page.getByTestId('kpi-delayed-value')).toHaveText('0')
  await expect(page.getByTestId('task-table-empty')).toBeVisible()
  await expect(page.getByTestId('workers-empty')).toBeVisible()
  await expect(page.getByTestId('sources-empty')).toBeVisible()
})
