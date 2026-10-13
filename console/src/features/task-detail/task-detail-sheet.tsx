// Task detail drawer, ported from frontend/src/components/TaskDetailDrawer.vue
// plus its handlers in App.vue. Rendered by the /tasks/$taskId and
// /alerts/$taskId routes, so a detail link opens directly.
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  CircleAlert,
  Clock,
  FileText,
  Info,
  KeyRound,
  Link2,
  Loader2,
  TriangleAlert,
  Waves,
} from 'lucide-react'
import { toast } from 'sonner'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useObservation } from '@/context/observation'
import { useTaskForm } from '@/features/task-form/task-form-dialog'
import {
  deleteTask,
  downloadReplayArchive,
  downloadTaskFile,
  getTask,
  listFiles,
  listPITRReplay,
  retryUpload,
  startTask,
  stopTask,
  type AnyRecord,
} from '@/lib/api'
import { useI18n } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import {
  formatDelay,
  formatReplicationReason,
  formatTs,
  leaseRiskLabel,
  replicationStatusLabel,
  replicationTone,
  sourceLabel,
  stateLabel,
  stateTone,
} from '../tasks/format'
import { ToneBadge } from '../tasks/tone-badge'
import {
  damagedReplayText,
  diskBase,
  downloadErrorText,
  effectiveRetention,
  fileServerLabel,
  formatBreak,
  formatCheckpoint,
  formatReplayCommand,
  formatStart,
  isLeftoverDiskTask,
  locationLabel,
  parseErr,
  pendingDumpTitle,
  pitrErrorText,
  recoveryRange,
  replayHint,
  saveBlob,
  showBucketOnly,
  showContinuedBanner,
  showStoppedBanner,
  sourceServers,
  sourceSwitches,
  stoppedMove,
  stoppedReasonText,
  storageAlertLines,
  switchWhere,
} from './detail-format'
import { RUN_HISTORY_LIMIT, TASK_DETAIL_INVENTORY_LIMIT, useTaskDetail } from './use-task-detail'

export function TaskDetailSheet({ taskId, onClose }: { taskId: string; onClose: () => void }) {
  const { t, locale } = useI18n()
  const { refresh, nowRefMs } = useObservation()
  const { detail, error, loading, reload, setFiles } = useTaskDetail(taskId)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const task = detail?.task ?? null

  const { openEdit } = useTaskForm()

  // After Start/Stop the task passes through STARTING/STOPPING. Poll the task
  // (GET /api/tasks/{id} only) until it settles, then reload the whole drawer
  // and the list once, so the drawer no longer sticks on "Starting".
  const settleRun = useRef(0)
  useEffect(() => () => void settleRun.current++, [taskId])
  async function settle() {
    const run = ++settleRun.current
    await reload()
    let failures = 0
    for (let i = 0; i < SETTLE_MAX_POLLS; i++) {
      await new Promise((r) => setTimeout(r, SETTLE_POLL_MS))
      if (run !== settleRun.current) return
      let state = ''
      try {
        state = String((await getTask(taskId))?.state || '')
        failures = 0
      } catch {
        // A blip (network, 5xx) is retried a few times; after that stop polling
        // but still do the final reload below so the drawer is not left on STARTING/STOPPING.
        if (++failures >= SETTLE_MAX_FAILURES) break
        continue
      }
      if (run !== settleRun.current) return
      if (!TRANSITIONAL_STATES.has(state)) break
    }
    if (run !== settleRun.current) return
    await refresh()
    await reload()
  }

  async function onStart() {
    if (!task) return
    try {
      await startTask(task.id)
      toast.success(t('msg.taskStarted', { id: task.id }))
      await refresh()
    } catch (err) {
      toast.error(parseErr(err))
      return
    }
    await settle()
  }

  async function onStop() {
    if (!task) return
    try {
      await stopTask(task.id)
      toast.success(t('msg.taskStopped', { id: task.id }))
      await refresh()
    } catch (err) {
      toast.error(parseErr(err))
      return
    }
    await settle()
  }

  async function onDelete() {
    if (!task) return
    setConfirmDelete(false)
    settleRun.current++
    try {
      await deleteTask(task.id)
      toast.success(t('msg.taskDeleted', { id: task.id }))
      await refresh()
      onClose()
    } catch (err) {
      toast.error(parseErr(err))
    }
  }

  async function onRetryUpload() {
    if (!task) return
    try {
      await retryUpload(task.id, 100)
      setFiles(await listFiles(task.id, TASK_DETAIL_INVENTORY_LIMIT))
      toast.success(t('msg.retryUploadTriggered'))
    } catch (err) {
      toast.error(parseErr(err))
    }
  }

  async function download(name: string, fetcher: () => Promise<Blob>, okKey: string) {
    try {
      saveBlob(name, await fetcher())
      toast.success(t(okKey))
    } catch (err) {
      toast.error(await downloadErrorText(err))
    }
  }

  return (
    <Sheet open onOpenChange={(open) => !open && onClose()}>
      <SheetContent
        data-testid='task-drawer'
        className='w-full gap-0 overflow-y-auto p-0 sm:max-w-[min(66vw,1200px)]'
      >
        <SheetHeader className='bg-background sticky top-0 z-10 border-b'>
          <SheetTitle>
            {t('detail.center')} #{task?.id ?? taskId}
          </SheetTitle>
          <SheetDescription className='sr-only'>{t('detail.center')}</SheetDescription>
        </SheetHeader>
        {!detail && loading && (
          <div className='text-muted-foreground flex items-center gap-2 p-6 text-sm'>
            <Loader2 className='size-4 animate-spin' />
          </div>
        )}
        {!detail && !loading && error && (
          <div className='p-4'>
            <Alert variant='destructive' data-testid='task-drawer-error'>
              <CircleAlert />
              <AlertTitle>
                {t('detail.center')} #{taskId}
              </AlertTitle>
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          </div>
        )}
        {detail && task && (
          <DetailBody
            detail={detail}
            now={nowRefMs()}
            locale={locale}
            onEdit={() => openEdit(task, reload)}
            onStart={onStart}
            onStop={onStop}
            onDelete={() => setConfirmDelete(true)}
            onRetryUpload={onRetryUpload}
            onDownloadFile={(name) =>
              download(name, () => downloadTaskFile(task.id, name), 'msg.segmentDownloaded')
            }
            onDownloadReplay={() =>
              download(
                `task-${task.id}-replay.tar`,
                () => downloadReplayArchive(task.id),
                'msg.replayDownloaded',
              )
            }
            onDownloadPitr={(w) =>
              download(
                `task-${task.id}-replay.tar`,
                () => downloadReplayArchive(task.id, w),
                'msg.pitrDownloaded',
              )
            }
          />
        )}
      </SheetContent>
      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent data-testid='task-delete-confirm'>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('msg.deleteConfirmTitle')}</AlertDialogTitle>
            <AlertDialogDescription>
              {t('msg.confirmDelete', { id: task?.id ?? taskId })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t('btn.cancel')}</AlertDialogCancel>
            <AlertDialogAction
              className='bg-destructive hover:bg-destructive/90 text-white'
              onClick={() => void onDelete()}
            >
              {t('btn.delete')}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Sheet>
  )
}

function Panel({
  icon,
  title,
  children,
  ...rest
}: { icon: ReactNode; title: ReactNode; children: ReactNode } & Record<`data-${string}`, string>) {
  return (
    <section className='min-w-0 rounded-lg border p-4' {...rest}>
      <h3 className='mb-3 flex items-center gap-2 text-sm font-semibold'>
        {icon}
        {title}
      </h3>
      {children}
    </section>
  )
}

function Item({
  label,
  children,
  ...rest
}: { label: ReactNode; children: ReactNode } & Record<`data-${string}`, string>) {
  return (
    <div className='bg-muted/40 min-w-0 rounded-md px-3 py-2'>
      <span className='text-muted-foreground block text-xs'>{label}</span>
      <strong className='block text-sm font-medium break-all' {...rest}>
        {children}
      </strong>
    </div>
  )
}

const grid = 'grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-4'
const pre =
  'bg-muted mt-2 max-w-full overflow-x-auto rounded-md p-3 font-mono text-xs whitespace-pre'

const TRANSITIONAL_STATES = new Set(['STARTING', 'STOPPING'])
const SETTLE_POLL_MS = 1500
const SETTLE_MAX_POLLS = 20
const SETTLE_MAX_FAILURES = 3

function DetailBody({
  detail,
  now,
  locale,
  onEdit,
  onStart,
  onStop,
  onDelete,
  onRetryUpload,
  onDownloadFile,
  onDownloadReplay,
  onDownloadPitr,
}: {
  detail: NonNullable<ReturnType<typeof useTaskDetail>['detail']>
  now: number
  locale: string
  onEdit: () => void
  onStart: () => void
  onStop: () => void
  onDelete: () => void
  onRetryUpload: () => void
  onDownloadFile: (name: string) => void
  onDownloadReplay: () => void
  onDownloadPitr: (w: { stop: string; start: string; gtid: string; executed: string }) => void
}) {
  const { t } = useI18n()
  const { task, replication, lease, checkpoint, files, replay, recovery, events } = detail
  const runs = detail.runs.slice(0, RUN_HISTORY_LIMIT)
  const servers = sourceServers(task)
  const switches = sourceSwitches(task)
  const fmt = (ts?: string) => formatTs(locale, ts)

  const [pitr, setPitr] = useState({ stop: '', gtid: '', start: '', executed: '' })
  const [pitrResult, setPitrResult] = useState<AnyRecord | null>(null)
  const [pitrError, setPitrError] = useState('')
  useEffect(() => {
    setPitr({ stop: '', gtid: '', start: '', executed: '' })
    setPitrResult(null)
    setPitrError('')
  }, [task.id])
  const canBuildPitr = Boolean(pitr.stop.trim() || pitr.gtid.trim())
  const pitrCommand = String(pitrResult?.command || '')
  const pitrNote = String(pitrResult?.note || '').trim()
  const replayCommand = useMemo(() => formatReplayCommand(replay), [replay])
  const replayWarning = damagedReplayText(t, task, replay?.warning)
  const pitrWarning = damagedReplayText(t, task, pitrResult?.warning)
  const alertLines = storageAlertLines(t, task)
  const breaks: AnyRecord[] = Array.isArray(recovery?.breaks) ? recovery.breaks : []
  const recoveryGtid = String(recovery?.gtid_set || '').trim()
  const stopped = showStoppedBanner(task)
  const continued = showContinuedBanner(task)
  const move = stoppedMove(t, task)

  async function buildPitr() {
    const stop = pitr.stop.trim()
    const gtid = pitr.gtid.trim()
    if (!stop && !gtid) return
    setPitrError('')
    setPitrResult(null)
    try {
      setPitrResult(
        await listPITRReplay(task.id, {
          stop,
          start: pitr.start.trim(),
          gtid,
          executed: pitr.executed.trim(),
        }),
      )
    } catch (err) {
      setPitrError(pitrErrorText(t, err))
    }
  }

  async function copy(text: string, ok: string, fail: string) {
    if (!text) return
    try {
      await navigator.clipboard.writeText(text)
      toast.success(t(ok))
    } catch {
      toast.error(t(fail))
    }
  }

  return (
    <div className='flex min-w-0 flex-col gap-4 p-4'>
      <section className='min-w-0 rounded-lg border p-4'>
        <div className='flex flex-wrap items-start justify-between gap-3'>
          <div className='min-w-0'>
            <div className='text-muted-foreground text-xs'>{t('detail.center')}</div>
            <h3 className='flex items-center gap-2 text-lg font-semibold break-all'>
              <Info className='size-4 shrink-0' />
              {task.name}
            </h3>
            <div className='text-muted-foreground mt-2 flex flex-wrap items-center gap-2 text-xs'>
              <ToneBadge tone={stateTone(task.state)} data-testid='task-drawer-status'>
                {stateLabel(t, task.state)}
              </ToneBadge>
              {replication && (
                <ToneBadge
                  tone={replicationTone(replication.status)}
                  data-testid='task-drawer-replication'
                >
                  {replicationStatusLabel(t, replication.status)}
                </ToneBadge>
              )}
              <span data-testid='task-drawer-source'>
                {t('detail.source')} {sourceLabel(task)}
              </span>
              <span>
                {t('detail.clusterKey')} {task.cluster_key || '--'}
              </span>
            </div>
          </div>
          <div className='flex flex-wrap gap-2' data-testid='task-drawer-actions'>
            {isLeftoverDiskTask(task) ? (
              <Button size='sm' variant='outline' data-testid='task-action-adopt' onClick={onEdit}>
                {t('btn.adopt')}
              </Button>
            ) : (
              <Button size='sm' variant='outline' data-testid='task-action-edit' onClick={onEdit}>
                {t('btn.edit')}
              </Button>
            )}
            <Button
              size='sm'
              className='bg-emerald-600 text-white hover:bg-emerald-700'
              data-testid='task-action-start'
              onClick={onStart}
            >
              {t('btn.start')}
            </Button>
            <Button
              size='sm'
              className='bg-amber-500 text-white hover:bg-amber-600'
              data-testid='task-action-stop'
              onClick={onStop}
            >
              {t('btn.stop')}
            </Button>
            <Button
              size='sm'
              variant='outline'
              className='border-red-300 text-red-600 hover:bg-red-50'
              data-testid='task-action-delete'
              onClick={onDelete}
            >
              {t('btn.delete')}
            </Button>
          </div>
        </div>
        <div className={cn(grid, 'mt-3')}>
          <Item label={t('detail.currentState')}>{stateLabel(t, task.state)}</Item>
          <Item label={t('detail.replicationStatus')}>
            {replication ? replicationStatusLabel(t, replication.status) : '--'}
          </Item>
          <Item label={t('detail.delay')}>
            {replication
              ? `${formatDelay(replication.delay_seconds, replication.has_progress)} ${t('detail.seconds')}`
              : '--'}
          </Item>
          <Item label={t('detail.leaseStatus')}>{leaseRiskLabel(t, task, now, lease)}</Item>
        </div>
        {task.storage_alert && (
          <Alert variant='destructive' className='mt-3' data-testid='task-storage-alert'>
            <CircleAlert />
            <AlertTitle>{t('detail.storageAlert')}</AlertTitle>
            <AlertDescription>
              {alertLines.map((line, i) => (
                <p key={i} data-testid='task-storage-alert-line'>
                  {line}
                </p>
              ))}
            </AlertDescription>
          </Alert>
        )}
        {task.pending_dump_cleanup?.connection_id && (
          <Alert
            className='mt-3 border-amber-300 text-amber-800'
            data-testid='task-pending-dump-cleanup'
          >
            <TriangleAlert />
            <AlertTitle className='line-clamp-none'>{pendingDumpTitle(t, task)}</AlertTitle>
          </Alert>
        )}
        {continued && (
          <Alert
            className='mt-3 border-amber-300 text-amber-800'
            data-testid='task-source-continued'
          >
            <TriangleAlert />
            <AlertTitle className='line-clamp-none'>{t('detail.sourceContinuedTitle')}</AlertTitle>
            <AlertDescription>{t('detail.sourceContinuedBody')}</AlertDescription>
          </Alert>
        )}
        {stopped && (
          <Alert variant='destructive' className='mt-3' data-testid='task-source-stopped'>
            <CircleAlert />
            <AlertTitle className='line-clamp-none'>{stoppedReasonText(t, task)}</AlertTitle>
            <AlertDescription>
              {move && <p data-testid='task-source-stopped-move'>{move}</p>}
              <p data-testid='task-source-next'>{t('detail.sourceStoppedNext')}</p>
            </AlertDescription>
          </Alert>
        )}
      </section>

      {replication && (
        <Panel icon={<Waves className='size-4' />} title={t('detail.replicationAndPosition')}>
          <div className={grid}>
            <Item label={t('detail.replicationStatus')}>
              <ToneBadge tone={replicationTone(replication.status)}>
                {replicationStatusLabel(t, replication.status)}
              </ToneBadge>
            </Item>
            <Item label={t('detail.delay')}>
              {formatDelay(replication.delay_seconds, replication.has_progress)}{' '}
              {t('detail.seconds')}
            </Item>
            <Item label={t('detail.checkpoint')} data-testid='task-drawer-checkpoint'>
              {formatCheckpoint(t, checkpoint)}
            </Item>
            <Item label={t('detail.errorReason')}>{formatReplicationReason(t, replication)}</Item>
            <Item label={t('detail.alertThreshold')}>
              {replication.threshold_seconds || '--'} {t('detail.seconds')}
            </Item>
            <Item label={t('detail.lastEventTime')}>{fmt(replication.last_event_at)}</Item>
            <Item label={t('detail.lastPosition')}>
              {replication.last_event_file || '-'}:{replication.last_event_pos || 0}
            </Item>
            <Item label={t('detail.statusCode')}>{replication.reason || '--'}</Item>
          </div>
        </Panel>
      )}

      <Panel icon={<Info className='size-4' />} title={t('detail.basicInfo')}>
        <div className={grid}>
          <Item label={t('table.name')}>{task.name}</Item>
          <Item label={t('detail.clusterKey')}>{task.cluster_key || '--'}</Item>
          <Item label={t('detail.source')}>{sourceLabel(task)}</Item>
          <Item label={t('detail.startMode')} data-testid='task-drawer-start'>
            {formatStart(task.start)}
          </Item>
          <Item label={t('detail.checkpoint')} data-testid='task-drawer-resume'>
            {formatCheckpoint(t, checkpoint)}
          </Item>
          <Item label={t('form.semiSync')}>
            {task.source?.semi_sync ? t('detail.on') : t('detail.off')}
          </Item>
          <Item label={t('form.retentionDays')} data-testid='task-drawer-retention'>
            {task.storage?.retention_days || '--'}
          </Item>
          <Item label={t('form.localRetentionDays')} data-testid='task-drawer-local-retention'>
            {effectiveRetention(task.storage, 'local')}
          </Item>
          <Item label={t('form.bucketRetentionDays')} data-testid='task-drawer-bucket-retention'>
            {effectiveRetention(task.storage, 'bucket')}
          </Item>
        </div>
      </Panel>

      {lease && (
        <Panel icon={<KeyRound className='size-4' />} title={t('detail.leaseAndWorker')}>
          <div className={grid}>
            <Item label={t('detail.ownerWorker')} data-testid='task-drawer-worker'>
              {lease.owner_worker_id || '--'}
            </Item>
            <Item label={t('detail.epoch')}>{lease.epoch || '--'}</Item>
            <Item label={t('detail.leaseStatus')}>{leaseRiskLabel(t, task, now, lease)}</Item>
            <Item label={t('detail.updatedAt')}>{fmt(lease.updated_at)}</Item>
          </div>
        </Panel>
      )}

      {(servers.length > 0 || switches.length > 0) && (
        <Panel
          icon={<Link2 className='size-4' />}
          title={t('detail.sourceChain')}
          data-testid='task-source-chain'
        >
          <p className='text-muted-foreground mb-2 text-xs'>{t('detail.sourceChainHint')}</p>
          <ul className='flex flex-col gap-1 text-sm'>
            {servers.map((server, index) => (
              <li
                key={`${server.identity}-${index}`}
                data-testid='task-source-server'
                className='flex flex-wrap items-center gap-2'
              >
                {t('detail.serverOrdinal', { n: index + 1 })}
                <code className='bg-muted rounded px-1 break-all'>{server.identity}</code>
                {server.prefix && server.prefix !== server.identity && (
                  <span className='text-muted-foreground text-xs' data-testid='task-source-stint'>
                    {t('detail.serverPrefix', { prefix: server.prefix })}
                  </span>
                )}
                {server.current && (
                  <ToneBadge tone='success' data-testid='task-source-current'>
                    {t('detail.serverCurrent')}
                  </ToneBadge>
                )}
              </li>
            ))}
          </ul>
          {switches.length > 0 && (
            <ul className='mt-2 flex flex-col gap-1 text-sm'>
              {switches.map((sw, index) => (
                <li
                  key={`${sw.old}-${sw.new}-${index}`}
                  data-testid='task-source-switch'
                  className='flex flex-wrap items-center gap-2'
                >
                  <span>{fmt(sw.time)}</span>
                  <strong className='break-all'>
                    {sw.old} → {sw.new}
                  </strong>
                  {switchWhere(sw) && <span className='break-all'>{switchWhere(sw)}</span>}
                  <ToneBadge tone={sw.continued ? 'success' : sw.resolved ? 'info' : 'danger'}>
                    {sw.continued
                      ? t('detail.switchContinued')
                      : sw.resolved
                        ? t('detail.switchResumed')
                        : t('detail.switchStopped')}
                  </ToneBadge>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      )}

      <Panel icon={<FileText className='size-4' />} title={t('detail.filesAndUpload')}>
        <div className='rounded-md border p-3' data-testid='task-recovery-window'>
          <div className='flex flex-wrap items-start justify-between gap-2'>
            <div className='min-w-0'>
              <strong className='text-sm'>{t('detail.recoveryWindow')}</strong>
              <p className='text-muted-foreground text-xs'>{t('detail.recoveryHint')}</p>
            </div>
            {recovery && (
              <ToneBadge
                tone={recovery.continuous ? 'success' : 'warning'}
                data-testid='task-recovery-continuous'
              >
                {recovery.continuous ? t('detail.recoveryContinuous') : t('detail.recoveryBroken')}
              </ToneBadge>
            )}
          </div>
          <p className='mt-2 font-mono text-sm break-all' data-testid='task-recovery-range'>
            {recoveryRange(t, recovery)}
          </p>
          {recoveryGtid && (
            <p className='text-muted-foreground text-xs break-all' data-testid='task-recovery-gtid'>
              {t('detail.recoveryGtid')}: {recoveryGtid}
            </p>
          )}
          {breaks.length > 0 && (
            <Alert
              className='mt-2 border-amber-300 text-amber-800'
              data-testid='task-recovery-breaks'
            >
              <TriangleAlert />
              <AlertTitle>{t('detail.recoveryBreaks')}</AlertTitle>
              <AlertDescription>
                <ul className='list-disc pl-4'>
                  {breaks.map((item, index) => (
                    <li key={index} data-testid='task-recovery-break' className='break-all'>
                      {formatBreak(item)}
                    </li>
                  ))}
                </ul>
              </AlertDescription>
            </Alert>
          )}
        </div>

        <div className='mt-3 rounded-md border p-3' data-testid='task-replay'>
          <div className='flex flex-wrap items-start justify-between gap-2'>
            <div className='min-w-0'>
              <strong className='text-sm'>{t('detail.replay')}</strong>
              <p className='text-muted-foreground text-xs' data-testid='task-replay-hint'>
                {replayHint(t, replay)}
              </p>
            </div>
            <div className='flex flex-wrap gap-2'>
              <Button
                size='sm'
                variant='outline'
                data-testid='task-replay-copy'
                disabled={!replayCommand}
                onClick={() => copy(replayCommand, 'msg.replayCopied', 'msg.replayCopyFailed')}
              >
                {t('btn.copyReplay')}
              </Button>
              <Button
                size='sm'
                variant='outline'
                data-testid='task-replay-download'
                onClick={onDownloadReplay}
              >
                {t('btn.downloadReplay')}
              </Button>
            </div>
          </div>
          {replayWarning && (
            <Alert
              className='mt-2 border-amber-300 text-amber-800'
              data-testid='task-replay-damaged'
            >
              <TriangleAlert />
              <AlertTitle className='line-clamp-none'>{replayWarning}</AlertTitle>
            </Alert>
          )}
          {replayCommand ? (
            <pre className={pre} data-testid='task-replay-command'>
              {replayCommand}
            </pre>
          ) : (
            <p className='text-muted-foreground mt-2 text-sm' data-testid='task-replay-command'>
              {t('detail.replayEmpty')}
            </p>
          )}

          <div className='mt-4 border-t pt-3' data-testid='task-pitr'>
            <strong className='text-sm'>{t('detail.pitr')}</strong>
            <p className='text-muted-foreground text-xs'>{t('detail.pitrHint')}</p>
            <div className='mt-2 grid grid-cols-1 gap-2 sm:grid-cols-2'>
              <Input
                data-testid='task-pitr-stop'
                value={pitr.stop}
                placeholder={t('detail.pitrStop')}
                onChange={(e) => setPitr((p) => ({ ...p, stop: e.target.value }))}
              />
              <Input
                data-testid='task-pitr-gtid'
                value={pitr.gtid}
                placeholder={t('detail.pitrGtid')}
                onChange={(e) => setPitr((p) => ({ ...p, gtid: e.target.value }))}
              />
              <Input
                data-testid='task-pitr-start'
                value={pitr.start}
                placeholder={t('detail.pitrStart')}
                onChange={(e) => setPitr((p) => ({ ...p, start: e.target.value }))}
              />
              <Input
                data-testid='task-pitr-executed'
                value={pitr.executed}
                placeholder={t('detail.pitrExecuted')}
                onChange={(e) => setPitr((p) => ({ ...p, executed: e.target.value }))}
              />
            </div>
            <div className='mt-2 flex flex-wrap gap-2'>
              <Button
                size='sm'
                variant='outline'
                data-testid='task-pitr-build'
                disabled={!canBuildPitr}
                onClick={() => void buildPitr()}
              >
                {t('btn.buildPitr')}
              </Button>
              <Button
                size='sm'
                variant='outline'
                data-testid='task-pitr-copy'
                disabled={!pitrCommand}
                onClick={() => copy(pitrCommand, 'msg.pitrCopied', 'msg.pitrCopyFailed')}
              >
                {t('btn.copyPitr')}
              </Button>
              <Button
                size='sm'
                variant='outline'
                data-testid='task-pitr-download'
                disabled={!canBuildPitr}
                onClick={() =>
                  onDownloadPitr({
                    stop: pitr.stop.trim(),
                    start: pitr.start.trim(),
                    gtid: pitr.gtid.trim(),
                    executed: pitr.executed.trim(),
                  })
                }
              >
                {t('btn.downloadPitr')}
              </Button>
            </div>
            {pitrWarning && (
              <p className='text-muted-foreground mt-2 text-sm' data-testid='task-pitr-damaged'>
                {pitrWarning}
              </p>
            )}
            {pitrError ? (
              <p className='mt-2 text-sm break-all text-red-600' data-testid='task-pitr-error'>
                {pitrError}
              </p>
            ) : pitrCommand ? (
              <pre className={pre} data-testid='task-pitr-command'>
                {pitrCommand}
              </pre>
            ) : pitrNote ? (
              <p className='text-muted-foreground mt-2 text-sm' data-testid='task-pitr-command'>
                {pitrNote}
              </p>
            ) : pitrResult ? (
              <p className='text-muted-foreground mt-2 text-sm' data-testid='task-pitr-command'>
                {t('detail.pitrEmpty')}
              </p>
            ) : null}
          </div>
        </div>

        <div className='mt-3 flex flex-wrap gap-2'>
          {files.some((f) => f.upload_state === 'UPLOAD_FAILED') && (
            <Button
              size='sm'
              className='bg-amber-500 text-white hover:bg-amber-600'
              data-testid='retry-upload-action'
              onClick={onRetryUpload}
            >
              {t('btn.retryUpload')}
            </Button>
          )}
        </div>
        {showBucketOnly(files, replay, pitrResult) && (
          <p className='text-muted-foreground mt-2 text-sm' data-testid='task-bucket-only-hint'>
            {t('detail.bucketOnly')}
          </p>
        )}
        <div className='mt-2 overflow-x-auto rounded-md border'>
          <Table data-testid='task-files'>
            <TableHeader>
              <TableRow>
                <TableHead>{t('table.file')}</TableHead>
                <TableHead>{t('table.server')}</TableHead>
                <TableHead>{t('table.filePath')}</TableHead>
                <TableHead>{t('table.size')}</TableHead>
                <TableHead>{t('table.startPos')}</TableHead>
                <TableHead>{t('table.endPos')}</TableHead>
                <TableHead>{t('table.location')}</TableHead>
                <TableHead>{t('table.uploadState')}</TableHead>
                <TableHead>{t('table.checksum')}</TableHead>
                <TableHead>{t('table.objectKey')}</TableHead>
                <TableHead>{t('table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {files.map((row, i) => {
                const base = diskBase(row)
                return (
                  <TableRow key={`${base}-${i}`}>
                    <TableCell data-testid={`file-disk-name-${base}`}>{base}</TableCell>
                    <TableCell data-testid={`file-source-${base}`}>
                      {fileServerLabel(t, row, servers)}
                    </TableCell>
                    <TableCell data-testid='file-disk-path'>{row.file_path}</TableCell>
                    <TableCell>{row.size_bytes}</TableCell>
                    <TableCell>{row.start_pos}</TableCell>
                    <TableCell>{row.end_pos}</TableCell>
                    <TableCell data-testid={`file-location-${base}`}>
                      {locationLabel(t, row.location)}
                    </TableCell>
                    <TableCell data-testid={`file-upload-state-${base}`}>
                      {row.upload_state}
                    </TableCell>
                    <TableCell data-testid={`file-checksum-${base}`}>
                      {row.checksum || '--'}
                    </TableCell>
                    <TableCell>{row.object_key}</TableCell>
                    <TableCell>
                      {base && (
                        <Button
                          size='sm'
                          variant='outline'
                          data-testid={`file-download-${base}`}
                          onClick={() => onDownloadFile(base)}
                        >
                          {t('btn.download')}
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </div>
      </Panel>

      <Panel
        icon={<Clock className='size-4' />}
        title={t('detail.runHistory', { limit: RUN_HISTORY_LIMIT })}
        data-testid='task-drawer-runs'
      >
        <div className='overflow-x-auto rounded-md border'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('table.runId')}</TableHead>
                <TableHead>{t('table.worker')}</TableHead>
                <TableHead>{t('table.epoch')}</TableHead>
                <TableHead>{t('table.startTime')}</TableHead>
                <TableHead>{t('table.endTime')}</TableHead>
                <TableHead>{t('table.endReason')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.map((row, i) => (
                <TableRow key={`${row.run_id}-${i}`}>
                  <TableCell>{row.run_id}</TableCell>
                  <TableCell>{row.worker_id}</TableCell>
                  <TableCell>{row.epoch}</TableCell>
                  <TableCell>{fmt(row.started_at)}</TableCell>
                  <TableCell>{fmt(row.ended_at)}</TableCell>
                  <TableCell>{row.end_reason || '--'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {runs.length === 0 && (
            <p className='text-muted-foreground py-6 text-center text-sm'>
              {t('empty.noRunHistory')}
            </p>
          )}
        </div>
      </Panel>

      <Panel
        icon={<Clock className='size-4' />}
        title={t('detail.eventTimeline')}
        data-testid='task-drawer-events'
      >
        <ol className='border-muted relative ml-2 border-l'>
          {events.map((ev) => (
            <li key={`${ev.sequence}-${ev.type}`} className='mb-3 ml-4'>
              <span className='bg-primary absolute -left-1.5 mt-1.5 size-3 rounded-full border-2 border-white' />
              <time className='text-muted-foreground text-xs'>{ev.time}</time>
              <p className='text-sm font-semibold'>{ev.type}</p>
              <p className='text-sm break-all'>{ev.message}</p>
            </li>
          ))}
        </ol>
      </Panel>
    </div>
  )
}
