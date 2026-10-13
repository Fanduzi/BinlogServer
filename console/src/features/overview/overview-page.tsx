// Overview view (App.vue activeView === 'overview'): the shell's KPI strip,
// the cluster panel with its note, and source coverage.
import { ClusterPanel } from '../cluster/cluster-panel'
import { overviewNote } from '../cluster/format'
import { SourceCoveragePanel } from '../cluster/source-coverage'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'

export function OverviewPage() {
  const { t } = useI18n()
  const { overview } = useObservation()
  return (
    <div className='flex min-w-0 flex-col gap-4'>
      <ClusterPanel>
        <p className='text-muted-foreground text-sm' data-testid='overview-note'>
          {overviewNote(t, overview)}
        </p>
      </ClusterPanel>
      <SourceCoveragePanel />
    </div>
  )
}
