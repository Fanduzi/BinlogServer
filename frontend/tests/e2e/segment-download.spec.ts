// input: healthy dashboard mock with a sealed name and two open epochs for one index
// output: proof each files-table row downloads that on-disk basename, including the current open epoch, while the replay command stays one path per index
// pos: Playwright coverage for Console segment download
// note: if this file changes, update this header and frontend/tests/e2e/README.md

import { test, expect, type Download } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('task files table downloads the listed basename, including an open epoch', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'healthy' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-100').click()
  await expect(page.getByTestId('task-drawer')).toBeVisible()

  const names = [
    'mysql-bin.000001',
    'mysql-bin.000002',
    'mysql-bin.000002.open.e1',
    'mysql-bin.000002.open.e4',
  ]
  for (const name of names) {
    await expect(page.getByTestId(`file-download-${name}`)).toBeVisible()
  }

  await expect(page.getByTestId('task-replay-command')).toHaveText(
    [
      'mysqlbinlog \\',
      '  /data/1/mysql-bin.000001 \\',
      '  /data/1/mysql-bin.000002.open.e4',
    ].join('\n'),
  )
  await expect(page.getByTestId('task-replay-copy')).toBeEnabled()

  const requestPromise = page.waitForRequest(
    (req) => req.method() === 'GET' && req.url().includes('/api/tasks/100/files/mysql-bin.000002.open.e1'),
  )
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByTestId('file-download-mysql-bin.000002.open.e1').click(),
  ])
  const request = await requestPromise
  expect(new URL(request.url()).pathname).toBe('/api/tasks/100/files/mysql-bin.000002.open.e1')
  expect(download.suggestedFilename()).toBe('mysql-bin.000002.open.e1')
  expect(await readDownload(download)).toBe('segment-bytes:mysql-bin.000002.open.e1')
  await expect(page.getByText('分段已下载')).toBeVisible()

  const sealedPromise = page.waitForEvent('download')
  await page.getByTestId('file-download-mysql-bin.000002').click()
  const sealed = await sealedPromise
  expect(sealed.suggestedFilename()).toBe('mysql-bin.000002')
  expect(await readDownload(sealed)).toBe('segment-bytes:mysql-bin.000002')
})

async function readDownload(download: Download) {
  const stream = await download.createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk))
  }
  return Buffer.concat(chunks).toString('utf8')
}
