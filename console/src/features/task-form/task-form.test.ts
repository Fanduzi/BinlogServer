import { describe, expect, it } from 'vitest'
import { t } from '@/test/t'
import {
  ADOPT_START_DEFAULT,
  adoptForm,
  buildAdoptPayload,
  buildPayload,
  defaultForm,
  editForm,
  validateAdoptPayload,
  validateTaskPayload,
} from './task-form'

const valid = () => {
  const f = defaultForm()
  f.name = 'orders'
  f.cluster_key = 'orders'
  return f
}

describe('create/edit payload and validation', () => {
  it('a default form with name and key is valid; empty password is omitted', () => {
    const p = buildPayload(valid())
    expect(validateTaskPayload(t, p)).toBe('')
    expect(p.source).not.toHaveProperty('password')
    expect(p.storage).toEqual({ retention_days: 7 })
  })
  it('validation order and messages', () => {
    const cases: [(f: ReturnType<typeof valid>) => void, string][] = [
      [(f) => (f.name = ' '), 'validation.taskNameInvalid'],
      [(f) => (f.cluster_key = ''), 'validation.clusterKeyEmpty'],
      [(f) => (f.cluster_key = 'a/b'), 'validation.clusterKeyInvalid'],
      [(f) => (f.cluster_key = 'a..b'), 'validation.clusterKeyInvalid'],
      [(f) => (f.source.host = 'a b'), 'validation.hostInvalid'],
      [(f) => (f.source.port = 70000), 'validation.portInvalid'],
      [(f) => (f.source.user = ''), 'validation.userInvalid'],
      [(f) => (f.source.flavor = 'my sql'), 'validation.flavorInvalid'],
      [(f) => (f.start.mode = 'FILE_POS'), 'validation.filePosRequired'],
      [(f) => (f.start.mode = 'GTID'), 'validation.gtidSetRequired'],
      [(f) => (f.storage.retention_days = 0), 'validation.retentionInvalid'],
      [(f) => (f.storage.local_retention_days = 9999), 'validation.localRetentionInvalid'],
      [
        (f) => ((f.storage.local_retention_days = 10), (f.storage.bucket_retention_days = 5)),
        'validation.bucketRetentionShorter',
      ],
    ]
    for (const [mutate, key] of cases) {
      const f = valid()
      mutate(f)
      expect(validateTaskPayload(t, buildPayload(f)), key).toBe(t(key))
    }
  })
  it('FILE_POS and GTID carry their fields', () => {
    const f = valid()
    f.start = { mode: 'FILE_POS', file: ' mysql-bin.000003 ', pos: 4, gtid_set: '' }
    expect(buildPayload(f).start).toEqual({ mode: 'FILE_POS', file: 'mysql-bin.000003', pos: 4 })
  })
  it('editForm keeps the task and clears the password', () => {
    const f = editForm({
      id: 5,
      name: 'x',
      cluster_key: 'x',
      source: { host: 'h', port: 1, user: 'u', password: 'secret' },
    })
    expect(f.id).toBe('5')
    expect(f.source.password).toBe('')
    expect(f.start).toEqual(defaultForm().start)
  })
})

describe('adopt', () => {
  it('default start is omitted; password is required', () => {
    const f = adoptForm({ id: 900, storage: { retention_days: 30 } })
    expect(f.start.mode).toBe(ADOPT_START_DEFAULT)
    f.cluster_key = 'adopted'
    f.source.host = 'h'
    f.source.user = 'repl'
    const p = buildAdoptPayload(f)
    expect(p).not.toHaveProperty('start')
    expect(p.name).toBe('900')
    expect(p.storage).toEqual({ retention_days: 30 })
    expect(validateAdoptPayload(t, p)).toBe(t('validation.passwordRequired'))
    f.source.password = 'pw'
    expect(validateAdoptPayload(t, buildAdoptPayload(f))).toBe('')
  })
  it('an out-of-range stored retention falls back to 7', () => {
    expect(adoptForm({ id: 1, storage: { retention_days: 99999 } }).storage.retention_days).toBe(7)
  })
})
