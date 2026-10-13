// Shared observation state for every Console page: one refresh runs
// GET /api/dashboard, GET /api/cluster/overview and GET /api/workers together,
// the same trio frontend/src/composables/useDashboard.js refreshAll issues.
import {
  createContext,
  useCallback,
  useContext,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { toast } from 'sonner'
import {
  getClusterOverview,
  getDashboard,
  listWorkers,
  type ClusterOverview,
  type Dashboard,
  type DashboardParams,
  type Worker,
} from '@/lib/api'

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
  if (!Number.isInteger(offset) || (offset as number) < 0) throw new Error('invalid dashboard offset')
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

export function toTimeMs(ts?: string) {
  if (!ts) return 0
  const ms = new Date(ts).getTime()
  return Number.isNaN(ms) ? 0 : ms
}

interface ObservationValue {
  loading: boolean
  dashboard: Dashboard
  overview: ClusterOverview
  workers: Worker[]
  /** Reference "now": dashboard generated_at when known, else the clock. */
  nowRefMs: () => number
  refresh: (params?: DashboardParams) => Promise<void>
}

const ObservationContext = createContext<ObservationValue | null>(null)

export function ObservationProvider({ children }: { children: ReactNode }) {
  const [loading, setLoading] = useState(false)
  const [dashboard, setDashboard] = useState<Dashboard>(emptyDashboard)
  const [overview, setOverview] = useState<ClusterOverview>(emptyOverview)
  const [workers, setWorkers] = useState<Worker[]>([])
  const lastParams = useRef<DashboardParams>({})
  const dashboardRef = useRef(dashboard)
  dashboardRef.current = dashboard

  const refresh = useCallback(async (params?: DashboardParams) => {
    if (params) lastParams.current = params
    try {
      setLoading(true)
      const [d, o, w] = await Promise.all([
        getDashboard(lastParams.current),
        getClusterOverview(),
        listWorkers(),
      ])
      const next = applyDashboard(dashboardRef.current, d)
      setDashboard(next)
      if (o) setOverview((prev) => ({ ...prev, ...o }))
      setWorkers(Array.isArray(w) ? w : [])
    } catch (err) {
      const e = err as { message?: string }
      toast.error(e?.message || String(err))
    } finally {
      setLoading(false)
    }
  }, [])

  const nowRefMs = useCallback(() => {
    const ms = toTimeMs(dashboardRef.current.generated_at)
    return ms > 0 ? ms : Date.now()
  }, [])

  return (
    <ObservationContext.Provider
      value={{ loading, dashboard, overview, workers, nowRefMs, refresh }}
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
