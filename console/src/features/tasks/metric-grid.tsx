// KPI cards, as frontend/src/components/MetricGrid.vue: each card applies its
// quick filter.
import { Bug, CircleCheck, Hourglass, Layers, Loader, Play, TriangleAlert } from 'lucide-react'
import { useObservation } from '@/context/observation'
import type { DashboardSummary } from '@/lib/api'
import { useApplyQuickFilter, useTaskFilter } from './filter-context'
import { useI18n } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import type { QuickFilter } from './filter'

const cards = [
  {
    filter: 'abnormal',
    variant: 'danger',
    icon: TriangleAlert,
    field: 'abnormal',
    valueTestid: 'kpi-abnormal-value',
  },
  {
    filter: 'failed',
    variant: 'danger',
    icon: Bug,
    field: 'failed',
    valueTestid: 'kpi-failed-value',
  },
  {
    filter: 'delayed',
    variant: 'warning',
    icon: Hourglass,
    field: 'delayed',
    valueTestid: 'kpi-delayed-value',
  },
  {
    filter: 'starting',
    variant: 'warning',
    icon: Loader,
    field: 'starting',
    valueTestid: 'kpi-starting-value',
  },
  { filter: 'running', variant: '', icon: Play, field: 'running', valueTestid: undefined },
  { filter: 'all', variant: '', icon: Layers, field: 'total', valueTestid: undefined },
  {
    filter: 'normal',
    variant: 'success',
    icon: CircleCheck,
    field: 'normal',
    valueTestid: undefined,
  },
] as const

const labelKey: Record<string, string> = { all: 'metrics.total' }

export function MetricGrid({
  summary,
  active,
  onFilter,
}: {
  summary: DashboardSummary
  active: QuickFilter
  onFilter: (f: QuickFilter) => void
}) {
  const { t } = useI18n()
  return (
    <section className='grid grid-cols-2 gap-3 sm:grid-cols-4 xl:grid-cols-7'>
      {cards.map((card) => (
        <button
          key={card.filter}
          type='button'
          data-testid={`kpi-${card.filter}`}
          data-active={active === card.filter}
          onClick={() => onFilter(card.filter)}
          className={cn(
            'bg-card hover:bg-accent rounded-xl border p-3 text-left shadow-xs transition-colors',
            'data-[active=true]:ring-ring data-[active=true]:ring-2',
            card.variant === 'danger' && 'border-l-4 border-l-red-500',
            card.variant === 'warning' && 'border-l-4 border-l-amber-500',
            card.variant === 'success' && 'border-l-4 border-l-emerald-500',
          )}
        >
          <p className='text-muted-foreground flex items-center gap-1.5 text-xs'>
            <card.icon className='size-3.5' />
            {t(labelKey[card.filter] || `metrics.${card.filter}`)}
          </p>
          <strong className='mt-1 block text-2xl tabular-nums' data-testid={card.valueTestid}>
            {summary[card.field]}
          </strong>
        </button>
      ))}
    </section>
  )
}

/** The KPI strip the shell shows above every page, as App.vue does. */
export function ShellMetricGrid() {
  const { dashboard } = useObservation()
  const { activeQuick } = useTaskFilter()
  const applyQuick = useApplyQuickFilter()
  return <MetricGrid summary={dashboard.summary} active={activeQuick} onFilter={applyQuick} />
}
