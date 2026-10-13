import { chromium } from '@playwright/test'
import { BASE, CLASSIC, OUT } from './env.mjs'

const ok = (c, m) => { console.log((c ? 'PASS ' : 'FAIL ') + m); if (!c) process.exitCode = 1 }
const b = await chromium.launch()
const mk = async (token) => { const ctx = await b.newContext({ viewport: { width: 1440, height: 1000 } }); await ctx.addInitScript((tk) => { if (tk) localStorage.setItem('binlog_server_token', tk); localStorage.setItem('locale', 'en') }, token); return ctx }
// 1) KPI jumps vs Vue
{ const ctx = await mk('ui302-test-token'); const p = await ctx.newPage(); const calls = []; p.on('request', r => { const u = new URL(r.url()); if (u.pathname.startsWith('/api/dashboard')) calls.push(u.search) })
  for (const k of ['abnormal', 'failed', 'delayed', 'starting', 'running', 'all', 'normal']) {
    await p.goto(CLASSIC + '/#/overview'); await p.waitForTimeout(1500)
    const vc = p.getByTestId('kpi-' + k); let vue = 'n/a'
    if (await vc.count()) { calls.length = 0; await vc.first().click(); await p.waitForTimeout(1200); vue = `${new URL(p.url()).hash} rows=${await p.locator('.el-table__body-wrapper tbody tr').count()} q=${calls.at(-1) || ''}` }
    await p.goto(BASE + '/#/overview'); await p.waitForSelector('[data-testid=cluster-panel]'); await p.waitForTimeout(800)
    calls.length = 0; await p.getByTestId('kpi-' + k).first().click(); await p.waitForTimeout(1200)
    const re = `${new URL(p.url()).hash} rows=${await p.locator('tbody tr').count()} q=${calls.at(-1) || ''}`
    console.log(`kpi-${k}: vue[${vue}] react[${re}]`)
    if (vue !== 'n/a') ok(vue.split(' ')[1] === re.split(' ')[1], `kpi-${k} same row count as Vue`)
    ok(re.startsWith('#/tasks'), `kpi-${k} jumps to #/tasks`)
  }
  await ctx.close() }
// 2) 401 shows only the auth dialog
{ const ctx = await mk(''); const p = await ctx.newPage(); const errs = []; p.on('pageerror', e => errs.push(e.message))
  for (const r of ['#/', '#/overview', '#/tasks', '#/alerts', '#/sources', '#/workers', '#/tasks/2']) {
    await p.goto(BASE + '/' + r); await p.waitForTimeout(2000)
    const dialogs = await p.locator('[role=dialog]').count(), sd = await p.getByTestId('settings-dialog').count()
    const toasts = await p.locator('[data-sonner-toast]').count()
    const errEls = await p.locator('[data-testid$=-error], [data-testid=task-drawer-error]').count()
    const body = await p.locator('body').innerText()
    const raw401 = /401|Unauthorized|status code/i.test(body)
    ok(sd === 1 && dialogs === 1 && toasts === 0 && errEls === 0 && !raw401, `${r}: only auth dialog (dialogs=${dialogs} settings=${sd} toasts=${toasts} errorEls=${errEls} raw401=${raw401})`)
    await p.evaluate(() => localStorage.removeItem('binlog_server_token'))
  }
  ok(errs.length === 0, 'no page errors ' + errs.join(';')); await ctx.close() }
// 3) unknown values show raw (mocked dashboard row), compare with Vue
{ const ctx = await mk('ui302-test-token')
  await ctx.route('**/api/dashboard*', async (r) => { const res = await r.fetch(); const j = await res.json(); const row = j.tasks?.find(x => String(x.task?.id) === '2') || j.tasks?.[0]; if (row) { row.task.state = 'WEIRD_STATE'; row.replication = { ...(row.replication || {}), status: 'ODD_STATUS', reason: 'ODD_REASON' }; row.task.source = { ...row.task.source, flavor: 'ODD_FLAVOR' } } await r.fulfill({ response: res, json: j }) })
  await ctx.route('**/api/tasks/2', async (r) => { if (r.request().method() !== 'GET') return r.continue(); const res = await r.fetch(); const j = await res.json(); j.state = 'WEIRD_STATE'; await r.fulfill({ response: res, json: j }) })
  const p = await ctx.newPage()
  await p.goto(CLASSIC + '/#/tasks'); await p.waitForTimeout(2500); const vb = await p.locator('body').innerText()
  await p.goto(BASE + '/#/tasks'); await p.waitForSelector('[data-testid=task-row-2]'); await p.waitForTimeout(800); const rb = await p.locator('body').innerText()
  for (const v of ['WEIRD_STATE', 'ODD_STATUS']) { console.log(`${v}: vue=${vb.includes(v)} react=${rb.includes(v)}`); ok(rb.includes(v), `${v} shown raw in tasks table`) }
  ok(!/state\.WEIRD|status\.ODD|\{\{|undefined|NaN/.test(rb), 'no raw i18n keys / undefined / NaN in table')
  await p.goto(BASE + '/#/tasks/2'); await p.waitForSelector('[data-testid=task-drawer-status]'); await p.waitForTimeout(800)
  const ds = await p.getByTestId('task-drawer-status').innerText(); ok(ds.includes('WEIRD_STATE'), 'drawer status raw: ' + ds.replace(/\s+/g, ' ').slice(0, 60))
  await ctx.close() }
// 4) number inputs clamp on blur (task form + batch)
{ const ctx = await mk('ui302-test-token'); const p = await ctx.newPage()
  await p.goto(BASE + '/#/tasks'); await p.waitForSelector('[data-testid=task-row-2]'); await p.getByTestId('create-task').click(); await p.waitForSelector('[data-testid=task-form]')
  const t = async (id, v) => { await p.getByTestId(id).fill(v); await p.getByTestId('task-form-name').click(); return p.getByTestId(id).inputValue() }
  const cases = [['task-form-port', '0', '1'], ['task-form-port', '70000', '65535'], ['task-form-port', '', '1'], ['task-form-server-id', '0', '1'], ['task-form-retention', '9999', '3650'], ['task-form-retention', '0', '1'], ['task-form-local-retention', '-5', '0'], ['task-form-bucket-retention', '99999', '3650']]
  for (const [id, v, want] of cases) { const got = await t(id, v); ok(got === want, `${id} "${v}" -> "${got}" (want ${want})`) }
  const ph = await p.getByTestId('task-form-password').getAttribute('placeholder'); ok(!ph, 'create: no keep-password hint: ' + ph)
  await p.keyboard.press('Escape'); await p.waitForTimeout(300)
  await p.getByTestId('batch-create-open').click(); await p.waitForSelector('[data-testid=batch-create]')
  for (const [id, v, want] of [['batch-server-id-start', '0', '1'], ['batch-retention', '0', '1']]) { await p.getByTestId(id).fill(v); await p.getByTestId('batch-user').click(); const got = await p.getByTestId(id).inputValue(); ok(got === want, `${id} "${v}" -> "${got}"`) }
  await ctx.close() }
await b.close()
