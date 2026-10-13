// Worker list rows (App.vue workerRows): use the server's online flag when it
// is a boolean, else online means a heartbeat (last_seen_at, then updated_at)
// within LEASE_RISK_SECONDS of the reference now.
import type { Worker } from '@/lib/api'
import { toTimeMs } from '@/lib/time'
import { LEASE_RISK_SECONDS } from '../tasks/format'

export interface WorkerRow extends Worker {
  online: boolean
  task_count: number
  running: number
  leased: number
}

const count = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0)

export function workerRows(workers: Worker[] | null | undefined, nowMs: number): WorkerRow[] {
  return (workers || []).map((w) => {
    const seenAt = w.last_seen_at || w.updated_at
    const seenMs = toTimeMs(seenAt)
    const fallback = seenMs > 0 && nowMs - seenMs <= LEASE_RISK_SECONDS * 1000
    return {
      ...w,
      last_seen_at: seenAt,
      online: typeof w.online === 'boolean' ? w.online : fallback,
      // A missing count reads 0, not "undefined".
      task_count: count(w.task_count),
      running: count(w.running),
      leased: count(w.leased),
    }
  })
}
