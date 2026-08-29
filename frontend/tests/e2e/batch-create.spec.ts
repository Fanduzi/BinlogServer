// input: shared batch API mock, batch-create form interactions, and browser request events
// output: regression coverage for local batch limits, safe ordered partial results, one batch request, and auto-start isolation
// pos: Playwright coverage for the frontend batch task creation contract
// note: if this file changes, update this header and frontend/tests/e2e/README.md.

import { test, expect } from '@playwright/test'
import { handleMockRequest } from '../../src/mocks/mock-handler.js'
import { registerMockRoutes } from './fixtures/mock-routes'

function validBatchItem(name: string, clusterKey: string, port: number) {
  return {
    name,
    cluster_key: clusterKey,
    source: {
      host: '127.0.0.1',
      port,
      user: 'repl',
      password: 'secret',
      flavor: 'mysql',
      server_id: 200001,
    },
    start: { mode: 'LATEST' },
    storage: { retention_days: 7 },
  }
}

test('shared batch mock keeps ordered partial results and redacts passwords', () => {
  const response = handleMockRequest({
    scenario: 'empty',
    method: 'POST',
    path: '/api/tasks/batch',
    body: {
      items: [
        validBatchItem('first', 'batch-first', 3306),
        { name: 'invalid', cluster_key: 'batch-invalid' },
        validBatchItem('third', 'batch-third', 3308),
      ],
    },
  })

  expect(response.status).toBe(200)
  expect(response.body).toHaveLength(3)
  expect(response.body[0]).toMatchObject({ index: 0, cluster_key: 'batch-first' })
  expect(response.body[0].task.source.password).toBe('')
  expect(response.body[1]).toMatchObject({
    index: 1,
    cluster_key: 'batch-invalid',
    error: { code: 'INVALID_REQUEST' },
  })
  expect(response.body[2]).toMatchObject({ index: 2, cluster_key: 'batch-third' })
})

test('batch preview rejects more than 100 valid rows without a batch request', async ({ page }) => {
  const batchRequests: unknown[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (request.method() === 'POST' && url.pathname === '/api/tasks/batch') batchRequests.push(request.postData())
  })

  await registerMockRoutes(page, { scenario: 'empty' })
  await page.goto('/#/tasks')
  await page.getByRole('banner').getByRole('button', { name: '批量创建' }).click()

  const dialog = page.getByRole('dialog', { name: '批量创建任务' })
  const lines = Array.from({ length: 101 }, (_, index) => 'task-' + index + ',127.0.0.1,' + (3306 + index)).join('\n')
  await dialog.getByRole('textbox', { name: 'host，例如 127.0.0.1' }).fill(lines)
  await dialog.getByRole('button', { name: '预览校验' }).click()

  await expect(page.locator('.el-message')).toContainText('100')
  await expect(dialog.getByRole('button', { name: '开始批量创建' })).toBeDisabled()
  expect(batchRequests).toHaveLength(0)
})

test('batch create isolates first auto-start failure and renders markup labels as text', async ({ page }) => {
  const batchRequests: Array<{ items: unknown[] }> = []
  const startRequests: string[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (request.method() === 'POST' && url.pathname === '/api/tasks/batch') {
      batchRequests.push(JSON.parse(request.postData() || '{}'))
    }
    if (request.method() === 'POST' && url.pathname.match(/^\/api\/tasks\/[^/]+\/start$/)) {
      startRequests.push(url.pathname)
    }
  })

  await registerMockRoutes(page, { scenario: 'empty', failFirstStart: true })
  await page.goto('/#/tasks')
  await page.getByRole('banner').getByRole('button', { name: '批量创建' }).click()

  const dialog = page.getByRole('dialog', { name: '批量创建任务' })
  await dialog.getByRole('textbox', { name: '复制用户' }).fill('repl')
  await dialog.getByRole('textbox', { name: '复制密码' }).fill('secret')
  const markupLabel = '<img src=x onerror=alert(1)>'
  await dialog.getByRole('textbox', { name: 'host，例如 127.0.0.1' }).fill(markupLabel + ',127.0.0.1,3306\nsecond,127.0.0.1,3307')
  await dialog.getByRole('button', { name: '预览校验' }).click()
  await expect(dialog.getByRole('button', { name: '开始批量创建' })).toBeEnabled()
  await dialog.locator('.el-switch').last().click()
  await dialog.getByRole('button', { name: '开始批量创建' }).click()

  await expect.poll(() => batchRequests.length).toBe(1)
  expect(batchRequests[0].items).toHaveLength(2)
  await expect.poll(() => startRequests.length).toBe(2)
  expect(startRequests).toEqual(['/api/tasks/1/start', '/api/tasks/2/start'])
  await expect(page.locator('.el-message').filter({ hasText: '成功 1 个，失败 1 个' })).toBeVisible()

  const errorDialog = page.getByRole('dialog').filter({ hasText: '批量创建失败明细' })
  await expect(errorDialog).toBeVisible()
  await expect(errorDialog.locator('.el-message-box__message')).toContainText(markupLabel)
  await expect(errorDialog.locator('.el-message-box__message img')).toHaveCount(0)
})
