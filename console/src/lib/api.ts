// API layer for the new Console. Endpoints, params and auth handling match
// frontend/src/api.js; pages move over one by one (issue #302) and add their
// calls here as they land.
import axios from 'axios'
import { clearAuthToken, getAuthToken } from './auth'

export const AUTH_REQUIRED_EVENT = 'auth-required'

const http = axios.create({ baseURL: '/', timeout: 10000 })

let authDialogShown = false

/** True after a 401 until settings are saved, so a shell that mounts after
 * the first 401 still opens the re-auth dialog. */
export function isAuthDialogPending() {
  return authDialogShown
}

export function resetAuthDialogShown() {
  authDialogShown = false
}

http.interceptors.request.use((config) => {
  const token = getAuthToken()
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

http.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error?.response?.status === 401) {
      clearAuthToken()
      if (!authDialogShown) {
        authDialogShown = true
        window.dispatchEvent(new CustomEvent(AUTH_REQUIRED_EVENT))
      }
    }
    return Promise.reject(error)
  },
)

export interface TaskSource {
  host?: string
  port?: number
  semi_sync?: boolean
}

export interface Task {
  id: number | string
  name?: string
  state?: string
  storage_alert?: boolean
  owner_worker_id?: string
  epoch?: number
  updated_at?: string
  source?: TaskSource
}

export interface Replication {
  status?: string
  reason?: string
  delay_seconds?: number | null
  has_progress?: boolean
  last_event_at?: string
  last_error?: string
  error?: string
  err?: string
  message?: string
}

export interface DashboardRow {
  task: Task
  replication: Replication
}

export interface DashboardSummary {
  total: number
  starting: number
  running: number
  retry_backoff: number
  stopped: number
  failed: number
  normal: number
  delayed: number
  abnormal: number
}

export interface SourceCoverage {
  host: string
  port: number
  task_count: number
  starting?: number
  running: number
  normal: number
  delayed: number
  abnormal: number
}

export interface Dashboard {
  generated_at: string
  threshold_seconds: number
  total: number
  limit: number
  offset: number
  summary: DashboardSummary
  tasks: DashboardRow[]
  sources: SourceCoverage[]
}

export interface ClusterOverview {
  task_count: number
  worker_count: number
  running_task_count: number
  leased_task_count: number
  single_process: boolean
}

export interface Worker {
  worker_id: string
  online?: boolean
  last_seen_at?: string
  updated_at?: string
  task_count?: number
  running?: number
  leased?: number
}

export type DashboardParams = {
  limit?: number
  offset?: number
  state?: string
  host?: string
  port?: number
}

export async function getDashboard(params: DashboardParams = {}) {
  const { data } = await http.get('/api/dashboard', { params })
  return data
}

export async function getClusterOverview() {
  const { data } = await http.get('/api/cluster/overview')
  return data
}

export async function listWorkers() {
  const { data } = await http.get('/api/workers')
  return data
}
