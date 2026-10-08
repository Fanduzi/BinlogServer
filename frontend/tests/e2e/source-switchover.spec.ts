// input: source-switch mock with one continued RUNNING task and one stopped FAILED task
// output: proof the task drawer shows the server chain, the current server, and a continued banner that is distinct from a stopped SOURCE_SWITCHOVER
// pos: Playwright coverage for the operator-visible VIP source switch
// note: if this file changes, update this header and frontend/tests/e2e/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

const oldID = 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'
const newID = 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb'

test('continued switchover is visible and a stopped switchover says what to do next', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'source-switch' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-710').click()
  const drawer = page.getByTestId('task-drawer')
  await expect(drawer).toBeVisible()
  await expect(page.getByTestId('task-drawer-status')).toContainText('运行中')
  await expect(page.getByTestId('task-source-continued')).toBeVisible()
  await expect(page.getByTestId('task-source-continued')).toContainText('这份备份仍在复制')
  await expect(page.getByTestId('task-source-stopped')).toHaveCount(0)
  await expect(page.getByTestId('task-source-chain')).toContainText(oldID)
  await expect(page.getByTestId('task-source-chain')).toContainText(newID)
  await expect(page.getByTestId('task-source-current')).toHaveCount(1)
  await expect(page.getByTestId('task-source-switch')).toContainText(oldID)
  await expect(page.getByTestId('task-source-switch')).toContainText(newID)
  await expect(page.getByTestId('task-source-switch')).toContainText('mysql-bin.000001:154')
  await expect(page.getByTestId('task-source-switch')).toContainText(`${oldID}:1-3`)
  await expect(page.getByTestId('file-source-mysql-bin.000001')).toContainText(oldID)
  await expect(page.getByTestId('file-source-mysql-bin.000001')).toContainText('第 1 台')
  await expect(page.getByTestId(`file-source-${newID}.mysql-bin.000002`)).toContainText(newID)
  await expect(page.getByTestId(`file-source-${newID}.mysql-bin.000002`)).toContainText('当前')

  await page.keyboard.press('Escape')
  await expect(drawer).toBeHidden()
  await page.getByTestId('task-detail-trigger-711').click()
  await expect(page.getByTestId('task-drawer')).toBeVisible()
  await expect(page.getByTestId('task-source-continued')).toHaveCount(0)
  await expect(page.getByTestId('task-source-stopped')).toBeVisible()
  await expect(page.getByTestId('task-source-stopped')).toContainText('没有 GTID 集合')
  await expect(page.getByTestId('task-source-stopped-move')).toContainText(oldID)
  await expect(page.getByTestId('task-source-stopped-move')).toContainText(newID)
  await expect(page.getByTestId('task-source-next')).toContainText('新建任务')
  await expect(page.getByTestId('task-source-next')).toContainText('保留这份备份')
  await expect(page.getByTestId('task-source-next')).toContainText('.source-chain')
  await expect(page.getByTestId('task-source-current')).toContainText('当前')
  await expect(page.getByTestId('task-source-chain')).toContainText(oldID)
})
