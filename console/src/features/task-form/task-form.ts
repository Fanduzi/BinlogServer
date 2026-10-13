// Create / edit / adopt form logic ported from frontend/src/composables/useTaskForm.js.
// Same defaults, same validation order and messages, same payloads:
// POST /api/tasks, PUT /api/tasks/{id}, POST /api/tasks/{id}/adopt (adopt never starts replication).
import type { AnyRecord } from '@/lib/api'

type T = (key: string, params?: Record<string, unknown>) => string

// Form-only value. Omitted from POST /adopt so the server keeps FILE_POS at the highest segment.
export const ADOPT_START_DEFAULT = 'DISK_END'

export type FormMode = 'create' | 'edit' | 'adopt'

export interface TaskForm {
  id: string
  name: string
  cluster_key: string
  source: {
    host: string
    port: number
    user: string
    password: string
    flavor: string
    server_id: number
    semi_sync: boolean
    [k: string]: unknown
  }
  start: { mode: string; file: string; pos: number; gtid_set: string }
  storage: {
    retention_days: number
    local_retention_days: number
    bucket_retention_days: number
  }
}

const NAME_MAX_LENGTH = 255
const CLUSTER_KEY_PATTERN = /^[A-Za-z0-9._-]+$/
const SOURCE_HOST_MAX_LENGTH = 255
const SOURCE_USER_MAX_LENGTH = 128
const SOURCE_FLAVOR_MAX_LENGTH = 32
const START_FILE_MAX_LENGTH = 255
export const RETENTION_DAYS_MIN = 1
export const RETENTION_DAYS_MAX = 3650

const hasWhitespace = (text: unknown) => /\s/.test(String(text || ''))

export function defaultForm(): TaskForm {
  return {
    id: '',
    name: '',
    cluster_key: '',
    source: {
      host: '127.0.0.1',
      port: 3306,
      user: 'repl',
      password: '',
      flavor: 'mysql',
      server_id: 200001,
      semi_sync: false,
    },
    start: { mode: 'LATEST', file: '', pos: 0, gtid_set: '' },
    storage: {
      retention_days: 7,
      local_retention_days: 0,
      bucket_retention_days: 0,
    },
  }
}

// Same as openEdit: the stored task over the defaults, password cleared.
export function editForm(task: AnyRecord): TaskForm {
  const base = defaultForm()
  const copy = JSON.parse(JSON.stringify(task)) as AnyRecord
  const form = { ...base, ...copy } as TaskForm
  form.id = String(copy.id ?? '')
  form.source = { ...(copy.source ?? base.source) }
  form.source.password = ''
  form.start = copy.start ?? base.start
  form.storage = copy.storage ?? base.storage
  return form
}

export function adoptForm(task: AnyRecord): TaskForm {
  const form = defaultForm()
  form.id = String(task?.id || '')
  form.name = String(task?.name || task?.id || '')
  form.source.host = ''
  form.source.user = ''
  form.start.mode = ADOPT_START_DEFAULT
  const retention = Number(task?.storage?.retention_days || 0)
  form.storage.retention_days =
    Number.isInteger(retention) &&
    retention >= RETENTION_DAYS_MIN &&
    retention <= RETENTION_DAYS_MAX
      ? retention
      : 7
  form.storage.local_retention_days = Number(task?.storage?.local_retention_days || 0)
  form.storage.bucket_retention_days = Number(task?.storage?.bucket_retention_days || 0)
  return form
}

function validateClusterKey(t: T, raw: unknown) {
  const clusterKey = String(raw || '').trim()
  if (!clusterKey) return t('validation.clusterKeyEmpty')
  if (
    clusterKey.includes('/') ||
    clusterKey.includes('\\') ||
    clusterKey.includes('..') ||
    !CLUSTER_KEY_PATTERN.test(clusterKey)
  ) {
    return t('validation.clusterKeyInvalid')
  }
  return ''
}

function storagePayload(storage: AnyRecord) {
  const out: AnyRecord = { retention_days: Number(storage?.retention_days) }
  const local = Number(storage?.local_retention_days || 0)
  const bucket = Number(storage?.bucket_retention_days || 0)
  if (Number.isInteger(local) && local > 0) out.local_retention_days = local
  if (Number.isInteger(bucket) && bucket > 0) out.bucket_retention_days = bucket
  return out
}

function optionalRetentionDays(value: unknown) {
  const days = Number(value || 0)
  if (!Number.isInteger(days) || days < 0 || days > RETENTION_DAYS_MAX) return false
  if (days !== 0 && days < RETENTION_DAYS_MIN) return false
  return true
}

function validateStorage(t: T, storage: AnyRecord) {
  const retentionDays = Number(storage?.retention_days || 0)
  if (
    !Number.isInteger(retentionDays) ||
    retentionDays < RETENTION_DAYS_MIN ||
    retentionDays > RETENTION_DAYS_MAX
  ) {
    return t('validation.retentionInvalid')
  }
  if (!optionalRetentionDays(storage?.local_retention_days))
    return t('validation.localRetentionInvalid')
  if (!optionalRetentionDays(storage?.bucket_retention_days))
    return t('validation.bucketRetentionInvalid')
  const local = Number(storage?.local_retention_days || 0) || retentionDays
  const bucket = Number(storage?.bucket_retention_days || 0) || retentionDays
  if (bucket < local) return t('validation.bucketRetentionShorter')
  return ''
}

function validateSource(t: T, source: AnyRecord, requirePassword: boolean) {
  const host = String(source.host || '').trim()
  const user = String(source.user || '').trim()
  const flavor = String(source.flavor || '').trim() || 'mysql'
  const port = Number(source.port || 0)
  const serverID = Number(source.server_id || 0)
  if (!host || host.length > SOURCE_HOST_MAX_LENGTH || hasWhitespace(host))
    return t('validation.hostInvalid')
  if (!Number.isInteger(port) || port < 1 || port > 65535) return t('validation.portInvalid')
  if (!user || user.length > SOURCE_USER_MAX_LENGTH || hasWhitespace(user))
    return t('validation.userInvalid')
  if (requirePassword && !String(source.password || '')) return t('validation.passwordRequired')
  if (!flavor || flavor.length > SOURCE_FLAVOR_MAX_LENGTH || !CLUSTER_KEY_PATTERN.test(flavor)) {
    return t('validation.flavorInvalid')
  }
  if (!Number.isInteger(serverID) || serverID < 0 || serverID > 4294967295)
    return t('validation.serverIdInvalid')
  return ''
}

function validateStart(t: T, start: AnyRecord) {
  const mode = String(start?.mode || '').trim()
  if (!['LATEST', 'FILE_POS', 'GTID'].includes(mode)) return t('validation.startModeInvalid')
  if (mode === 'FILE_POS') {
    const file = String(start.file || '').trim()
    const pos = Number(start.pos || 0)
    if (!file || file.length > START_FILE_MAX_LENGTH || !Number.isInteger(pos) || pos <= 0) {
      return t('validation.filePosRequired')
    }
  }
  if (mode === 'GTID' && !String(start.gtid_set || '').trim())
    return t('validation.gtidSetRequired')
  return ''
}

export function buildPayload(form: TaskForm): AnyRecord {
  const payload: AnyRecord = {
    name: form.name.trim(),
    cluster_key: form.cluster_key?.trim(),
    source: {
      ...form.source,
      host: form.source.host?.trim() || '',
      user: form.source.user?.trim() || '',
      flavor: form.source.flavor?.trim() || '',
      semi_sync: !!form.source.semi_sync,
    },
    start: { mode: form.start.mode },
    storage: storagePayload(form.storage),
  }
  if (!payload.source.password) delete payload.source.password
  if (payload.start.mode === 'FILE_POS') {
    payload.start.file = form.start.file?.trim() || ''
    payload.start.pos = Number(form.start.pos || 0)
  }
  if (payload.start.mode === 'GTID') payload.start.gtid_set = form.start.gtid_set?.trim() || ''
  return payload
}

export function validateTaskPayload(t: T, payload: AnyRecord) {
  const name = String(payload?.name || '').trim()
  if (!name || name.length > NAME_MAX_LENGTH) return t('validation.taskNameInvalid')
  return (
    validateClusterKey(t, payload?.cluster_key) ||
    validateSource(t, payload?.source || {}, false) ||
    validateStart(t, payload?.start || {}) ||
    validateStorage(t, payload?.storage)
  )
}

export function buildAdoptPayload(form: TaskForm): AnyRecord {
  const payload: AnyRecord = {
    cluster_key: form.cluster_key?.trim(),
    source: {
      host: form.source.host?.trim() || '',
      port: Number(form.source.port || 0),
      user: form.source.user?.trim() || '',
      password: form.source.password,
      flavor: form.source.flavor?.trim() || '',
      server_id: Number(form.source.server_id || 0),
      semi_sync: !!form.source.semi_sync,
    },
  }
  const name = form.name.trim()
  if (name) payload.name = name
  const mode = String(form.start.mode || '').trim()
  if (mode && mode !== ADOPT_START_DEFAULT) {
    payload.start = { mode }
    if (mode === 'FILE_POS') {
      payload.start.file = form.start.file?.trim() || ''
      payload.start.pos = Number(form.start.pos || 0)
    }
    if (mode === 'GTID') payload.start.gtid_set = form.start.gtid_set?.trim() || ''
  }
  if (Number(form.storage.retention_days || 0)) payload.storage = storagePayload(form.storage)
  return payload
}

export function validateAdoptPayload(t: T, payload: AnyRecord) {
  const name = String(payload?.name || '').trim()
  if (name && name.length > NAME_MAX_LENGTH) return t('validation.taskNameInvalid')
  return (
    validateClusterKey(t, payload?.cluster_key) ||
    validateSource(t, payload?.source || {}, true) ||
    (payload?.start?.mode ? validateStart(t, payload.start) : '') ||
    (payload?.storage ? validateStorage(t, payload.storage) : '')
  )
}
