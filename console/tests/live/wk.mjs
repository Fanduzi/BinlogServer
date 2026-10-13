import { chromium } from '@playwright/test'
import { BASE, CLASSIC, OUT } from './env.mjs'
const b = await chromium.launch(); const ctx = await b.newContext({ viewport: { width: 1440, height: 1000 } })
await ctx.addInitScript(() => { localStorage.setItem('binlog_server_token', 'ui302-test-token'); localStorage.setItem('locale', 'en') })
const ok = (c, m) => { console.log((c ? 'PASS ' : 'FAIL ') + m); if (!c) process.exitCode = 1 }
const p = await ctx.newPage(); const errs = []; p.on('pageerror', e => errs.push(e.message))
// 1) real env: single process, no workers
await p.goto(CLASSIC + '/#/workers'); await p.waitForTimeout(2000)
const vueEmpty = (await p.getByTestId('workers-empty').innerText()).trim()
await p.goto(BASE + '/#/workers'); await p.waitForSelector('[data-testid=cluster-panel]'); await p.waitForTimeout(800)
const reEmpty = (await p.getByTestId('workers-empty').innerText()).trim()
ok(vueEmpty === reEmpty, `empty state same as Vue: "${reEmpty}"`)
ok(await p.getByTestId('cluster-task-count').innerText() === '26', 'cluster counters')
// 2) mocked workers + multi-process overview, same mock for both consoles
let genAt = ''
await ctx.route('**/api/dashboard*', async (r) => { const res = await r.fetch(); const j = await res.json(); genAt = j.generated_at; await r.fulfill({ response: res, json: j }) })
await ctx.route('**/api/cluster/overview', async (r) => { const res = await r.fetch(); const j = await res.json(); await r.fulfill({ response: res, json: { ...j, single_process: false, worker_count: 4 } }) })
await ctx.route('**/api/workers', (r) => {
  const now = genAt ? Date.parse(genAt) : Date.now()
  const iso = (s) => new Date(now - s * 1000).toISOString()
  r.fulfill({ json: [
    { worker_id: 'w-flag-off', online: false, last_seen_at: iso(1), task_count: 3, running: 2, leased: 2 },
    { worker_id: 'w-fresh', last_seen_at: iso(10), task_count: 5, running: 4, leased: 5 },
    { worker_id: 'w-stale', last_seen_at: iso(120), task_count: 0, running: 0, leased: 0 },
    { worker_id: 'w-updated', updated_at: iso(5), task_count: 1, running: 1, leased: 1 },
  ] })
})
await p.goto(CLASSIC + '/#/workers'); await p.waitForTimeout(2500)
const vue = await p.$$eval('.cluster-worker-item', els => els.map(e => e.innerText.replace(/\s+/g, ' ').trim()))
const vueSummary = (await p.getByTestId('cluster-worker-summary').innerText()).trim()
await p.screenshot({ path: OUT + '/wk-classic.png', fullPage: true })
await p.goto(BASE + '/#/workers'); await p.waitForSelector('[data-testid=worker-row-w-fresh]'); await p.waitForTimeout(500)
const re = await p.$$eval('[data-testid^=worker-row-]', els => els.map(e => [...e.querySelectorAll('td')].map(td => td.innerText.trim()).join(' ').replace(/\s+/g, ' ')))
await p.screenshot({ path: OUT + '/wk-console.png', fullPage: true })
console.log('vue', vue); console.log('react', re)
ok(JSON.stringify(vue) === JSON.stringify(re), 'worker rows same text/order as Vue (id, online, tasks, heartbeat)')
ok((await p.getByTestId('cluster-worker-summary').innerText()).trim() === vueSummary, 'summary same as Vue: ' + vueSummary)
await p.setViewportSize({ width: 400, height: 800 }); await p.waitForTimeout(300)
const sw = await p.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth])
ok(sw[0] <= sw[1], 'no page overflow at 400px ' + sw)
ok(errs.length === 0, 'no page errors ' + errs.join(';'))
await b.close()
