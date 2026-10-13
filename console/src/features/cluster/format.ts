// Pure text helpers for the cluster and source-coverage panels (App.vue).
import type { ClusterOverview, SourceCoverage } from '@/lib/api'
import type { TFunction } from '@/lib/i18n'

export function clusterSummary(t: TFunction, overview: ClusterOverview) {
  return overview.single_process
    ? t('cluster.singleProcess')
    : t('cluster.workerCount', { count: overview.worker_count })
}

export function overviewNote(t: TFunction, overview: ClusterOverview) {
  return overview.single_process ? t('cluster.singleProcess') : t('cluster.overviewNote')
}

export const sourceKey = (s: Pick<SourceCoverage, 'host' | 'port'>) => `${s.host}:${s.port}`

export function sourceStats(t: TFunction, s: SourceCoverage) {
  return t('source.stats', {
    tasks: s.task_count,
    starting: s.starting ?? 0,
    running: s.running,
    normal: s.normal,
    delayed: s.delayed,
    abnormal: s.abnormal,
  })
}
