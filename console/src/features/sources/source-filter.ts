// Pure helpers for the Sources lookup (useDashboard.js buildSourceFilter,
// App.vue applySourceFilter).
import type { DashboardParams, SourceLookup } from '@/lib/api'

export interface SourceQuery {
  host: string
  port: number | null
}

export interface LookupResult {
  checked: boolean
  exists: boolean
  count: number
}

export const emptySourceQuery: SourceQuery = { host: '', port: null }
export const emptyLookup: LookupResult = { checked: false, exists: false, count: 0 }

/** Dashboard host/port params: trimmed host, port only when set. */
export function buildSourceFilter(q: SourceQuery): DashboardParams {
  const params: DashboardParams = {}
  const host = q.host.trim()
  if (host) params.host = host
  if (q.port) params.port = Number(q.port)
  return params
}

/** The lookup needs both; null means "ask for host and port". */
export function lookupParams(q: SourceQuery): { host: string; port: number } | null {
  const p = buildSourceFilter(q)
  return p.host && p.port ? { host: p.host, port: p.port } : null
}

export function lookupResult(resp: SourceLookup | null | undefined): LookupResult {
  return { checked: true, exists: !!resp?.exists, count: Number(resp?.count || 0) }
}
