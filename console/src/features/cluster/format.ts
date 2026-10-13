// Pure text helpers for the cluster and source-coverage panels (App.vue).
import type { ClusterOverview, SourceCoverage } from '@/lib/api'
import type { TFunction } from '@/lib/i18n'

/**
 * Registered workers: the GET /api/workers list. The overview's worker_count
 * only counts workers that own tasks, so a heartbeating idle worker is missing
 * from it; it is used only while the list is empty (not loaded or failed).
 */
export function workerCount(overview: ClusterOverview, workers?: readonly unknown[] | null) {
  const listed = Array.isArray(workers) ? workers.length : 0
  return listed > 0 ? listed : overview.worker_count || 0
}

export function clusterSummary(
  t: TFunction,
  overview: ClusterOverview,
  workers?: readonly unknown[] | null,
) {
  return overview.single_process
    ? t('cluster.singleProcess')
    : t('cluster.workerCount', { count: workerCount(overview, workers) })
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
