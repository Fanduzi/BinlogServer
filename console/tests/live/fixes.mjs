// Cutover fixes (#302 QA): deep link without a token, worker counts, the
// worker-count badge, and the batch preview strings.
import { chromium } from '@playwright/test'
import { BASE, CLASSIC } from './env.mjs'
const ok = (c, m) => { console.log((c ? 'PASS ' : 'FAIL ') + m); if (!c) process.exitCode = 1 }
const b = await chromium.launch()
const mk = async (token, locale = 'en') => {
  const ctx = await b.newContext({ viewport: { width: 1440, height: 1000 } })
  await ctx.addInitScript(([tk, lc]) => { if (tk && !sessionStorage.getItem('seeded')) { localStorage.setItem('binlog_server_token', tk); sessionStorage.setItem('seeded', '1') } localStorage.setItem('locale', lc) }, [token, locale])
  return ctx
}

// 1) deep link to a task with no token: only the auth dialog; after saving a token the drawer loads.
{
  const ctx = await mk('')
  const p = await ctx.newPage(); const errs = []; p.on('pageerror', (e) => errs.push(e.message))
  await p.goto(BASE + '/#/tasks/2'); await p.waitForSelector('[data-testid=settings-dialog]'); await p.waitForTimeout(1500)
  const body = await p.locator('body').innerText()
  ok(await p.getByTestId('task-drawer-error').count() === 0, 'no drawer error on 401')
  ok(await p.locator('[role=dialog]').count() === 1, 'only the auth dialog is open')
  ok(!/401|Unauthorized|status code/i.test(body), 'no raw 401 text')
  ok(await p.locator('[data-sonner-toast]').count() === 0, 'no error toast')
  await p.fill('[data-testid=settings-token-input]', 'ui302-test-token')
  await p.getByTestId('settings-dialog').getByRole('button', { name: 'Save' }).click()
  await p.waitForSelector('[data-testid=task-drawer-status]', { timeout: 10000 }).catch(() => {})
  ok(await p.getByTestId('task-drawer-status').count() === 1, 'drawer loads after the token is saved')
  ok(errs.length === 0, 'no page errors ' + errs.join(';'))
  await ctx.close()
}

// 2) workers with missing counts, and a registered worker that owns no task
// (server overview worker_count counts owners only: 1 here, 2 registered).
{
  const ctx = await mk('ui302-test-token')
  await ctx.route('**/api/cluster/overview', async (r) => { const res = await r.fetch(); const j = await res.json(); await r.fulfill({ response: res, json: { ...j, single_process: false, worker_count: 1 } }) })
  await ctx.route('**/api/workers', (r) => r.fulfill({ json: [
    { worker_id: 'worker-a', online: true, last_seen_at: new Date().toISOString(), task_count: 4, running: 4, leased: 4 },
    { worker_id: 'worker-b', online: false, last_seen_at: new Date().toISOString() },
  ] }))
  const p = await ctx.newPage()
  await p.goto(CLASSIC + '/#/workers'); await p.waitForTimeout(2500)
  const vueRows = await p.$$eval('.cluster-worker-item', (els) => els.map((e) => e.innerText.replace(/\s+/g, ' ').trim()))
  await p.goto(BASE + '/#/workers'); await p.waitForSelector('[data-testid=worker-row-worker-b]'); await p.waitForTimeout(500)
  const rowB = (await p.getByTestId('worker-row-worker-b').innerText()).replace(/\s+/g, ' ')
  console.log('classic worker-b:', vueRows[1]); console.log('console worker-b:', rowB)
  ok(!/undefined|NaN/.test(rowB) && rowB.includes('Tasks 0 / Running 0 / Leased 0'), 'missing counts read 0')
  const summary = (await p.getByTestId('cluster-worker-summary').innerText()).trim()
  const badge = (await p.getByTestId('view-nav-badge-workers').innerText()).trim()
  ok(summary === '2 Workers', 'cluster summary counts registered workers: ' + summary)
  ok(badge === '2', 'sidebar badge counts registered workers: ' + badge)
  await ctx.close()
}

// 3) batch preview strings (classic and Console), both locales
for (const [locale, want] of [['en', ['2 rows', 'Some rows failed']], ['zh-CN', ['共 2 行', '存在失败行']]]) {
  const ctx = await mk('ui302-test-token', locale)
  const p = await ctx.newPage()
  await p.goto(BASE + '/#/tasks'); await p.waitForSelector('[data-testid=task-row-2]')
  await p.getByTestId('batch-create-open').click(); await p.waitForSelector('[data-testid=batch-create]')
  await p.locator('[data-testid=batch-create] textarea').fill('10.9.9.1:3306\nbad-line')
  await p.getByRole('button', { name: /Preview|预览/ }).click(); await p.waitForTimeout(500)
  const status = (await p.getByTestId('batch-preview-status').innerText()).replace(/\s+/g, ' ')
  ok(want.every((w) => status.includes(w)) && !/batch\./.test(status), `${locale} batch status: ${status}`)
  await ctx.close()
}
await b.close()
