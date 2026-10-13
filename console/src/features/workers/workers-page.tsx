// Workers view (App.vue activeView === 'workers'): the cluster panel with
// the worker list under it.
import { DataTable, type Column } from '@/components/data-table'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'
import { ClusterPanel } from '../cluster/cluster-panel'
import { workerRows, type WorkerRow } from '../cluster/workers'
import { formatTs } from '../tasks/format'
import { ToneBadge } from '../tasks/tone-badge'

export function WorkersPage() {
  const { t, locale } = useI18n()
  const { workers, overview, nowRefMs } = useObservation()
  const rows = workerRows(workers, nowRefMs())
  const columns: Column<WorkerRow>[] = [
    {
      key: 'id',
      header: t('table.worker'),
      cell: (w) => <span className='font-mono font-medium break-all'>{w.worker_id}</span>,
    },
    {
      key: 'status',
      header: t('workersView.status'),
      cell: (w) => (
        <ToneBadge tone={w.online ? 'success' : 'info'} data-online={String(w.online)}>
          {w.online ? t('cluster.workerOnline') : t('cluster.workerOffline')}
        </ToneBadge>
      ),
    },
    {
      key: 'load',
      header: t('workersView.load'),
      cell: (w) =>
        t('cluster.workerTasks', { tasks: w.task_count, running: w.running, leased: w.leased }),
    },
    {
      key: 'heartbeat',
      header: t('workersView.heartbeat'),
      cell: (w) => (
        <span className='text-muted-foreground'>
          {t('cluster.lastHeartbeat', { time: formatTs(locale, w.last_seen_at) })}
        </span>
      ),
    },
  ]
  return (
    <ClusterPanel>
      <DataTable
        testid='worker-table'
        columns={columns}
        rows={rows}
        rowKey={(w) => w.worker_id}
        rowProps={(w) => ({ 'data-testid': `worker-row-${w.worker_id}` })}
        empty={overview.single_process ? t('cluster.singleProcess') : t('cluster.noWorkers')}
        emptyTestid='workers-empty'
      />
    </ClusterPanel>
  )
}
