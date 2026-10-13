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

// The header the server reads the credential from, learned once from the
// anonymous GET /auth/config: Authorization with the Bearer scheme, or the
// configured API key header. 404 (older servers) keeps Bearer for the session;
// any other failure sends Bearer for that request only and retries the lookup next time.
interface AuthHeader {
  name: string
  scheme: string
}
const BEARER: AuthHeader = { name: 'Authorization', scheme: 'Bearer' }
let authHeaderPromise: Promise<AuthHeader> | null = null

function loadAuthHeader(): Promise<AuthHeader> {
  if (!authHeaderPromise) {
    authHeaderPromise = axios
      .get('/auth/config', { timeout: 5000 })
      .then(({ data }) =>
        data?.mode === 'api_key' && data?.header
          ? { name: String(data.header), scheme: '' }
          : BEARER,
      )
      .catch((err) => {
        // 404: an older server without the route, so Bearer is the answer.
        // Timeouts, network errors and 5xx are not cached; the next request asks again.
        if (err?.response?.status !== 404) authHeaderPromise = null
        return BEARER
      })
  }
  return authHeaderPromise
}

http.interceptors.request.use(async (config) => {
  const token = getAuthToken()
  if (token) {
    const header = await loadAuthHeader()
    config.headers.set(header.name, header.scheme ? `${header.scheme} ${token}` : token)
  }
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

export interface SourceLookup {
  exists?: boolean
  count?: number
}

export async function lookupSource(params: { host: string; port: number }): Promise<SourceLookup> {
  const { data } = await http.get('/api/sources/lookup', { params })
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

// Route params reach the API path, so encode them (#317 QA).
const enc = (id: string | number) => encodeURIComponent(String(id))

// ---- Task detail drawer (frontend/src/api.js, useTaskDetail.js, App.vue) ----

export type AnyRecord = Record<string, any> // eslint-disable-line @typescript-eslint/no-explicit-any

export async function getTask(id: string | number) {
  const { data } = await http.get(`/api/tasks/${enc(id)}`)
  return data
}

export async function getCheckpoint(id: string | number) {
  try {
    const { data } = await http.get(`/api/tasks/${enc(id)}/checkpoint`)
    return data
  } catch (err) {
    if ((err as AnyRecord)?.response?.status === 404) return null
    throw err
  }
}

export async function getReplication(id: string | number) {
  const { data } = await http.get(`/api/tasks/${enc(id)}/replication`)
  return data
}

export async function getTaskLease(id: string | number) {
  const { data } = await http.get(`/api/tasks/${enc(id)}/lease`)
  return data
}

export async function listTaskRuns(id: string | number, limit = 10) {
  const { data } = await http.get(`/api/tasks/${enc(id)}/runs`, { params: { limit } })
  return data
}

export async function listEvents(id: string | number, limit = 120) {
  const { data } = await http.get(`/api/tasks/${enc(id)}/events`, { params: { limit } })
  return data
}

export async function listFiles(id: string | number, limit = 80) {
  const { data } = await http.get(`/api/tasks/${enc(id)}/files`, { params: { limit } })
  return data
}

// Without a limit replay lists every file a restore needs (#305).
export async function listReplay(id: string | number, limit?: number) {
  const params = limit ? { limit } : {}
  const { data } = await http.get(`/api/tasks/${enc(id)}/replay`, { params })
  return data
}

export async function getRecoveryWindow(id: string | number) {
  const { data } = await http.get(`/api/tasks/${enc(id)}/window`)
  return data
}

export interface PitrWindow {
  stop?: string
  start?: string
  gtid?: string
  executed?: string
}

export async function listPITRReplay(id: string | number, w: PitrWindow) {
  const params: Record<string, string> = {}
  const stop = String(w.stop || '').trim()
  const gtid = String(w.gtid || '').trim()
  if (stop) params.stop_datetime = stop
  if (gtid) params.stop_gtid = gtid
  const start = String(w.start || '').trim()
  if (start) params.start_datetime = start
  const executed = String(w.executed || '').trim()
  if (executed) params.start_gtid_set = executed
  const { data } = await http.get(`/api/tasks/${enc(id)}/replay`, { params })
  return data
}

export async function downloadTaskFile(id: string | number, name: string): Promise<Blob> {
  const path = `/api/tasks/${encodeURIComponent(String(id))}/files/${encodeURIComponent(name)}`
  const { data } = await http.get(path, { responseType: 'blob' })
  return data
}

// No limit: the archive holds every replay file (#305).
export async function downloadReplayArchive(id: string | number, w?: PitrWindow): Promise<Blob> {
  const path = `/api/tasks/${encodeURIComponent(String(id))}/replay/archive`
  const params: Record<string, string> = {}
  const stop = String(w?.stop || '').trim()
  const gtid = String(w?.gtid || '').trim()
  const executed = String(w?.executed || '').trim()
  if (stop || gtid || executed) {
    if (stop) params.stop_datetime = stop
    if (gtid) params.stop_gtid = gtid
    if (executed) params.start_gtid_set = executed
    const start = String(w?.start || '').trim()
    if (start) params.start_datetime = start
  }
  const { data } = await http.get(path, { params, responseType: 'blob' })
  return data
}

// POST /api/tasks/batch: one result per item, {index, task} or {index, error}.
export async function createTasksBatch(payload: { items: AnyRecord[] }): Promise<AnyRecord[]> {
  const { data } = await http.post('/api/tasks/batch', payload)
  return data
}

export async function createTask(payload: AnyRecord): Promise<Task> {
  const { data } = await http.post('/api/tasks', payload)
  return data
}

export async function updateTask(id: string | number, payload: AnyRecord): Promise<Task> {
  const { data } = await http.put(`/api/tasks/${enc(id)}`, payload)
  return data
}

export async function adoptTask(id: string | number, payload: AnyRecord): Promise<Task> {
  const { data } = await http.post(`/api/tasks/${enc(id)}/adopt`, payload)
  return data
}

export async function startTask(id: string | number) {
  await http.post(`/api/tasks/${enc(id)}/start`)
}

export async function stopTask(id: string | number) {
  await http.post(`/api/tasks/${enc(id)}/stop`)
}

export async function deleteTask(id: string | number) {
  await http.delete(`/api/tasks/${enc(id)}`)
}

export async function retryUpload(id: string | number, limit = 100) {
  const { data } = await http.post(`/api/tasks/${enc(id)}/files/retry-upload`, null, {
    params: { limit },
  })
  return data
}
