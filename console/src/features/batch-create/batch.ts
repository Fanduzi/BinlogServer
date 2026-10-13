// Batch create parsing and payloads, ported from frontend/src/composables/useBatchCreate.js.
// Same line formats (name,host,port / host,port / host:port / name-less), same
// duplicate checks, cluster_key derivation, 100-item cap and POST /api/tasks/batch body.
import type { AnyRecord } from '@/lib/api'
import type { TFunction } from '@/lib/i18n'
import { validateTaskPayload } from '../task-form/task-form'

export const MAX_BATCH_ITEMS = 100

export interface BatchForm {
  lines: string
  user: string
  password: string
  flavor: string
  serverIdStart: number
  retentionDays: number
  semiSync: boolean
  autoStart: boolean
}

export interface BatchRow {
  lineNo: number
  name: string
  host: string
  port: number | string
  clusterKey?: string
  valid: boolean
  error: string
}

export interface BatchPreview {
  ready: boolean
  canSubmit: boolean
  validCount: number
  rows: BatchRow[]
  errors: string[]
}

export function defaultBatchForm(): BatchForm {
  return {
    lines: '',
    user: '',
    password: '',
    flavor: 'mysql',
    serverIdStart: 300000,
    retentionDays: 7,
    semiSync: false,
    autoStart: false,
  }
}

export const emptyPreview = (): BatchPreview => ({
  ready: false,
  canSubmit: false,
  validCount: 0,
  rows: [],
  errors: [],
})

function makeClusterKey(name: string, host: string, port: number, lineNo = 0) {
  const raw = `${name}-${host}-${port}-${lineNo}`.toLowerCase()
  const key = raw
    .replace(/[^a-z0-9._-]+/g, '-')
    .replace(/-+/g, '-')
    .replace(/^-|-$/g, '')
  return key || `cluster-${Date.now()}-${Math.floor(Math.random() * 1000)}`
}

function parseHostPort(t: TFunction, text: string, lineNo: number): [string, number] {
  const idx = text.lastIndexOf(':')
  if (idx <= 0 || idx === text.length - 1)
    throw new Error(t('batch.errors.hostPortFormatError', { lineNo }))
  const host = text.slice(0, idx).trim()
  const port = Number(text.slice(idx + 1).trim())
  if (!host || !Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error(t('batch.errors.hostPortFormatError', { lineNo }))
  }
  return [host, port]
}

function parseBatchLine(t: TFunction, raw: string, lineNo: number) {
  const parts = raw
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
  let name = ''
  let host = ''
  let port = 0
  if (parts.length === 1) {
    if (!parts[0].includes(':'))
      throw new Error(t('batch.errors.singleColumnMustBeHostPort', { lineNo }))
    ;[host, port] = parseHostPort(t, parts[0], lineNo)
    name = `task-${host}-${port}`
  } else if (parts.length === 2) {
    if (parts[0].includes(':')) {
      ;[host, port] = parseHostPort(t, parts[0], lineNo)
      name = parts[1]
    } else {
      host = parts[0]
      port = Number(parts[1])
      name = `task-${host}-${port}`
    }
  } else if (parts.length === 3) {
    name = parts[0]
    host = parts[1]
    port = Number(parts[2])
  } else {
    throw new Error(t('batch.errors.onlySupportFormats', { lineNo }))
  }
  if (!host || !Number.isInteger(Number(port)) || Number(port) <= 0 || Number(port) > 65535) {
    throw new Error(t('batch.errors.portInvalid', { lineNo }))
  }
  if (!name) name = `task-${host}-${port}`
  const clusterKey = makeClusterKey(name, host, Number(port), lineNo)
  const clusterKeyErr = validateTaskPayload(t, {
    name,
    cluster_key: clusterKey,
    source: {
      host,
      port: Number(port),
      user: 'repl',
      flavor: 'mysql',
      server_id: 1,
    },
    start: { mode: 'LATEST' },
    storage: { retention_days: 7 },
  })
  if (clusterKeyErr)
    throw new Error(t('batch.errors.clusterKeyInvalid', { lineNo, error: clusterKeyErr }))
  return { lineNo, name, host, port: Number(port), clusterKey }
}

export function parseBatchLines(t: TFunction, rawText: string) {
  const rows: BatchRow[] = []
  const errors: string[] = []
  const sourceSeen = new Set<string>()
  const nameSeen = new Set<string>()
  const clusterKeySeen = new Set<string>()
  const lines = rawText.split('\n')
  for (let i = 0; i < lines.length; i += 1) {
    const lineNo = i + 1
    const raw = lines[i].trim()
    if (!raw || raw.startsWith('#')) continue
    try {
      const row = parseBatchLine(t, raw, lineNo)
      const sourceKey = `${row.host}:${row.port}`
      const nameKey = row.name.toLowerCase()
      if (sourceSeen.has(sourceKey))
        throw new Error(t('batch.errors.sourceDuplicate', { lineNo, source: sourceKey }))
      if (nameSeen.has(nameKey))
        throw new Error(t('batch.errors.nameDuplicate', { lineNo, name: row.name }))
      if (clusterKeySeen.has(row.clusterKey))
        throw new Error(
          t('batch.errors.clusterKeyDuplicate', {
            lineNo,
            key: row.clusterKey,
          }),
        )
      sourceSeen.add(sourceKey)
      nameSeen.add(nameKey)
      clusterKeySeen.add(row.clusterKey)
      rows.push({ ...row, valid: true, error: '' })
    } catch (err) {
      const msg = (err as Error)?.message || t('batch.errors.lineFormatError', { lineNo })
      rows.push({
        lineNo,
        name: '--',
        host: '--',
        port: '--',
        valid: false,
        error: msg,
      })
      errors.push(msg)
    }
  }
  if (!rows.length) errors.push(t('batch.noData'))
  return { rows, errors }
}

export function buildPreview(t: TFunction, lines: string) {
  const parsed = parseBatchLines(t, lines || '')
  const validCount = parsed.rows.filter((x) => x.valid).length
  const tooManyError =
    validCount > MAX_BATCH_ITEMS ? t('batch.tooManyItems', { max: MAX_BATCH_ITEMS }) : ''
  const errors = tooManyError ? [...parsed.errors, tooManyError] : parsed.errors
  const preview: BatchPreview = {
    ready: true,
    rows: parsed.rows,
    errors,
    validCount,
    canSubmit: validCount > 0 && errors.length === 0,
  }
  return { preview, tooManyError }
}

// One payload per valid row; rows that fail validateTaskPayload become error lines.
export function buildBatchPayloads(t: TFunction, form: BatchForm, rows: BatchRow[]) {
  const errors: string[] = []
  const submittedRows: BatchRow[] = []
  const payloads: AnyRecord[] = []
  for (const row of rows.filter((r) => r.valid)) {
    const source: AnyRecord = {
      host: row.host,
      port: row.port,
      user: form.user,
      flavor: form.flavor,
      server_id: form.serverIdStart + row.lineNo - 1,
      semi_sync: !!form.semiSync,
    }
    if (form.password) source.password = form.password
    const payload = {
      name: row.name,
      cluster_key: row.clusterKey,
      source,
      start: { mode: 'LATEST' },
      storage: { retention_days: Number(form.retentionDays) },
    }
    const validationErr = validateTaskPayload(t, payload)
    if (validationErr) {
      errors.push(`${row.name}(${row.host}:${row.port}) -> ${validationErr}`)
      continue
    }
    submittedRows.push(row)
    payloads.push(payload)
  }
  return { errors, submittedRows, payloads }
}

export function batchItemError(itemError: unknown) {
  if (typeof itemError === 'string') return itemError
  const e = itemError as AnyRecord | null
  return e?.error || JSON.stringify(itemError || 'unknown error')
}
