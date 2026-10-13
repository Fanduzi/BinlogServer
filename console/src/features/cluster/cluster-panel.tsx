// "Cluster View" panel shared by Overview and Workers (App.vue): summary,
// three counters, then the page's own body (overview note or worker list).
import type { ReactNode } from 'react'
import { Network } from 'lucide-react'
import { Panel, StatCell } from '@/components/panel'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'
import { clusterSummary } from './format'

export function ClusterPanel({ children }: { children?: ReactNode }) {
  const { t } = useI18n()
  const { overview } = useObservation()
  return (
    <Panel
      icon={Network}
      title={t('cluster.title')}
      hint={<span data-testid='cluster-worker-summary'>{clusterSummary(t, overview)}</span>}
      testid='cluster-panel'
    >
      <div className='grid grid-cols-1 gap-2 sm:grid-cols-3'>
        <StatCell
          label={t('cluster.taskCount')}
          value={overview.task_count}
          testid='cluster-task-count'
        />
        <StatCell
          label={t('cluster.runningTaskCount')}
          value={overview.running_task_count}
          testid='cluster-running-count'
        />
        <StatCell
          label={t('cluster.leasedTaskCount')}
          value={overview.leased_task_count}
          testid='cluster-leased-count'
        />
      </div>
      {children && <div className='mt-3'>{children}</div>}
    </Panel>
  )
}
