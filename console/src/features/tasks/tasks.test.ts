import { describe, expect, it } from 'vitest'
import type { DashboardRow } from '@/lib/api'
import { t } from '@/test/t'
import {
  defaultUiFilter,
  filterRows,
  isAlertRow,
  paginationParams,
  quickFilterState,
} from './filter'
import {
  formatDelay,
  formatReplicationReason,
  leaseRiskLabel,
  leaseRiskTone,
  replicationStatusLabel,
  sourceLabel,
  stateLabel,
  stateTone,
} from './format'

const row = (
  id: number,
  name: string,
  state: string,
  status: string,
  delay?: number,
  host = '10.0.0.1',
  port = 3306,
  updated = '2026-10-13T00:00:00Z',
): DashboardRow => ({
  task: { id, name, state, source: { host, port }, updated_at: updated },
  replication: { status, delay_seconds: delay, has_progress: delay != null },
})

const rows = [
  row(1, 'orders', 'RUNNING', 'NORMAL', 1),
  row(2, 'billing', 'RUNNING', 'DELAYED', 90, '10.0.0.2'),
  row(3, 'bad-port', 'RETRY_BACKOFF', 'ABNORMAL', undefined, '10.0.0.3', 3399),
  row(4, 'idle', 'CREATED', 'IDLE', undefined, '10.0.0.4', 3306, '2026-10-13T05:00:00Z'),
  row(5, 'broken', 'FAILED', 'IDLE'),
]

describe('filterRows', () => {
  it('sorts by delay desc by default', () => {
    expect(filterRows(rows, defaultUiFilter, '', '').map((r) => r.task.id)).toEqual([2, 1, 3, 4, 5])
  })
  it('keyword matches id or name, source keyword matches host:port', () => {
    expect(filterRows(rows, defaultUiFilter, 'BILL', '').map((r) => r.task.id)).toEqual([2])
    expect(filterRows(rows, defaultUiFilter, '4', '').map((r) => r.task.id)).toEqual([4])
    expect(filterRows(rows, defaultUiFilter, '', ':3399').map((r) => r.task.id)).toEqual([3])
  })
  it('state, replication status and alert-only', () => {
    expect(filterRows(rows, { ...defaultUiFilter, taskState: 'RUNNING' }, '', '')).toHaveLength(2)
    expect(
      filterRows(rows, { ...defaultUiFilter, replicationStatus: 'ABNORMAL' }, '', ''),
    ).toHaveLength(1)
    expect(
      filterRows(rows, { ...defaultUiFilter, onlyAlert: true }, '', '')
        .map((r) => r.task.id)
        .sort(),
    ).toEqual([2, 3, 5])
  })
  it('name and updated sorts', () => {
    expect(filterRows(rows, { ...defaultUiFilter, sortBy: 'name_asc' }, '', '')[0].task.name).toBe(
      'bad-port',
    )
    expect(
      filterRows(rows, { ...defaultUiFilter, sortBy: 'updated_desc' }, '', '')[0].task.id,
    ).toBe(4)
  })
  it('does not mutate its input', () => {
    const copy = [...rows]
    filterRows(rows, { ...defaultUiFilter, sortBy: 'name_asc' }, '', '')
    expect(rows).toEqual(copy)
  })
})

it('isAlertRow', () => {
  expect(rows.map(isAlertRow)).toEqual([false, true, true, false, true])
})

it('isAlertRow flags storage alerts and open findings on a healthy-looking row (S14)', () => {
  const healthy = row(6, 'quiet', 'RUNNING', 'NORMAL', 0)
  expect(isAlertRow(healthy)).toBe(false)
  expect(isAlertRow({ ...healthy, task: { ...healthy.task, storage_alert: true } })).toBe(true)
  expect(isAlertRow({ ...healthy, open_findings: 2 })).toBe(true)
  expect(isAlertRow({ ...healthy, open_findings: 0 })).toBe(false)
})

it('paginationParams sends state only when filtered', () => {
  expect(paginationParams(1, 20, 'ALL')).toEqual({ limit: 20, offset: 0 })
  expect(paginationParams(3, 50, 'FAILED')).toEqual({ limit: 50, offset: 100, state: 'FAILED' })
})

describe('quickFilterState', () => {
  const custom = {
    ...defaultUiFilter,
    keyword: 'x',
    sortBy: 'name_asc' as const,
    taskState: 'RUNNING',
  }
  it('keeps keyword and sort, resets the rest', () => {
    expect(quickFilterState('abnormal', custom)).toMatchObject({
      keyword: 'x',
      sortBy: 'name_asc',
      taskState: 'ALL',
      replicationStatus: 'ABNORMAL',
      onlyAlert: false,
    })
    expect(quickFilterState('alert', custom)).toMatchObject({ onlyAlert: true, taskState: 'ALL' })
    expect(quickFilterState('starting', custom).taskState).toBe('STARTING')
  })
  it("'all' resets everything", () => {
    expect(quickFilterState('all', custom)).toEqual(defaultUiFilter)
  })
})

describe('format', () => {
  it('labels known values and passes unknown ones through', () => {
    expect(stateLabel(t, 'RUNNING')).toBe(t('state.RUNNING'))
    expect(stateLabel(t, 'NEW_STATE')).toBe('NEW_STATE')
    expect(stateLabel(t, undefined)).toBe('--')
    expect(replicationStatusLabel(t, 'WHATEVER')).toBe('WHATEVER')
  })
  it('tones', () => {
    expect(['RUNNING', 'RETRY_BACKOFF', 'FAILED', 'CREATED'].map((s) => stateTone(s))).toEqual([
      'success',
      'warning',
      'danger',
      'info',
    ])
  })
  it('sourceLabel and formatDelay', () => {
    expect(sourceLabel(rows[0].task)).toBe('10.0.0.1:3306')
    expect(sourceLabel({ id: 9 })).toBe('-:-')
    expect(formatDelay(0, true)).toBe('0')
    expect(formatDelay(5, false)).toBe('--')
    expect(formatDelay(null, true)).toBe('--')
  })
  it('lease risk from the task copy', () => {
    const now = Date.parse('2026-10-13T00:01:00Z')
    const fresh = { id: 1, owner_worker_id: 'w1', epoch: 2, updated_at: '2026-10-13T00:00:30Z' }
    const stale = { ...fresh, updated_at: '2026-10-12T23:59:00Z' }
    expect(leaseRiskTone(fresh, now)).toBe('success')
    expect(leaseRiskTone(stale, now)).toBe('warning')
    expect(leaseRiskLabel(t, { id: 1 }, now)).toBe('--')
    expect(leaseRiskLabel(t, stale, now)).toBe(t('lease.risk'))
  })
  it('replication reason', () => {
    expect(formatReplicationReason(t, undefined)).toBe('--')
    expect(formatReplicationReason(t, { status: 'DELAYED' })).toBe(
      t('replication.delayExceedsThreshold'),
    )
    expect(formatReplicationReason(t, { reason: 'RUNNER_ERROR', last_error: 'dial tcp' })).toBe(
      `${t('replication.runnerError')}: dial tcp`,
    )
    expect(formatReplicationReason(t, { reason: 'CUSTOM' })).toBe('CUSTOM')
  })
})
