import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('401 responses trigger auth-required guidance and open settings dialog', async ({ page }) => {
  page.on('dialog', async (dialog) => {
    throw new Error(`unexpected native dialog: ${dialog.message()}`)
  })

  await registerMockRoutes(page, { scenario: 'auth-required' })
  await page.goto('/')

  await expect(page.getByTestId('auth-required-banner')).toBeVisible()
  await expect(page.getByTestId('auth-required-banner')).toContainText('接口认证已失效或尚未配置')
  await expect(page.getByTestId('settings-dialog')).toBeVisible()
  await expect(page.getByTestId('settings-token-input')).toBeVisible()
})
