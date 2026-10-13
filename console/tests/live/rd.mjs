import { chromium } from '@playwright/test'
import { BASE, CLASSIC, OUT } from './env.mjs'


const b = await chromium.launch()
const ctx = await b.newContext({ viewport: { width: 1440, height: 900 }, acceptDownloads: true, permissions: ['clipboard-read', 'clipboard-write'] })
const p = await ctx.newPage()
const calls = []
p.on('request', (r) => { const u = new URL(r.url()); if (u.pathname.startsWith('/api')) calls.push(`${r.method()} ${u.pathname}${u.search}`) })
const errs = []; p.on('pageerror', (e) => errs.push(String(e)))
const ok = (c, m) => { console.log((c ? 'PASS ' : 'FAIL ') + m); if (!c) process.exitCode = 1 }
const norm = (a) => [...a].sort()
await ctx.addInitScript(() => { localStorage.setItem('binlog_server_token', 'ui302-test-token'); localStorage.setItem('locale', 'zh-CN') })
await p.goto(BASE + '/#/tasks'); await p.waitForSelector('[data-testid=task-row-2]'); await p.waitForTimeout(500)

// 1. Detail button opens drawer; calls equal the Vue drawer's
calls.length = 0
await p.click('[data-testid=task-detail-trigger-2]')
await p.waitForSelector('[data-testid=task-drawer-status]')
await p.waitForTimeout(800)
ok(p.url().endsWith('#/tasks/2'), 'detail button -> #/tasks/2')
console.log('open calls:', norm(calls))
ok(norm(calls).join('|') === norm(['GET /api/tasks/2', 'GET /api/tasks/2/checkpoint', 'GET /api/tasks/2/events?limit=120', 'GET /api/tasks/2/files?limit=80', 'GET /api/tasks/2/replay', 'GET /api/tasks/2/replication', 'GET /api/tasks/2/window', 'GET /api/tasks/2/lease', 'GET /api/tasks/2/runs?limit=10']).join('|'), 'drawer issues the same 9 requests as useTaskDetail.showDetail')
ok((await p.textContent('[data-testid=task-replay-command]')).includes('mysqlbinlog'), 'replay command rendered')
ok((await p.textContent('[data-testid=task-recovery-range]')).includes('UTC'), 'recovery window rendered')
ok(await p.locator('[data-testid^=file-download-]').count() === 2, 'two file rows with download')
await p.screenshot({ path: `${OUT}/01-drawer.png`, fullPage: false })

// 2. copy replay -> clipboard
await p.click('[data-testid=task-replay-copy]'); await p.waitForTimeout(300)
const clip = await p.evaluate(() => navigator.clipboard.readText())
ok(clip.startsWith('mysqlbinlog \\\n  tmp/ui302/data/2/mysql-bin.000003'), 'copy replay puts the command on the clipboard')

// 3. downloads
calls.length = 0
let [dl] = await Promise.all([p.waitForEvent('download'), p.click('[data-testid=task-replay-download]')])
ok(dl.suggestedFilename() === 'task-2-replay.tar', 'replay download name task-2-replay.tar')
;[dl] = await Promise.all([p.waitForEvent('download'), p.click('[data-testid="file-download-mysql-bin.000003"]')])
ok(dl.suggestedFilename() === 'mysql-bin.000003', 'segment download name')
console.log('download calls:', calls)

// 4. PITR build + download
calls.length = 0
await p.fill('[data-testid=task-pitr-stop]', '2099-01-01 00:00:00')
await p.click('[data-testid=task-pitr-build]'); await p.waitForTimeout(800)
const pitrText = await p.textContent('[data-testid=task-pitr]')
ok(/mysqlbinlog|stop-datetime/.test(pitrText), 'PITR command built')
;[dl] = await Promise.all([p.waitForEvent('download'), p.click('[data-testid=task-pitr-download]')])
console.log('pitr calls:', calls)
ok(calls.some((c) => c.includes('/replay?stop_datetime=2099-01-01')) && calls.some((c) => c.includes('/replay/archive?stop_datetime=2099-01-01')), 'PITR uses stop_datetime on /replay and /replay/archive')
calls.length = 0
await p.fill('[data-testid=task-pitr-stop]', 'not-a-date'); await p.click('[data-testid=task-pitr-build]'); await p.waitForTimeout(600)
ok(await p.locator('[data-testid=task-pitr-error]').isVisible(), 'bad PITR input shows server error: ' + (await p.textContent('[data-testid=task-pitr-error]').catch(() => '')))
await p.screenshot({ path: `${OUT}/02-pitr.png` })

// 5. close returns to list; row click opens too; Enter key
await p.keyboard.press('Escape'); await p.waitForTimeout(400)
ok(p.url().endsWith('#/tasks'), 'Esc closes drawer -> #/tasks')
await p.click('[data-testid=task-row-3]'); await p.waitForSelector('[data-testid=task-drawer-status]')
ok(p.url().endsWith('#/tasks/3'), 'row click -> #/tasks/3')
await p.keyboard.press('Escape'); await p.waitForTimeout(1000)
await p.locator('tr.task-row-4').focus(); await p.keyboard.press('Enter'); await p.waitForSelector('[data-testid=task-drawer-status]')
ok(p.url().endsWith('#/tasks/4'), 'Enter on row -> #/tasks/4')
await p.keyboard.press('Escape'); await p.waitForTimeout(400)

// 6. start / stop / delete on idle-1 (id 5)
await p.goto(BASE + '/#/tasks/6'); await p.waitForSelector('[data-testid=task-drawer-status]'); await p.waitForTimeout(500)
ok(await p.locator('[data-testid=task-action-edit]').isEnabled(), 'edit is enabled')
calls.length = 0
await p.click('[data-testid=task-action-start]'); await p.waitForTimeout(1500)
console.log('start calls:', calls)
ok(calls[0] === 'POST /api/tasks/6/start' && calls.includes('GET /api/tasks/6'), 'start: POST start, refresh, reload detail')
calls.length = 0
await p.click('[data-testid=task-action-stop]'); await p.waitForTimeout(4000)
console.log('stop calls:', calls)
ok(calls[0] === 'POST /api/tasks/6/stop' && calls.includes('GET /api/tasks/6'), 'stop: POST stop, refresh, drawer reloads')
calls.length = 0
await p.click('[data-testid=task-action-delete]'); await p.waitForSelector('[data-testid=task-delete-confirm]')
await p.screenshot({ path: `${OUT}/03-delete-confirm.png` })
await p.getByRole('button', { name: '取消' }).click(); await p.waitForTimeout(300)
ok(!calls.some((c) => c.startsWith('DELETE')), 'cancel delete sends nothing')
await p.click('[data-testid=task-action-delete]'); await p.waitForSelector('[data-testid=task-delete-confirm]')
await p.locator('[data-testid=task-delete-confirm]').getByRole('button', { name: '删除' }).click(); await p.waitForTimeout(1500)
console.log('delete calls:', calls)
ok(calls[0] === 'DELETE /api/tasks/6', 'confirm delete sends DELETE /api/tasks/6')
ok(p.url().endsWith('#/tasks'), 'drawer closes after delete')

// 7. direct links
await p.goto(BASE + '/#/tasks/999999'); await p.waitForTimeout(1500)
ok(await p.locator('[data-testid=task-drawer-error]').isVisible(), 'unknown id: drawer shows the load error, list behind it')
await p.screenshot({ path: `${OUT}/04-unknown-id.png` })
await p.goto(BASE + '/#/alerts/1'); await p.waitForSelector('[data-testid=task-drawer-status]')
ok(true, 'alerts/1 direct link opens drawer')
await p.keyboard.press('Escape'); await p.waitForTimeout(400)
ok(p.url().endsWith('#/alerts'), 'close from alerts returns to #/alerts')
await p.goto(BASE + '/#/no-such-page'); await p.waitForTimeout(800)
ok(await p.locator('[data-testid=not-found]').isVisible(), 'unknown path shows NotFound inside the shell')

// 8. mobile drawer
await p.setViewportSize({ width: 400, height: 800 }); await p.goto(BASE + '/#/tasks/2'); await p.waitForSelector('[data-testid=task-drawer-status]'); await p.waitForTimeout(600)
const sw = await p.evaluate(() => [document.documentElement.scrollWidth, document.querySelector('[data-testid=task-drawer]').scrollWidth, document.querySelector('[data-testid=task-drawer]').clientWidth])
ok(sw[0] === 400 && sw[1] === sw[2], 'no horizontal overflow at 400px with drawer open ' + sw)
await p.screenshot({ path: `${OUT}/05-mobile-drawer.png` })
ok(errs.length === 0, 'no page errors ' + errs.join(';'))
await b.close()
