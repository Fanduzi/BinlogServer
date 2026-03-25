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
  await page.goto('/')

  await page.getByTestId('task-detail-trigger-301').click()
  await expect(page.getByTestId('retry-upload-action')).toBeVisible()
  await page.getByTestId('retry-upload-action').click()
  await expect.poll(() => retried).toBe(true)
  await expect(page.getByTestId('file-upload-state-mysql-bin.000020')).toContainText('UPLOADED')

  await page.reload()
  await registerMockRoutes(page, { scenario: 'healthy' })
  await page.goto('/')
  await page.getByTestId('task-detail-trigger-100').click()
  await expect(page.getByTestId('retry-upload-action')).toHaveCount(0)
})
