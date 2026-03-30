// input: upload-failed mock scenario plus retry callback spy
// output: regression coverage for retry-upload action visibility and post-retry refresh
// pos: Playwright E2E coverage for task drawer upload recovery flow in task workspace
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('retry upload entry appears only for upload failed files and refreshes state after success', async ({ page }) => {
  let retried = false
  await registerMockRoutes(page, {
    scenario: 'upload-failed',
    onRetryUpload: () => {
      retried = true
    },
  })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-301').click()
  await expect(page.getByTestId('retry-upload-action')).toBeVisible()
  await page.getByTestId('retry-upload-action').click()
  await expect.poll(() => retried).toBe(true)
  await expect(page.getByTestId('file-upload-state-mysql-bin.000020')).toContainText('UPLOADED')

  const healthyPage = await page.context().newPage()
  await registerMockRoutes(healthyPage, { scenario: 'healthy' })
  await healthyPage.goto('/#/tasks')
  await healthyPage.getByTestId('task-detail-trigger-100').click()
  await expect(healthyPage.getByTestId('retry-upload-action')).toHaveCount(0)
  await healthyPage.close()
})
