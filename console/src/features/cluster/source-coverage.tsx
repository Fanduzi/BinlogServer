// "Source Coverage" panel shared by Overview and Sources (App.vue).
import { Database } from 'lucide-react'
import { EmptyState } from '@/components/empty-state'
import { Panel } from '@/components/panel'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'
import { sourceKey, sourceStats } from './format'

export function SourceCoveragePanel() {
  const { t } = useI18n()
  const { dashboard } = useObservation()
  const sources = dashboard.sources
  return (
    <Panel
      icon={Database}
      title={t('panel.sourceCoverage')}
      hint={t('panel.hosts', { count: sources.length })}
      testid='source-coverage'
    >
      {sources.length > 0 ? (
        <div className='grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-3'>
          {sources.map((s) => (
            <div
              key={sourceKey(s)}
              className='bg-muted/40 min-w-0 rounded-lg border px-3 py-2'
              data-testid='source-cell'
            >
              <p className='font-mono text-sm font-medium break-all'>{sourceKey(s)}</p>
              <p className='text-muted-foreground text-xs'>{sourceStats(t, s)}</p>
            </div>
          ))}
        </div>
      ) : (
        <EmptyState testid='sources-empty'>{t('empty.noSources')}</EmptyState>
      )}
    </Panel>
  )
}
