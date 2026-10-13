// Tasks / Alerts view, ported from the tasks|alerts branch of App.vue:
// KPI cards, current-page filter panel, server-paged task table.
import { useMemo, type ReactNode } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Info, SlidersHorizontal, Table2, Bell, ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Panel } from '@/components/panel'
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
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { DataTable, type Column } from '@/components/data-table'
import { useObservation } from '@/context/observation'
import type { DashboardRow } from '@/lib/api'
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
import { PAGE_SIZES, filterRows, quickFilterState, type UiFilter } from './filter'
import { useApplyQuickFilter, useTaskFilter } from './filter-context'
import { ToneBadge } from './tone-badge'
import { useDebounced } from '@/hooks/use-debounced'

export function TasksPage({ view, children }: { view: 'tasks' | 'alerts'; children?: ReactNode }) {
  const { t, locale } = useI18n()
  const navigate = useNavigate()
  const { dashboard, nowRefMs } = useObservation()
  const { filter, setFilter, pager, setPager, setActiveQuick } = useTaskFilter()
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

  const applyQuick = useApplyQuickFilter()

  function openDetail(id: string | number) {
    void navigate({
      to: view === 'alerts' ? '/alerts/$taskId' : '/tasks/$taskId',
      params: { taskId: String(id) },
    })
  }

  const pageCount = Math.max(1, Math.ceil(total / pager.pageSize))

  const sticky = 'bg-background sticky right-0'
  const columns: Column<DashboardRow>[] = [
    {
      key: 'id',
      header: t('table.id'),
      headClassName: 'w-[70px]',
      cell: (row) => <span data-testid={`task-row-${row.task.id}`}>{row.task.id}</span>,
    },
    {
      key: 'name',
      header: t('table.name'),
      headClassName: 'min-w-[180px]',
      className: 'font-medium',
      cell: (row) => row.task.name,
    },
    {
      key: 'state',
      header: t('table.taskState'),
      cell: (row) => (
        <div className='flex flex-wrap gap-1'>
          <ToneBadge tone={stateTone(row.task.state)}>{stateLabel(t, row.task.state)}</ToneBadge>
          {row.task.storage_alert && (
            <ToneBadge tone='danger' data-testid='task-storage-alert-row'>
              {t('detail.storageAlert')}
            </ToneBadge>
          )}
        </div>
      ),
    },
    { key: 'owner', header: t('table.ownerWorker'), cell: (row) => ownerWorkerLabel(row.task) },
    {
      key: 'lease',
      header: t('table.leaseRisk'),
      cell: (row) => (
        <ToneBadge tone={leaseRiskTone(row.task, now)}>
          {leaseRiskLabel(t, row.task, now)}
        </ToneBadge>
      ),
    },
    {
      key: 'replication',
      header: t('table.replicationStatus'),
      cell: (row) => {
        const reason = formatReplicationReason(t, row.replication)
        return (
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
        )
      },
    },
    {
      key: 'delay',
      header: t('table.delay'),
      className: 'tabular-nums',
      cell: (row) => formatDelay(row.replication?.delay_seconds, row.replication?.has_progress),
    },
    { key: 'source', header: t('table.source'), cell: (row) => sourceLabel(row.task) },
    {
      key: 'lastEvent',
      header: t('table.lastEventTime'),
      cell: (row) => formatTs(locale, row.replication?.last_event_at),
    },
    {
      key: 'semi',
      header: t('table.semiSync'),
      cell: (row) => (row.task.source?.semi_sync ? t('detail.on') : t('detail.off')),
    },
    {
      key: 'actions',
      header: t('table.actions'),
      className: sticky,
      cell: (row) => (
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
      ),
    },
  ]

  return (
    <div className='flex flex-col gap-4'>
      <div className='grid gap-4 lg:grid-cols-[280px_minmax(0,1fr)]'>
        <Panel
          icon={SlidersHorizontal}
          title={t('filter.title')}
          hint={t('filter.hint')}
          className='self-start'
        >
          <div className='flex flex-col gap-3'>
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
          </div>
        </Panel>

        <Panel
          icon={view === 'alerts' ? Bell : Table2}
          title={view === 'alerts' ? t('nav.alerts') : t('nav.tasks')}
          hint={
            <span data-testid='task-filter-count'>
              {view === 'alerts'
                ? t('filter.alertFilteredCount', { filtered: rows.length, total })
                : t('filter.filteredCount', { filtered: rows.length, total })}
            </span>
          }
        >
          <DataTable
            columns={columns}
            rows={rows}
            rowKey={(row) => String(row.task.id)}
            rowClassName={(row) => `task-row task-row-${row.task.id}`}
            onRowActivate={(row) => openDetail(row.task.id)}
            empty={t('empty.noTasks')}
            emptyTestid='task-table-empty'
          />

          <div
            className='mt-3 flex flex-wrap items-center justify-end gap-2 text-sm'
            data-testid='task-pager'
          >
            <span className='text-muted-foreground' data-testid='pager-total'>
              {t('pagination.total', { total })}
            </span>
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
                  data-testid={`pager-page-${n}`}
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
        </Panel>
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
