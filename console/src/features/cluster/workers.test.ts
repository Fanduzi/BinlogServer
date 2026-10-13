import { expect, it } from 'vitest'
import { workerRows } from './workers'

const now = Date.parse('2026-10-13T00:01:00Z')

it('a boolean online flag wins over the heartbeat', () => {
  const rows = workerRows(
    [
      { worker_id: 'a', online: false, last_seen_at: '2026-10-13T00:00:59Z' },
      { worker_id: 'b', online: true, last_seen_at: '2020-01-01T00:00:00Z' },
    ],
    now,
  )
  expect(rows.map((r) => r.online)).toEqual([false, true])
})

it('falls back to last_seen_at, then updated_at, within 45s', () => {
  const rows = workerRows(
    [
      { worker_id: 'fresh', last_seen_at: '2026-10-13T00:00:15Z' },
      {
        worker_id: 'edge',
        last_seen_at: '2026-10-13T00:00:16Z',
        updated_at: '2020-01-01T00:00:00Z',
      },
      { worker_id: 'stale', last_seen_at: '2026-10-13T00:00:14Z' },
      { worker_id: 'updated', updated_at: '2026-10-13T00:00:50Z' },
      { worker_id: 'none' },
    ],
    now,
  )
  expect(rows.map((r) => [r.worker_id, r.online])).toEqual([
    ['fresh', true],
    ['edge', true],
    ['stale', false],
    ['updated', true],
    ['none', false],
  ])
  expect(rows[3].last_seen_at).toBe('2026-10-13T00:00:50Z')
})

it('null list', () => {
  expect(workerRows(null, now)).toEqual([])
})

it('missing counts read 0', () => {
  const [r] = workerRows([{ worker_id: 'x', online: true }], now)
  expect([r.task_count, r.running, r.leased]).toEqual([0, 0, 0])
})
