// input: healthy dashboard mock with a sealed name and two open epochs for one index
// output: proof the task drawer shows and copies the one-path-per-index replay command
// pos: Playwright coverage for the Console replay set
// note: if this file changes, update this header and frontend/tests/e2e/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test.use({ permissions: ['clipboard-read', 'clipboard-write'] })

test('task drawer copies the highest-epoch replay command', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'healthy' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-100').click()
  await expect(page.getByTestId('task-drawer')).toBeVisible()

  const inventory = await page.getByTestId('file-disk-path').allTextContents()
  expect(inventory).toEqual([
    '/data/1/mysql-bin.000001',
    '/data/1/mysql-bin.000002',
    '/data/1/mysql-bin.000002.open.e1',
    '/data/1/mysql-bin.000002.open.e4',
  ])

  await expect(page.getByTestId('task-replay-hint')).toHaveText('MySQL mysqlbinlog')
  const command = page.getByTestId('task-replay-command')
  await expect(command).toHaveText(
    [
      'mysqlbinlog \\',
      '  /data/1/mysql-bin.000001 \\',
      '  /data/1/mysql-bin.000002.open.e4',
    ].join('\n'),
  )
  await expect(command).not.toContainText('.open.e1')
  await expect(command).not.toContainText('/data/1/mysql-bin.000002 \\')

  await page.getByTestId('task-replay-copy').click()
  await expect(page.getByText('已复制回放命令')).toBeVisible()
  const copied = await page.evaluate(() => navigator.clipboard.readText())
  expect(copied).toBe(
    [
      'mysqlbinlog \\',
      '  /data/1/mysql-bin.000001 \\',
      '  /data/1/mysql-bin.000002.open.e4',
    ].join('\n'),
  )
})
