// Shared observation state for every Console page: one refresh runs
// GET /api/dashboard, GET /api/cluster/overview and GET /api/workers together,
// the same trio frontend/src/composables/useDashboard.js refreshAll issues.
//
// The dashboard query is composed from parts owned by different pages, as
// App.vue buildDashboardParams does: the task pager/state filter (Tasks,
// Alerts) and the source host:port filter (Sources). A page updates only its
// part; refresh() always sends the whole query. Overlapping refreshes are
// gated so only the latest response is applied.
import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from 'react'
import {
  getClusterOverview,
  getDashboard,
  listWorkers,
  type ClusterOverview,
  type Dashboard,
  type DashboardParams,
  type Worker,
} from '@/lib/api'
import { createLatest } from '@/lib/latest'
import { toTimeMs } from '@/lib/time'
import { notifyError } from '@/lib/notify'

const emptyDashboard: Dashboard = {
  generated_at: '',
  threshold_seconds: 30,
  total: 0,
  limit: 100,
  offset: 0,
  summary: {
    total: 0,
    starting: 0,
    running: 0,
    retry_backoff: 0,
    stopped: 0,
    failed: 0,
    normal: 0,
    delayed: 0,
    abnormal: 0,
  },
  tasks: [],
  sources: [],
}

const emptyOverview: ClusterOverview = {
  task_count: 0,
  worker_count: 0,
  running_task_count: 0,
  leased_task_count: 0,
  single_process: false,
}

function applyDashboard(prev: Dashboard, data: Partial<Dashboard> | null): Dashboard {
  if (!data) return prev
  const { total, limit, offset } = data
  if (!Number.isInteger(total) || (total as number) < 0) throw new Error('invalid dashboard total')
  if (!Number.isInteger(limit) || (limit as number) <= 0) throw new Error('invalid dashboard limit')
  if (!Number.isInteger(offset) || (offset as number) < 0)
    throw new Error('invalid dashboard offset')
  return {
    generated_at: data.generated_at || '',
    threshold_seconds: data.threshold_seconds || 30,
    total: total as number,
    limit: limit as number,
    offset: offset as number,
    summary: {
      ...prev.summary,
      ...(data.summary || {}),
      starting: data.summary?.starting ?? 0,
    },
    tasks: Array.isArray(data.tasks) ? data.tasks : [],
    sources: Array.isArray(data.sources) ? data.sources : [],
  }
}

export { toTimeMs }

export interface DashboardQuery {
  /** limit/offset/state from the task pager (features/tasks/sync.tsx). */
  paging: DashboardParams
  /** host/port from the Sources lookup. */
  source: DashboardParams
}

export function composeDashboardParams(query: DashboardQuery): DashboardParams {
  return { ...query.source, ...query.paging }
}

interface ObservationValue {
  loading: boolean
  dashboard: Dashboard
  overview: ClusterOverview
  workers: Worker[]
  /** Reference "now": dashboard generated_at when known, else the clock. */
  nowRefMs: () => number
  query: DashboardQuery
  /** Merge the given query parts, then fetch the trio. Resolves false if it failed or was superseded. */
  refresh: (patch?: Partial<DashboardQuery>) => Promise<boolean>
}

const ObservationContext = createContext<ObservationValue | null>(null)

export function ObservationProvider({ children }: { children: ReactNode }) {
  const [loading, setLoading] = useState(false)
  const [dashboard, setDashboard] = useState<Dashboard>(emptyDashboard)
  const [overview, setOverview] = useState<ClusterOverview>(emptyOverview)
  const [workers, setWorkers] = useState<Worker[]>([])
  const [query, setQuery] = useState<DashboardQuery>({ paging: {}, source: {} })
  const queryRef = useRef(query)
  const dashboardRef = useRef(dashboard)
  dashboardRef.current = dashboard
  const latest = useRef(createLatest()).current

  const refresh = useCallback(
    async (patch?: Partial<DashboardQuery>) => {
      if (patch) {
        queryRef.current = { ...queryRef.current, ...patch }
        setQuery(queryRef.current)
      }
      const token = latest.begin()
      setLoading(true)
      try {
        const [d, o, w] = await Promise.all([
          getDashboard(composeDashboardParams(queryRef.current)),
          getClusterOverview(),
          listWorkers(),
        ])
        if (!latest.isLatest(token)) return false
        setDashboard(applyDashboard(dashboardRef.current, d))
        if (o) setOverview((prev) => ({ ...prev, ...o }))
        setWorkers(Array.isArray(w) ? w : [])
        return true
      } catch (err) {
        if (latest.isLatest(token)) notifyError(err)
        return false
      } finally {
        if (latest.isLatest(token)) setLoading(false)
      }
    },
    [latest],
  )

  const nowRefMs = useCallback(() => {
    const ms = toTimeMs(dashboardRef.current.generated_at)
    return ms > 0 ? ms : Date.now()
  }, [])

  return (
    <ObservationContext.Provider
      value={{ loading, dashboard, overview, workers, nowRefMs, query, refresh }}
    >
      {children}
    </ObservationContext.Provider>
  )
}

export function useObservation() {
  const ctx = useContext(ObservationContext)
  if (!ctx) throw new Error('useObservation must be used inside ObservationProvider')
  return ctx
}
