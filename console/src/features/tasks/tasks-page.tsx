// Tasks / Alerts view, ported from the tasks|alerts branch of App.vue:
// KPI cards, current-page filter panel, server-paged task table.
import { useMemo, type ReactNode } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Info, SlidersHorizontal, Table2, Bell, ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'
import {
  REPLICATION_STATUSES,
  TASK_STATES,
  formatDelay,
  formatReplicationReason,
  formatTs,
  leaseRiskLabel,
  leaseRiskTone,
  ownerWorkerLabel,
  replicationStatusLabel,
  replicationTone,
  sourceLabel,
  stateLabel,
  stateTone,
} from './format'
import { PAGE_SIZES, filterRows, quickFilterState, type QuickFilter, type UiFilter } from './filter'
import { useTaskFilter } from './filter-context'
import { MetricGrid } from './metric-grid'
import { ToneBadge } from './tone-badge'
import { useDebounced } from './use-debounced'

export function TasksPage({ view, children }: { view: 'tasks' | 'alerts'; children?: ReactNode }) {
  const { t, locale } = useI18n()
  const navigate = useNavigate()
  const { dashboard, nowRefMs } = useObservation()
  const { filter, setFilter, pager, setPager, activeQuick, setActiveQuick } = useTaskFilter()
  const kw = useDebounced(filter.keyword, 300)
  const skw = useDebounced(filter.sourceKeyword, 300)
  const rows = useMemo(
    () => filterRows(dashboard.tasks, filter, kw, skw),
    [dashboard.tasks, filter, kw, skw],
  )
  const now = nowRefMs()
  const total = dashboard.total

  function update(patch: Partial<UiFilter>) {
    setFilter((f) => ({ ...f, ...patch }))
  }

  function applyQuick(kind: QuickFilter) {
    setActiveQuick(kind)
    setFilter((f) => quickFilterState(kind, f))
    void navigate({ to: kind === 'alert' ? '/alerts' : '/tasks' })
  }

  function openDetail(id: string | number) {
    void navigate({
      to: view === 'alerts' ? '/alerts/$taskId' : '/tasks/$taskId',
      params: { taskId: String(id) },
    })
  }

  const pageCount = Math.max(1, Math.ceil(total / pager.pageSize))

  return (
    <div className='flex flex-col gap-4'>
      <MetricGrid summary={dashboard.summary} active={activeQuick} onFilter={applyQuick} />

      <div className='grid gap-4 lg:grid-cols-[280px_minmax(0,1fr)]'>
        <Card className='gap-3 self-start py-4'>
          <CardHeader className='px-4'>
            <CardTitle className='flex items-center gap-2 text-sm'>
              <SlidersHorizontal className='size-4' />
              {t('filter.title')}
            </CardTitle>
            <p className='text-muted-foreground text-xs'>{t('filter.hint')}</p>
          </CardHeader>
          <CardContent className='flex flex-col gap-3 px-4'>
            <div className='flex flex-wrap gap-1.5'>
              <Button
                size='sm'
                variant={filter.onlyAlert ? 'default' : 'outline'}
                onClick={() => applyQuick('alert')}
              >
                {t('btn.alertOnly')}
              </Button>
              <Button size='sm' variant='outline' onClick={() => applyQuick('abnormal')}>
                {t('metrics.abnormal')}
              </Button>
              <Button size='sm' variant='outline' onClick={() => applyQuick('failed')}>
                {t('metrics.failed')}
              </Button>
              <Button size='sm' variant='outline' onClick={() => applyQuick('delayed')}>
                {t('metrics.delayed')}
              </Button>
            </div>
            <p className='text-muted-foreground text-xs' data-testid='filter-scope-note'>
              {t('filter.scopeNote')}
            </p>
            <Input
              value={filter.keyword}
              placeholder={t('placeholder.searchTask')}
              onChange={(e) => update({ keyword: e.target.value })}
            />
            <Label className='text-xs'>{t('filter.taskStateScope')}</Label>
            <Select value={filter.taskState} onValueChange={(v) => update({ taskState: v })}>
              <SelectTrigger className='w-full' data-testid='filter-task-state'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='ALL'>{t('filter.allTaskStates')}</SelectItem>
                {TASK_STATES.map((s) => (
                  <SelectItem key={s} value={s}>
                    {stateLabel(t, s)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={filter.replicationStatus}
              onValueChange={(v) => update({ replicationStatus: v })}
            >
              <SelectTrigger className='w-full' data-testid='filter-replication-status'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='ALL'>{t('filter.allReplicationStatuses')}</SelectItem>
                {REPLICATION_STATUSES.map((s) => (
                  <SelectItem key={s} value={s}>
                    {replicationStatusLabel(t, s)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Input
              value={filter.sourceKeyword}
              placeholder={t('placeholder.filterSource')}
              onChange={(e) => update({ sourceKeyword: e.target.value })}
            />
            <Label className='text-xs'>{t('filter.sortHint')}</Label>
            <Select
              value={filter.sortBy}
              onValueChange={(v) => update({ sortBy: v as UiFilter['sortBy'] })}
            >
              <SelectTrigger className='w-full'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='delay_desc'>{t('filter.sortByDelay')}</SelectItem>
                <SelectItem value='updated_desc'>{t('filter.sortByUpdated')}</SelectItem>
                <SelectItem value='name_asc'>{t('filter.sortByName')}</SelectItem>
              </SelectContent>
            </Select>
            <div className='flex items-center justify-between gap-2 text-sm'>
              <span>{t('filter.alertFirst')}</span>
              <Switch
                checked={filter.onlyAlert}
                onCheckedChange={(v) => update({ onlyAlert: v })}
              />
            </div>
            <p
              className='text-muted-foreground text-xs'
              data-testid='filter-summary'
              aria-live='polite'
              aria-atomic='true'
            >
              {t('filter.showingCount', { count: rows.length })}
            </p>
            <Button
              variant='outline'
              onClick={() => {
                setFilter(quickFilterState('all', filter))
                setActiveQuick('all')
              }}
            >
              {t('btn.resetFilter')}
            </Button>
          </CardContent>
        </Card>

        <Card className='min-w-0 gap-3 py-4'>
          <CardHeader className='flex flex-row items-center justify-between gap-2 px-4'>
            <CardTitle className='flex items-center gap-2 text-sm'>
              {view === 'alerts' ? <Bell className='size-4' /> : <Table2 className='size-4' />}
              {view === 'alerts' ? t('nav.alerts') : t('nav.tasks')}
            </CardTitle>
            <span className='text-muted-foreground text-xs' data-testid='task-filter-count'>
              {view === 'alerts'
                ? t('filter.alertFilteredCount', { filtered: rows.length, total })
                : t('filter.filteredCount', { filtered: rows.length, total })}
            </span>
          </CardHeader>
          <CardContent className='px-4'>
            <div className='overflow-x-auto rounded-md border'>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className='w-[70px]'>{t('table.id')}</TableHead>
                    <TableHead className='min-w-[180px]'>{t('table.name')}</TableHead>
                    <TableHead>{t('table.taskState')}</TableHead>
                    <TableHead>{t('table.ownerWorker')}</TableHead>
                    <TableHead>{t('table.leaseRisk')}</TableHead>
                    <TableHead>{t('table.replicationStatus')}</TableHead>
                    <TableHead>{t('table.delay')}</TableHead>
                    <TableHead>{t('table.source')}</TableHead>
                    <TableHead>{t('table.lastEventTime')}</TableHead>
                    <TableHead>{t('table.semiSync')}</TableHead>
                    <TableHead className='bg-background sticky right-0'>{t('table.actions')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((row) => {
                    const reason = formatReplicationReason(t, row.replication)
                    return (
                      <TableRow
                        key={String(row.task.id)}
                        className={`task-row task-row-${row.task.id} cursor-pointer`}
                        tabIndex={0}
                        onClick={() => openDetail(row.task.id)}
                        onKeyDown={(e) => {
                          if (e.key !== 'Enter' || e.target !== e.currentTarget) return
                          // Otherwise the same Enter press lands on the drawer's
                          // close button once the drawer takes focus.
                          e.preventDefault()
                          openDetail(row.task.id)
                        }}
                      >
                        <TableCell>
                          <span data-testid={`task-row-${row.task.id}`}>{row.task.id}</span>
                        </TableCell>
                        <TableCell className='font-medium'>{row.task.name}</TableCell>
                        <TableCell>
                          <div className='flex flex-wrap gap-1'>
                            <ToneBadge tone={stateTone(row.task.state)}>
                              {stateLabel(t, row.task.state)}
                            </ToneBadge>
                            {row.task.storage_alert && (
                              <ToneBadge tone='danger' data-testid='task-storage-alert-row'>
                                {t('detail.storageAlert')}
                              </ToneBadge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell>{ownerWorkerLabel(row.task)}</TableCell>
                        <TableCell>
                          <ToneBadge tone={leaseRiskTone(row.task, now)}>
                            {leaseRiskLabel(t, row.task, now)}
                          </ToneBadge>
                        </TableCell>
                        <TableCell>
                          <div className='flex items-center gap-1'>
                            <ToneBadge tone={replicationTone(row.replication?.status)}>
                              {replicationStatusLabel(t, row.replication?.status)}
                            </ToneBadge>
                            {reason !== '--' && (
                              <Tooltip>
                                <TooltipTrigger aria-label={reason}>
                                  <Info className='text-muted-foreground size-3.5' />
                                </TooltipTrigger>
                                <TooltipContent className='max-w-sm'>{reason}</TooltipContent>
                              </Tooltip>
                            )}
                          </div>
                        </TableCell>
                        <TableCell className='tabular-nums'>
                          {formatDelay(row.replication?.delay_seconds, row.replication?.has_progress)}
                        </TableCell>
                        <TableCell>{sourceLabel(row.task)}</TableCell>
                        <TableCell>{formatTs(locale, row.replication?.last_event_at)}</TableCell>
                        <TableCell>
                          {row.task.source?.semi_sync ? t('detail.on') : t('detail.off')}
                        </TableCell>
                        <TableCell className='bg-background sticky right-0'>
                          <Button
                            size='sm'
                            variant='outline'
                            data-testid={`task-detail-trigger-${row.task.id}`}
                            onClick={(e) => {
                              e.stopPropagation()
                              openDetail(row.task.id)
                            }}
                          >
                            {t('btn.detail')}
                          </Button>
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>
              {rows.length === 0 && (
                <p
                  className='text-muted-foreground py-10 text-center text-sm'
                  data-testid='task-table-empty'
                >
                  {t('empty.noTasks')}
                </p>
              )}
            </div>

            <div className='mt-3 flex flex-wrap items-center justify-end gap-2 text-sm'>
              <span className='text-muted-foreground'>{t('pagination.total', { total })}</span>
              <Select
                value={String(pager.pageSize)}
                onValueChange={(v) => setPager({ page: 1, pageSize: Number(v) })}
              >
                <SelectTrigger size='sm' className='w-[110px]' data-testid='pager-size'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {PAGE_SIZES.map((s) => (
                    <SelectItem key={s} value={String(s)}>
                      {t('pagination.perPage', { size: s })}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Button
                size='icon'
                variant='outline'
                className='size-8'
                aria-label={t('pagination.prev')}
                disabled={pager.page <= 1}
                onClick={() => setPager((p) => ({ ...p, page: p.page - 1 }))}
              >
                <ChevronLeft />
              </Button>
              {pageWindow(pager.page, pageCount).map((n, i) =>
                n === null ? (
                  <span key={`gap-${i}`} className='text-muted-foreground px-1'>
                    …
                  </span>
                ) : (
                  <Button
                    key={n}
                    size='sm'
                    variant={n === pager.page ? 'default' : 'outline'}
                    className='h-8 min-w-8 px-2'
                    onClick={() => setPager((p) => ({ ...p, page: n }))}
                  >
                    {n}
                  </Button>
                ),
              )}
              <Button
                size='icon'
                variant='outline'
                className='size-8'
                aria-label={t('pagination.next')}
                disabled={pager.page >= pageCount}
                onClick={() => setPager((p) => ({ ...p, page: p.page + 1 }))}
              >
                <ChevronRight />
              </Button>
            </div>
          </CardContent>
        </Card>
      </div>
      {children}
    </div>
  )
}

function pageWindow(page: number, count: number): (number | null)[] {
  if (count <= 7) return Array.from({ length: count }, (_, i) => i + 1)
  const out: (number | null)[] = [1]
  const start = Math.max(2, page - 2)
  const end = Math.min(count - 1, page + 2)
  if (start > 2) out.push(null)
  for (let n = start; n <= end; n++) out.push(n)
  if (end < count - 1) out.push(null)
  out.push(count)
  return out
}
