// Label and tag helpers ported from frontend/src/composables/useFormatters.js
// and useTaskFilter.js. List lease risk uses the task copy, never GET /lease.
import type { TFunction } from '@/lib/i18n'
import type { Replication, Task } from '@/lib/api'
import { toTimeMs } from '@/context/observation'

export const LEASE_RISK_SECONDS = 45

export const TASK_STATES = [
  'CREATED',
  'STARTING',
  'RUNNING',
  'LEASE_DEGRADED',
  'REBUILDING_FILE',
  'RETRY_BACKOFF',
  'STOPPING',
  'STOPPED',
  'FAILED',
] as const

export const REPLICATION_STATUSES = ['NORMAL', 'DELAYED', 'ABNORMAL', 'IDLE'] as const

/** Element Plus tag types the Vue console used; mapped to badge styles. */
export type Tone = 'success' | 'warning' | 'danger' | 'info'

export function stateLabel(t: TFunction, state?: string) {
  return t(`state.${state}`) || state || '--'
}

export function replicationStatusLabel(t: TFunction, status?: string) {
  return t(`replication.${status}`) || status || '--'
}

export function sourceLabel(task?: Task) {
  return `${task?.source?.host || '-'}:${task?.source?.port || '-'}`
}

export function ownerWorkerLabel(task?: Task) {
  return task?.owner_worker_id || '--'
}

export function stateTone(state?: string): Tone {
  if (state === 'RUNNING') return 'success'
  if (state === 'RETRY_BACKOFF') return 'warning'
  if (state === 'FAILED') return 'danger'
  return 'info'
}

export function replicationTone(status?: string): Tone {
  if (status === 'NORMAL') return 'success'
  if (status === 'DELAYED') return 'warning'
  if (status === 'ABNORMAL') return 'danger'
  return 'info'
}

function leaseStale(task: Task, nowMs: number): boolean | null {
  const owner = task?.owner_worker_id
  const epoch = Number(task?.epoch ?? 0)
  if (!owner || epoch <= 0) return null
  const updatedMs = toTimeMs(task?.updated_at)
  if (updatedMs <= 0) return true
  return nowMs - updatedMs > LEASE_RISK_SECONDS * 1000
}

export function leaseRiskTone(task: Task, nowMs: number): Tone {
  const stale = leaseStale(task, nowMs)
  if (stale === null) return 'info'
  return stale ? 'warning' : 'success'
}

export function leaseRiskLabel(t: TFunction, task: Task, nowMs: number) {
  const stale = leaseStale(task, nowMs)
  if (stale === null) return '--'
  return stale ? t('lease.risk') : t('lease.normal')
}

export function formatDelay(delaySeconds?: number | null, hasProgress?: boolean) {
  if (!hasProgress || delaySeconds === undefined || delaySeconds === null) return '--'
  return String(delaySeconds)
}

export function formatTs(locale: string, ts?: string) {
  if (!ts) return '--'
  const date = new Date(ts)
  if (Number.isNaN(date.getTime())) return '--'
  return date.toLocaleString(locale)
}

export function formatReplicationReason(t: TFunction, rep?: Replication) {
  if (!rep) return '--'
  const reasonMap: Record<string, string> = {
    NO_PROGRESS: t('replication.noProgress'),
    DELAY_EXCEEDS_THRESHOLD: t('replication.delayExceedsThreshold'),
    RUNNER_ERROR: t('replication.runnerError'),
    TASK_STATE_ERROR: t('replication.taskStateError'),
  }
  const rawErr = rep.last_error || rep.error || rep.err || rep.message || ''
  if (rawErr) {
    const label = (rep.reason && reasonMap[rep.reason]) || rep.reason || t('replication.runnerError')
    return `${label}: ${rawErr}`
  }
  if (rep.reason) return reasonMap[rep.reason] || rep.reason
  if (rep.status === 'DELAYED') return t('replication.delayExceedsThreshold')
  if (rep.status === 'ABNORMAL') return t('replication.abnormalNoDetail')
  return '--'
}
