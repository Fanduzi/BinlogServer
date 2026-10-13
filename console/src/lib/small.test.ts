import { describe, expect, it } from 'vitest'
import { tKnown } from './i18n'
import { createLatest } from './latest'
import { clampNumber, parseNumberInput } from './number'
import { t } from '@/test/t'

describe('createLatest', () => {
  it('only the newest token is current; cancel drops all', () => {
    const g = createLatest()
    const a = g.begin()
    const b = g.begin()
    expect(g.isLatest(a)).toBe(false)
    expect(g.isLatest(b)).toBe(true)
    g.cancel()
    expect(g.isLatest(b)).toBe(false)
  })
})

describe('number helpers', () => {
  it('parseNumberInput', () => {
    expect(parseNumberInput('')).toBeNull()
    expect(parseNumberInput('  ')).toBeNull()
    expect(parseNumberInput('3306')).toBe(3306)
    expect(parseNumberInput('abc')).toBeNull()
  })
  it('clampNumber follows el-input-number on blur', () => {
    expect(clampNumber(70000, 1, 65535)).toBe(65535)
    expect(clampNumber(0, 1, 65535)).toBe(1)
    expect(clampNumber(-5, 1)).toBe(1)
    expect(clampNumber(null, 1, 10)).toBe(1)
    expect(clampNumber(Number.NaN, 0)).toBe(0)
    expect(clampNumber(42, 0)).toBe(42)
  })
})

describe('i18n', () => {
  it('interpolates named params and vue-i18n literals', () => {
    expect(t('msg.taskStarted', { id: 7 })).toContain('7')
    expect(t('form.flavorHint')).toContain('@@server_uuid')
    expect(t('form.flavorHint')).not.toContain("{'")
  })
  it('a missing key returns the key; tKnown returns the fallback instead', () => {
    expect(t('no.such.key')).toBe('no.such.key')
    expect(tKnown(t, 'state.NOPE', 'NOPE')).toBe('NOPE')
    expect(tKnown(t, 'state.RUNNING', 'x')).toBe(t('state.RUNNING'))
  })
})
