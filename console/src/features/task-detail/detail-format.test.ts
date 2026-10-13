import { describe, expect, it } from 'vitest'
import { t } from '@/test/t'
import {
  effectiveRetention,
  formatCheckpoint,
  formatReplayCommand,
  formatStart,
  isLeftoverDiskTask,
  shellToken,
  stoppedReasonText,
} from './detail-format'

describe('detail-format', () => {
  it('formatCheckpoint accepts the field spellings the server has used', () => {
    expect(formatCheckpoint(t, null)).toBe(t('detail.noCheckpoint'))
    expect(formatCheckpoint(t, { file: 'mysql-bin.000004', pos: 455 })).toBe('mysql-bin.000004:455')
    expect(formatCheckpoint(t, { File: 'b.1', Pos: 4, gtid_set: 'u:1-5' })).toBe('b.1:4 GTID u:1-5')
  })
  it('formatStart', () => {
    expect(formatStart({ mode: 'LATEST' })).toContain('LATEST')
    expect(formatStart({ mode: 'FILE_POS', file: 'b.1', pos: 4 })).toContain('b.1:4')
  })
  it('shellToken quotes only when needed', () => {
    expect(shellToken('/data/1/mysql-bin.000001')).toBe('/data/1/mysql-bin.000001')
    expect(shellToken("/data/it's here")).not.toBe("/data/it's here")
  })
  it('formatReplayCommand is empty without paths', () => {
    expect(formatReplayCommand({ paths: [] })).toBe('')
    expect(formatReplayCommand(null)).toBe('')
  })
  it('effectiveRetention falls back to retention_days', () => {
    expect(effectiveRetention({ retention_days: 7 }, 'local')).toBe('7')
    expect(effectiveRetention({ retention_days: 7, bucket_retention_days: 30 }, 'bucket')).toBe(
      '30',
    )
  })
  it('isLeftoverDiskTask', () => {
    expect(isLeftoverDiskTask({ id: '900', source: { host: '', user: '' }, cluster_key: '' })).toBe(
      true,
    )
    expect(
      isLeftoverDiskTask({ id: '1', source: { host: 'h', user: 'u' }, cluster_key: 'k' }),
    ).toBe(false)
  })
  it('an unknown switch reason shows the raw reason, not a catalog key', () => {
    const task = {
      source_chain: {
        switches: [{ continued: false, resolved: false, reason: 'BRAND_NEW', old: 'a', new: 'b' }],
      },
    }
    const text = stoppedReasonText(t, task)
    expect(text).not.toContain('detail.switchReason')
  })
})
