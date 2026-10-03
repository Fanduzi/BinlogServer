// input: leftover and catalog task rows, the task drawer, and the shared mock adopt route
// output: proof that Adopt posts cluster_key and source, stays STOPPED without showing the password, and that catalog Edit still uses PUT
// pos: Playwright coverage for the Console adopt action
// note: if this file changes, update this header and frontend/tests/e2e/README.md.

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

const PASSWORD = 's3cret-adopt'

function trackTaskWrites(page) {
  const writes: Array<{ method: string; path: string; body: Record<string, any> | null }> = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (!url.pathname.startsWith('/api/tasks/')) return
    if (!['POST', 'PUT'].includes(request.method())) return
    const raw = request.postData()
    writes.push({
      method: request.method(),
      path: url.pathname,
      body: raw ? JSON.parse(raw) : null,
    })
  })
  return writes
}

async function fillAdopt(page, startMode?: string) {
  await page.getByTestId('task-detail-trigger-4').click()
  await expect(page.getByTestId('task-action-adopt')).toBeVisible()
  await expect(page.getByTestId('task-action-edit')).toHaveCount(0)
  await expect(page.getByTestId('task-action-start')).toBeVisible()
  await page.getByTestId('task-action-adopt').click()

  const dialog = page.getByRole('dialog', { name: '认领 #4' })
  await expect(dialog).toBeVisible()
  const startItem = dialog.locator('.el-form-item').filter({ hasText: '起点模式' })
  await expect(startItem).toContainText('默认（最高分段末尾）')
  if (startMode) {
    await startItem.locator('.el-select').click()
    await page.getByRole('listbox').getByRole('option', { name: startMode, exact: true }).click()
  }
  await dialog.getByRole('textbox', { name: 'Cluster Key' }).fill('adopted-4')
  await dialog.getByRole('textbox', { name: '主机' }).fill('10.0.0.8')
  await dialog.getByRole('textbox', { name: '用户' }).fill('repl')
  await dialog.getByRole('textbox', { name: '密码' }).fill(PASSWORD)
  return dialog
}

test('adopt a leftover directory from the console and start it afterwards', async ({ page }) => {
  const writes = trackTaskWrites(page)
  await registerMockRoutes(page, { scenario: 'disk-leftover' })
  await page.goto('/#/tasks')

  const dialog = await fillAdopt(page)
  await dialog.getByTestId('task-form-submit').click()

  await expect.poll(() => writes.filter((call) => call.path === '/api/tasks/4/adopt')).toHaveLength(1)
  const adopt = writes.find((call) => call.path === '/api/tasks/4/adopt')
  expect(adopt?.method).toBe('POST')
  expect(adopt?.body).toMatchObject({
    cluster_key: 'adopted-4',
    source: {
      host: '10.0.0.8',
      port: 3306,
      user: 'repl',
      password: PASSWORD,
      flavor: 'mysql',
    },
  })
  expect(adopt?.body).not.toHaveProperty('start')
  expect(writes.filter((call) => call.path.endsWith('/start'))).toHaveLength(0)
  expect(writes.filter((call) => call.method === 'PUT')).toHaveLength(0)

  const drawer = page.getByTestId('task-drawer')
  await expect(page.getByTestId('task-drawer-status')).toContainText('已停止')
  await expect(page.getByTestId('task-drawer-source')).toContainText('10.0.0.8:3306')
  await expect(drawer).toContainText('adopted-4')
  await expect(drawer).not.toContainText(PASSWORD)
  await expect(page.getByTestId('task-action-adopt')).toHaveCount(0)
  await expect(page.getByTestId('task-action-edit')).toBeVisible()

  await page.getByTestId('task-action-start').click()
  await expect.poll(() => writes.filter((call) => call.path === '/api/tasks/4/start')).toHaveLength(1)
  await expect(page.getByTestId('task-drawer-status')).toContainText('运行中')
  await expect(drawer).not.toContainText(PASSWORD)
})

test('an explicit adopt start mode is sent and still does not start replication', async ({ page }) => {
  const writes = trackTaskWrites(page)
  await registerMockRoutes(page, { scenario: 'disk-leftover' })
  await page.goto('/#/tasks')

  const dialog = await fillAdopt(page, 'LATEST')
  await dialog.getByTestId('task-form-submit').click()

  await expect.poll(() => writes.filter((call) => call.path === '/api/tasks/4/adopt')).toHaveLength(1)
  const adopt = writes.find((call) => call.path === '/api/tasks/4/adopt')
  expect(adopt?.body?.start).toEqual({ mode: 'LATEST' })
  expect(writes.filter((call) => call.path.endsWith('/start'))).toHaveLength(0)
  await expect(page.getByTestId('task-drawer-status')).toContainText('已停止')
  await expect(page.getByTestId('task-drawer')).not.toContainText(PASSWORD)
})

test('a catalog task edit still uses PUT', async ({ page }) => {
  const writes = trackTaskWrites(page)
  await registerMockRoutes(page, { scenario: 'disk-leftover' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-5').click()
  await expect(page.getByTestId('task-action-edit')).toBeVisible()
  await expect(page.getByTestId('task-action-adopt')).toHaveCount(0)
  await page.getByTestId('task-action-edit').click()

  const dialog = page.getByRole('dialog', { name: '编辑 #5' })
  await dialog.getByRole('textbox', { name: '任务名' }).fill('catalog-renamed')
  await dialog.getByTestId('task-form-submit').click()

  await expect.poll(() => writes.filter((call) => call.method === 'PUT' && call.path === '/api/tasks/5')).toHaveLength(1)
  expect(writes.find((call) => call.path === '/api/tasks/5')?.body).toMatchObject({
    name: 'catalog-renamed',
    cluster_key: 'catalog-5',
  })
  expect(writes.filter((call) => call.path.endsWith('/adopt'))).toHaveLength(0)
})
