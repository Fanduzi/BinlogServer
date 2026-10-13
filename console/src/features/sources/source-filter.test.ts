import { expect, it } from 'vitest'
import { buildSourceFilter, lookupParams, lookupResult } from './source-filter'

it('buildSourceFilter trims host and drops an empty port', () => {
  expect(buildSourceFilter({ host: '  10.0.0.1 ', port: 3306 })).toEqual({
    host: '10.0.0.1',
    port: 3306,
  })
  expect(buildSourceFilter({ host: ' ', port: null })).toEqual({})
  expect(buildSourceFilter({ host: '', port: 3306 })).toEqual({ port: 3306 })
})

it('lookupParams needs host and port', () => {
  expect(lookupParams({ host: 'h', port: null })).toBeNull()
  expect(lookupParams({ host: ' ', port: 1 })).toBeNull()
  expect(lookupParams({ host: ' h ', port: 1 })).toEqual({ host: 'h', port: 1 })
})

it('lookupResult tolerates a missing or partial body', () => {
  expect(lookupResult(undefined)).toEqual({ checked: true, exists: false, count: 0 })
  expect(lookupResult({ exists: true, count: 2 })).toEqual({
    checked: true,
    exists: true,
    count: 2,
  })
})
