import { chromium } from '@playwright/test'
import { BASE, CLASSIC, OUT } from './env.mjs'
const b = await chromium.launch(); const ctx = await b.newContext({ viewport: { width: 1440, height: 1000 } })
await ctx.addInitScript(() => { localStorage.setItem('binlog_server_token', 'ui302-test-token'); localStorage.setItem('locale', 'en') })
const p = await ctx.newPage(); const errs = []; p.on('pageerror', e => errs.push(e.message))
await p.goto(CLASSIC + '/#/overview'); await p.waitForTimeout(2500)
const vue = (await p.locator('body').innerText()).replace(/\s+/g, ' ')
await p.screenshot({ path: OUT + '/ov-classic.png', fullPage: true })
await p.goto(BASE + '/'); await p.waitForTimeout(2500)
const url = p.url()
const g = async id => (await p.getByTestId(id).first().innerText()).trim()
const r = { url, workers: await g('cluster-worker-summary'), tasks: await g('cluster-task-count'), running: await g('cluster-running-count'), leased: await g('cluster-leased-count'), note: await g('overview-note'), sources: await p.getByTestId('source-cell').count() }
await p.screenshot({ path: OUT + '/ov-console.png', fullPage: true })
// metric card click -> tasks quick filter
await p.getByTestId('kpi-abnormal').click(); await p.waitForTimeout(800); r.afterMetricClick = p.url()
console.log(JSON.stringify(r))
const ok = (c, m) => { console.log((c ? 'PASS ' : 'FAIL ') + m); if (!c) process.exitCode = 1 }
ok(r.url.endsWith('#/overview'), 'root opens #/overview')
ok(vue.includes(`Cluster View ${r.workers} Total Tasks ${r.tasks} Running Tasks ${r.running} Leased ${r.leased}`), 'cluster panel same as classic')
ok(vue.includes(`Source Coverage ${r.sources} hosts`), 'source coverage count same as classic')
ok(r.afterMetricClick.endsWith('#/tasks'), 'KPI click opens #/tasks')
ok(errs.length === 0, 'no page errors ' + errs.join(';'))
await b.close()
