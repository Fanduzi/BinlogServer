import { describe, expect, it } from 'vitest'
import { t } from '@/test/t'
import {
  batchItemError,
  buildBatchPayloads,
  buildPreview,
  defaultBatchForm,
  parseBatchLines,
} from './batch'

describe('parseBatchLines', () => {
  it('accepts the three formats and skips comments and blanks', () => {
    const { rows, errors } = parseBatchLines(
      t,
      'a,10.0.0.1,3306\n\n# note\n10.0.0.2,3307\n10.0.0.3:3308\n10.0.0.4:3309,named',
    )
    expect(errors).toEqual([])
    expect(rows.map((r) => [r.lineNo, r.name, r.host, r.port])).toEqual([
      [1, 'a', '10.0.0.1', 3306],
      [4, 'task-10.0.0.2-3307', '10.0.0.2', 3307],
      [5, 'task-10.0.0.3-3308', '10.0.0.3', 3308],
      [6, 'named', '10.0.0.4', 3309],
    ])
    expect(rows[0].clusterKey).toBe('a-10.0.0.1-3306-1')
  })
  it('reports each bad line with its number', () => {
    const { rows, errors } = parseBatchLines(t, 'a,h,3306\nh:3306\nbad\nb,h,99999\nw,x,y,z\nc,h2,1')
    expect(rows.filter((r) => r.valid)).toHaveLength(2)
    expect(errors).toEqual([
      t('batch.errors.sourceDuplicate', { lineNo: 2, source: 'h:3306' }),
      t('batch.errors.singleColumnMustBeHostPort', { lineNo: 3 }),
      t('batch.errors.portInvalid', { lineNo: 4 }),
      t('batch.errors.onlySupportFormats', { lineNo: 5 }),
    ])
  })
  it('duplicate names are case-insensitive', () => {
    const { errors } = parseBatchLines(t, 'Orders,h1,1\norders,h2,2')
    expect(errors).toEqual([t('batch.errors.nameDuplicate', { lineNo: 2, name: 'orders' })])
  })
  it('no rows at all', () => {
    expect(parseBatchLines(t, '\n# only comments\n').errors).toEqual([t('batch.noData')])
  })
})

describe('buildPreview', () => {
  it('caps at 100 valid rows', () => {
    const lines = Array.from({ length: 101 }, (_, i) => `10.1.0.${i % 250}:${1000 + i}`).join('\n')
    const { preview, tooManyError } = buildPreview(t, lines)
    expect(preview.validCount).toBe(101)
    expect(tooManyError).toBe(t('batch.tooManyItems', { max: 100 }))
    expect(preview.canSubmit).toBe(false)
  })
  it('submittable when all lines pass', () => {
    expect(buildPreview(t, 'h:1').preview).toMatchObject({
      ready: true,
      canSubmit: true,
      validCount: 1,
    })
  })
})

describe('buildBatchPayloads', () => {
  it('builds the same body as useBatchCreate', () => {
    const form = { ...defaultBatchForm(), user: 'repl', password: 'pw', serverIdStart: 500 }
    const { rows } = parseBatchLines(t, 'a,10.0.0.1,3306\n\n10.0.0.2:3307')
    const { payloads, errors } = buildBatchPayloads(t, form, rows)
    expect(errors).toEqual([])
    expect(payloads[1]).toEqual({
      name: 'task-10.0.0.2-3307',
      cluster_key: 'task-10.0.0.2-3307-10.0.0.2-3307-3',
      source: {
        host: '10.0.0.2',
        port: 3307,
        user: 'repl',
        flavor: 'mysql',
        server_id: 502,
        semi_sync: false,
        password: 'pw',
      },
      start: { mode: 'LATEST' },
      storage: { retention_days: 7 },
    })
  })
  it('omits an empty password and reports rows that fail validation', () => {
    const { rows } = parseBatchLines(t, 'h:1')
    const { payloads, errors } = buildBatchPayloads(t, defaultBatchForm(), rows)
    expect(payloads).toEqual([])
    expect(errors[0]).toContain(t('validation.userInvalid'))
  })
})

it('batchItemError', () => {
  expect(batchItemError('boom')).toBe('boom')
  expect(batchItemError({ error: 'cluster_key already exists', code: 'X' })).toBe(
    'cluster_key already exists',
  )
  expect(batchItemError(null)).toBe('unknown error')
})
