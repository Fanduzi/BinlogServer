// input: replay path lists, binlog client names, checkpoints, and locale labels
// output: copy-paste restore commands with set -o pipefail, --verify-binlog-checksum, --skip-gtid-strict-mode for mariadb-binlog, and a row-count or GTID verification comment
// pos: task-detail formatting for the replay command the operator copies
// note: if this file changes, update this header and console/src/features/task-detail/README.md.
// Pure helpers for the task detail drawer, ported line for line from
// frontend/src/components/TaskDetailDrawer.vue and the drawer handlers in
// App.vue / useTaskDetail.js / useFormatters.js.
import type { AnyRecord } from '@/lib/api'
import { tKnown, type TFunction } from '@/lib/i18n'

export function formatCheckpoint(t: TFunction, cp: AnyRecord | null) {
  if (!cp) return t('detail.noCheckpoint')
  const file =
    cp.file || cp.File || cp.file_name || cp.FileName || cp.binlog_file || cp.BinlogFile || ''
  if (!file) return t('detail.noCheckpoint')
  const pos = cp.pos ?? cp.Pos ?? cp.position ?? cp.Position ?? cp.binlog_pos ?? cp.BinlogPos ?? 0
  const gtid = String(cp.gtid_set || cp.GTIDSet || cp.gtid || '').trim()
  const base = `${file}:${pos}`
  return gtid ? `${base} GTID ${gtid}` : base
}

export function saveBlob(name: string, data: Blob | BlobPart) {
  const blob = data instanceof Blob ? data : new Blob([data], { type: 'application/octet-stream' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = name
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}

export function storageAlertLines(t: TFunction, task: AnyRecord | null): string[] {
  const alert = task?.storage_alert
  if (!alert) return []
  const segment = String(alert.segment || '').trim()
  if (!segment) return [String(alert.message || '').trim()].filter(Boolean)
  const lines = [t('detail.storageAlertSegment', { segment })]
  const missing = String(alert.missing_gtids || '').trim()
  if (missing) lines.push(t('detail.storageAlertMissing', { gtids: missing }))
  const valid: string[] = Array.isArray(alert.valid_segments) ? alert.valid_segments : []
  if (valid.length === 0) {
    lines.push(t('detail.storageAlertNoValid', { segment }))
  } else if (valid.length === 1) {
    lines.push(t('detail.storageAlertValidOne', { segment, first: valid[0] }))
  } else {
    lines.push(
      t('detail.storageAlertValid', {
        segment,
        count: valid.length,
        first: valid[0],
        last: valid[valid.length - 1],
      }),
    )
  }
  lines.push(t('detail.storageAlertNext'))
  const restart = String(alert.restart_gtid_set || '').trim()
  if (restart) lines.push(t('detail.storageAlertRestart', { gtids: restart }))
  return lines
}

export function damagedReplayText(t: TFunction, task: AnyRecord | null, warning: unknown) {
  const text = String(warning || '').trim()
  if (!text) return ''
  const segment = String(task?.storage_alert?.segment || '').trim()
  return segment ? t('detail.replayDamaged', { segment }) : text
}

function formatRecoveryInstant(value: unknown) {
  const text = String(value || '').trim()
  if (!text) return ''
  return text.replace('T', ' ').replace(/\.\d+/, '').replace(/Z$/, ' UTC')
}

export function recoveryRange(t: TFunction, recovery: AnyRecord | null) {
  const earliest = formatRecoveryInstant(recovery?.earliest)
  const latest = formatRecoveryInstant(recovery?.latest)
  if (!earliest && !latest) return t('detail.recoveryEmpty')
  return `${earliest || '--'} → ${latest || '--'}`
}

export function formatBreak(item: AnyRecord) {
  const files = Array.isArray(item?.files) ? item.files.filter(Boolean).join(', ') : ''
  const reason = String(item?.reason || '').trim()
  if (files && reason) return `${files}: ${reason}`
  return reason || files
}

export function replayHint(t: TFunction, replay: AnyRecord | null) {
  const hint = String(replay?.client_hint || '').trim()
  return hint || t('detail.replayFlavorUnset')
}

export function showBucketOnly(
  files: AnyRecord[],
  replay: AnyRecord | null,
  pitr: AnyRecord | null,
) {
  const rows = Array.isArray(files) ? files : []
  if (rows.some((row) => row && row.location === 'bucket')) return true
  return [replay, pitr].some(
    (set) => Array.isArray(set?.locations) && set.locations.includes('bucket'),
  )
}

export function shellToken(path: unknown) {
  const text = String(path)
  if (/[^A-Za-z0-9_./:@+-]/.test(text)) return `'${text.replace(/'/g, `'\\''`)}'`
  return text
}

export function restoreCommandNote(client: string) {
  if (client === 'mariadb-binlog') {
    return [
      '# Pipe stdout to mariadb in this same shell so pipefail sees a client failure.',
      '# Without --skip-gtid-strict-mode, MariaDB 11.4 stops after the first file.',
      '# Pass every file to one mariadb-binlog, in order. Do not restore one file at a time.',
      "# Verify: mariadb -Nse 'SELECT COUNT(*) FROM db.tbl; SELECT @@gtid_current_pos'",
    ].join('\n')
  }
  return [
    '# Pipe stdout to mysql in this same shell so pipefail sees a client failure.',
    '# Pass every file to one mysqlbinlog, in order. Do not restore one file at a time.',
    "# Verify: mysql -Nse 'SELECT COUNT(*) FROM db.tbl; SELECT @@gtid_executed'",
  ].join('\n')
}

// formatClientCommand matches internal/tasks formatClientCommand. stdout stays
// the client SQL. The comment tells the operator to pipe in the same shell.
export function formatClientCommand(client: string, tokens: string[]) {
  if (tokens.length === 0) return ''
  const lines: string[] = []
  if (client) {
    const flag = client === 'mariadb-binlog' ? ' --skip-gtid-strict-mode' : ''
    lines.push('set -o pipefail')
    lines.push(`TZ=UTC ${client}${flag} --verify-binlog-checksum \\`)
  }
  tokens.forEach((token, index) => {
    const indent = client || index > 0 ? '  ' : ''
    const cont = index === tokens.length - 1 ? '' : ' \\'
    lines.push(`${indent}${token}${cont}`)
  })
  if (client) lines.push(restoreCommandNote(client))
  return lines.join('\n')
}

export function formatReplayCommand(replay: AnyRecord | null) {
  const paths: string[] = Array.isArray(replay?.paths) ? replay.paths.filter(Boolean) : []
  if (paths.length === 0) return ''
  return formatClientCommand(String(replay?.client || '').trim(), paths.map(shellToken))
}

export function pendingDumpTitle(t: TFunction, task: AnyRecord) {
  const id = task?.pending_dump_cleanup?.connection_id
  const base = t('detail.pendingDumpCleanup', { id })
  if (task?.pending_dump_cleanup?.process_local) {
    return `${base} ${t('detail.pendingDumpCleanupProcessLocal')}`
  }
  return base
}

export function formatStart(start: AnyRecord | null | undefined) {
  if (!start) return '--'
  const mode = String(start.mode || '').trim()
  if (!mode) return '--'
  if (mode === 'FILE_POS') return `FILE_POS ${start.file || '-'}:${start.pos ?? 0}`
  if (mode === 'GTID') {
    const gtid = String(start.gtid_set || start.gtid || '').trim() || '--'
    return `GTID ${gtid}`
  }
  return mode
}

export function effectiveRetention(storage: AnyRecord | undefined, which: 'local' | 'bucket') {
  const base = Number(storage?.retention_days || 0)
  const specific =
    Number(which === 'local' ? storage?.local_retention_days : storage?.bucket_retention_days) || 0
  const days = specific > 0 ? specific : base
  return days > 0 ? String(days) : '--'
}

export function locationLabel(t: TFunction, location: unknown) {
  if (location === 'bucket') return t('detail.locationBucket')
  if (location === 'both') return t('detail.locationBoth')
  if (location === 'local') return t('detail.locationLocal')
  return '--'
}

export function sourceServers(task: AnyRecord | null): AnyRecord[] {
  const items = task?.source_chain?.servers
  return Array.isArray(items) ? items : []
}

export function sourceSwitches(task: AnyRecord | null): AnyRecord[] {
  const items = task?.source_chain?.switches
  return Array.isArray(items) ? items : []
}

// "resumed" means the task left that stop (it runs again): no banner.
export function showStoppedBanner(task: AnyRecord | null) {
  if (task?.source_chain?.outcome === 'stopped') return true
  return String(task?.last_error || '').startsWith('SOURCE_SWITCHOVER')
}

export function showContinuedBanner(task: AnyRecord | null) {
  return (
    task?.source_chain?.outcome === 'continued' &&
    task?.state === 'RUNNING' &&
    !showStoppedBanner(task)
  )
}

function latestStop(task: AnyRecord | null) {
  const stopped = sourceSwitches(task).filter(
    (item) => item && item.continued === false && !item.resolved,
  )
  return stopped.length ? stopped[stopped.length - 1] : null
}

export function stoppedReasonText(t: TFunction, task: AnyRecord | null) {
  const reason = String(latestStop(task)?.reason || '').trim()
  if (!reason) return t('detail.switchReason.unknown')
  return tKnown(t, `detail.switchReason.${reason}`, reason)
}

export function stoppedMove(t: TFunction, task: AnyRecord | null) {
  const sw = latestStop(task)
  if (!sw?.old || !sw?.new) return ''
  return t('detail.switchFromTo', { old: sw.old, new: sw.new })
}

export function switchWhere(sw: AnyRecord) {
  const file = String(sw?.file || '').trim()
  const pos = Number(sw?.pos || 0)
  const gtid = String(sw?.gtid_set || '').trim()
  const parts: string[] = []
  if (file && pos) parts.push(`${file}:${pos}`)
  else if (file) parts.push(file)
  if (gtid) parts.push(gtid)
  return parts.join(' · ')
}

export function fileServerLabel(t: TFunction, row: AnyRecord, servers: AnyRecord[]) {
  const id = String(row?.source_identity || '').trim()
  if (!id) return '--'
  // source_server is the stint: a failback A, B, A writes stint 3, not server 1.
  const stint = Number(row?.source_server || 0)
  const index =
    stint > 0 && stint <= servers.length
      ? stint - 1
      : servers.findIndex((server) => server && server.identity === id)
  const ordinal = index >= 0 ? t('detail.serverOrdinal', { n: index + 1 }) : ''
  const current = index >= 0 && servers[index].current ? t('detail.serverCurrent') : ''
  return [ordinal, id, current].filter(Boolean).join(' · ')
}

export function diskBase(row: AnyRecord) {
  const path = row && row.file_path ? String(row.file_path) : ''
  if (!path) return (row && row.file_name) || ''
  const parts = path.split(/[/\\]/)
  return parts[parts.length - 1] || (row && row.file_name) || ''
}

export function isLeftoverDiskTask(task: AnyRecord | null) {
  const source = task?.source || {}
  return (
    String(source.host || '').trim() === '' &&
    String(source.user || '').trim() === '' &&
    String(task?.cluster_key || '').trim() === ''
  )
}
