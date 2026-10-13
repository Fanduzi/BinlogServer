// input: create and batch dialogs, shared mock routes, and the submitted task payload
// output: proof that Flavor is a mysql/mariadb select and a MariaDB choice is sent as flavor=mariadb
// pos: Playwright coverage for the Console flavor control
// note: if this file changes, update this header and console/tests/e2e/README.md.

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
  await page.getByTestId('create-task').click()

  const dialog = page.getByRole('dialog', { name: '新建任务' })
  const selected = dialog.getByTestId('task-form-flavor')
  await expect(selected).toHaveRole('combobox')
  await expect(dialog).toContainText('MariaDB 没有 @@server_uuid')
  await expect(selected).toHaveText('mysql')

  await selected.click()
  const list = page.getByRole('listbox')
  await expect(list.getByRole('option', { name: 'mysql' })).toBeVisible()
  await expect(list.getByRole('option', { name: 'mariadb' })).toBeVisible()
  await list.getByRole('option', { name: 'mariadb' }).click()
  await expect(selected).toHaveText('mariadb')

  await dialog.getByTestId('task-form-name').fill('prod-mariadb-01')
  await dialog.getByTestId('task-form-cluster-key').fill('prod-cluster-main')
  await dialog.getByTestId('task-form-password').fill('replpass')
  await page.screenshot({ path: 'test-results/flavor-select.png', fullPage: false })
  await dialog.getByTestId('task-form-submit').click()

  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0].source.flavor).toBe('mariadb')

  await page.getByTestId('batch-create-open').click()
  const batch = page.getByRole('dialog', { name: '批量创建任务' })
  const batchFlavor = batch.getByTestId('batch-flavor')
  await expect(batchFlavor).toHaveRole('combobox')
  await expect(batchFlavor).toHaveText('mysql')
  await expect(batch).toContainText('MariaDB 没有 @@server_uuid')
})
