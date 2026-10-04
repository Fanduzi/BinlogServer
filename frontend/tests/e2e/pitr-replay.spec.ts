// input: healthy dashboard mock whose selected replay names have fixed UTC event spans
// output: proof the task drawer builds a stop-only and a start+stop mysqlbinlog command, rejects a bad datetime, and downloads that window
// pos: Playwright coverage for the Console point-in-time restore drill
// note: if this file changes, update this header and frontend/tests/e2e/README.md

import { test, expect, type Download } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test.use({ permissions: ['clipboard-read', 'clipboard-write'] })

test('task drawer builds a point-in-time replay command', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'healthy' })
  await page.goto('/#/tasks')
  await page.getByTestId('task-detail-trigger-100').click()
  await expect(page.getByTestId('task-drawer')).toBeVisible()
  await expect(page.getByTestId('task-replay-command')).toContainText('mysql-bin.000002.open.e4')

  await page.getByTestId('task-pitr').scrollIntoViewIfNeeded()
  await page.getByTestId('task-pitr-stop').fill('2024-01-01 00:30:00')
  const stopRequest = page.waitForRequest(
    (req) => req.method() === 'GET' && req.url().includes('/api/tasks/100/replay?'),
  )
  await page.getByTestId('task-pitr-build').click()
  const stopURL = new URL((await stopRequest).url())
  expect(stopURL.searchParams.get('stop_datetime')).toBe('2024-01-01 00:30:00')
  expect(stopURL.searchParams.get('start_datetime')).toBeNull()
  const stopCommand = [
    'TZ=UTC mysqlbinlog \\',
    "  --stop-datetime='2024-01-01 00:30:00' \\",
    '  /data/1/mysql-bin.000001',
  ].join('\n')
  await expect(page.getByTestId('task-pitr-command')).toHaveText(stopCommand)
  await expect(page.getByTestId('task-replay-command')).toContainText('mysql-bin.000002.open.e4')

  await page.getByTestId('task-pitr-copy').click()
  await expect(page.getByText('已复制定点恢复命令')).toBeVisible()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(stopCommand)

  await page.getByTestId('task-pitr-start').fill('2024-01-01 00:30:00')
  await page.getByTestId('task-pitr-stop').fill('2024-01-01 12:00:00')
  await page.getByTestId('task-pitr-build').click()
  await expect(page.getByTestId('task-pitr-command')).toHaveText(
    [
      'TZ=UTC mysqlbinlog \\',
      "  --start-datetime='2024-01-01 00:30:00' \\",
      "  --stop-datetime='2024-01-01 12:00:00' \\",
      '  /data/1/mysql-bin.000001 \\',
      '  /data/1/mysql-bin.000002.open.e4',
    ].join('\n'),
  )

  await page.getByTestId('task-pitr-stop').fill('not-a-time')
  await page.getByTestId('task-pitr-start').fill('')
  await page.getByTestId('task-pitr-build').click()
  await expect(page.getByTestId('task-pitr-error')).toHaveText('invalid stop_datetime')

  await page.getByTestId('task-pitr-stop').fill('2020-01-01 00:00:00')
  await page.getByTestId('task-pitr-build').click()
  await expect(page.getByTestId('task-pitr-command')).toHaveText('这个时间窗口里没有分段。')

  await page.getByTestId('task-pitr-stop').fill('2024-01-01 00:30:00')
  const archiveRequest = page.waitForRequest(
    (req) => req.method() === 'GET' && req.url().includes('/api/tasks/100/replay/archive'),
  )
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByTestId('task-pitr-download').click(),
  ])
  const archiveURL = new URL((await archiveRequest).url())
  expect(archiveURL.searchParams.get('stop_datetime')).toBe('2024-01-01 00:30:00')
  expect(archiveURL.searchParams.has('limit')).toBe(false)
  expect(download.suggestedFilename()).toBe('task-100-replay.tar')
  expect(await readDownload(download)).toBe('mysql-bin.000001')
  await expect(page.getByText('定点恢复分段已下载')).toBeVisible()
})

async function readDownload(download: Download) {
  const stream = await download.createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk))
  }
  return Buffer.concat(chunks).toString('utf8')
}
