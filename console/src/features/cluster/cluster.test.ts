import { expect, it } from 'vitest'
import { t } from '@/test/t'
import { clusterSummary, overviewNote, sourceKey, sourceStats, workerCount } from './format'

const overview = {
  task_count: 3,
  worker_count: 2,
  running_task_count: 1,
  leased_task_count: 1,
  single_process: false,
}

it('cluster summary and note follow single_process', () => {
  expect(clusterSummary(t, overview)).toBe(t('cluster.workerCount', { count: 2 }))
  expect(overviewNote(t, overview)).toBe(t('cluster.overviewNote'))
  const single = { ...overview, single_process: true }
  expect(clusterSummary(t, single)).toBe(t('cluster.singleProcess'))
  expect(overviewNote(t, single)).toBe(t('cluster.singleProcess'))
})

it('source key and stats default starting to 0', () => {
  const s = { host: 'h', port: 3306, task_count: 4, running: 2, normal: 1, delayed: 1, abnormal: 0 }
  expect(sourceKey(s)).toBe('h:3306')
  expect(sourceStats(t, s)).toBe(
    t('source.stats', { tasks: 4, starting: 0, running: 2, normal: 1, delayed: 1, abnormal: 0 }),
  )
})

it('worker count follows the registered list, not the owners-only overview count', () => {
  const ov = { ...overview, worker_count: 1 }
  expect(workerCount(ov, [{}, {}])).toBe(2)
  expect(workerCount(ov, [])).toBe(1)
  expect(clusterSummary(t, ov, [{}, {}])).toBe(t('cluster.workerCount', { count: 2 }))
})
