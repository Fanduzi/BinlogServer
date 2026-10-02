// input: create and batch dialogs, shared mock routes, and the submitted task payload
// output: proof that Flavor is a mysql/mariadb select and a MariaDB choice is sent as flavor=mariadb
// pos: Playwright coverage for the Console flavor control
// note: if this file changes, update this header and frontend/tests/e2e/README.md.

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('flavor is a mysql/mariadb select and submit sends mariadb', async ({ page }) => {
  const posts: Array<Record<string, any>> = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (request.method() === 'POST' && url.pathname === '/api/tasks') {
      posts.push(JSON.parse(request.postData() || '{}'))
    }
  })

  await registerMockRoutes(page, { scenario: 'empty' })
  await page.goto('/#/tasks')
  await page.getByRole('banner').getByRole('button', { name: '新建任务' }).click()

  const dialog = page.getByRole('dialog', { name: '新建任务' })
  const flavorItem = dialog.locator('.el-form-item').filter({ hasText: 'Flavor' })
  await expect(flavorItem.getByRole('textbox')).toHaveCount(0)
  await expect(flavorItem).toContainText('MariaDB 没有 @@server_uuid')
  const selected = flavorItem.locator('.el-select__placeholder')
  await expect(selected).toHaveText('mysql')

  await flavorItem.locator('.el-select').click()
  const list = page.getByRole('listbox')
  await expect(list.getByRole('option', { name: 'mysql' })).toBeVisible()
  await expect(list.getByRole('option', { name: 'mariadb' })).toBeVisible()
  await list.getByRole('option', { name: 'mariadb' }).click()
  await expect(selected).toHaveText('mariadb')

  await dialog.getByRole('textbox', { name: '任务名' }).fill('prod-mariadb-01')
  await dialog.getByRole('textbox', { name: 'Cluster Key' }).fill('prod-cluster-main')
  await dialog.getByRole('textbox', { name: '密码' }).fill('replpass')
  await page.screenshot({ path: '/opt/cursor/artifacts/flavor-select.png', fullPage: false })
  await dialog.getByRole('button', { name: '保存' }).click()

  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0].source.flavor).toBe('mariadb')

  await page.getByRole('banner').getByRole('button', { name: '批量创建' }).click()
  const batch = page.getByRole('dialog', { name: '批量创建任务' })
  const batchFlavor = batch.locator('.el-form-item').filter({ hasText: 'Flavor' })
  await expect(batchFlavor.getByRole('textbox')).toHaveCount(0)
  await expect(batchFlavor.locator('.el-select__placeholder')).toHaveText('mysql')
  await expect(batchFlavor).toContainText('MariaDB 没有 @@server_uuid')
})
