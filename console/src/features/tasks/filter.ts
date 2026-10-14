// Current-page filtering and server pagination, ported from
// frontend/src/composables/useTaskFilter.js. Only the task state goes to the
// server (GET /api/dashboard state=); keyword, source, replication status,
// alert-only and sort act on the page the server returned.
import type { DashboardParams, DashboardRow } from '@/lib/api'
import { sourceLabel } from './format'

export interface UiFilter {
  keyword: string
  sourceKeyword: string
  taskState: string
  replicationStatus: string
  sortBy: 'delay_desc' | 'updated_desc' | 'name_asc'
  onlyAlert: boolean
}

export const defaultUiFilter: UiFilter = {
  keyword: '',
  sourceKeyword: '',
  taskState: 'ALL',
  replicationStatus: 'ALL',
  sortBy: 'delay_desc',
  onlyAlert: false,
}

export const PAGE_SIZES = [20, 50, 100]

export function isAlertRow(row: DashboardRow) {
  const rep = row.replication?.status
  const state = row.task?.state
  return (
    rep === 'ABNORMAL' ||
    rep === 'DELAYED' ||
    state === 'FAILED' ||
    state === 'RETRY_BACKOFF' ||
    row.task?.storage_alert === true ||
    (row.open_findings ?? 0) > 0
  )
}

export function filterRows(
  rows: DashboardRow[],
  f: UiFilter,
  keyword: string,
  sourceKeyword: string,
): DashboardRow[] {
  let out = [...(rows || [])]
  const kw = keyword.trim().toLowerCase()
  if (kw) {
    out = out.filter((row) => {
      const id = String(row.task?.id || '').toLowerCase()
      const name = String(row.task?.name || '').toLowerCase()
      return id.includes(kw) || name.includes(kw)
    })
  }
  const skw = sourceKeyword.trim().toLowerCase()
  if (skw) out = out.filter((row) => sourceLabel(row.task).toLowerCase().includes(skw))
  if (f.taskState !== 'ALL') out = out.filter((row) => row.task?.state === f.taskState)
  if (f.replicationStatus !== 'ALL') {
    out = out.filter((row) => row.replication?.status === f.replicationStatus)
  }
  if (f.onlyAlert) out = out.filter(isAlertRow)
  out.sort((a, b) => {
    if (f.sortBy === 'name_asc') {
      return String(a.task?.name || '').localeCompare(String(b.task?.name || ''))
    }
    if (f.sortBy === 'updated_desc') {
      const at = new Date(a.task?.updated_at || 0).getTime()
      const bt = new Date(b.task?.updated_at || 0).getTime()
      return bt - at
    }
    const ad = Number(a.replication?.delay_seconds ?? -1)
    const bd = Number(b.replication?.delay_seconds ?? -1)
    return bd - ad
  })
  return out
}

export function paginationParams(
  page: number,
  pageSize: number,
  taskState: string,
): DashboardParams {
  const params: DashboardParams = { limit: pageSize, offset: (page - 1) * pageSize }
  if (taskState !== 'ALL') params.state = taskState
  return params
}

/** Quick filters from the KPI cards and chips (App.vue applyQuickFilter). */
export type QuickFilter =
  'all' | 'alert' | 'abnormal' | 'failed' | 'delayed' | 'starting' | 'running' | 'normal'

export function quickFilterState(kind: QuickFilter, current: UiFilter): UiFilter {
  if (kind === 'all') return { ...defaultUiFilter }
  // Keyword, source keyword and sort survive a quick filter, as in App.vue.
  const base: UiFilter = {
    ...current,
    onlyAlert: false,
    taskState: 'ALL',
    replicationStatus: 'ALL',
  }
  switch (kind) {
    case 'alert':
      return { ...base, onlyAlert: true }
    case 'abnormal':
      return { ...base, replicationStatus: 'ABNORMAL' }
    case 'failed':
      return { ...base, taskState: 'FAILED' }
    case 'delayed':
      return { ...base, replicationStatus: 'DELAYED' }
    case 'starting':
      return { ...base, taskState: 'STARTING' }
    case 'running':
      return { ...base, taskState: 'RUNNING' }
    case 'normal':
      return { ...base, replicationStatus: 'NORMAL' }
  }
}
