import { describe, expect, it } from 'vitest'
import { pollUntil } from './poll'

const noSleep = async () => {}

function script<T>(steps: (T | Error)[]) {
  let i = 0
  const calls = () => i
  const fetch = async () => {
    const step = steps[Math.min(i++, steps.length - 1)]
    if (step instanceof Error) throw step
    return step
  }
  return { fetch, calls }
}

const base = { intervalMs: 0, maxPolls: 10, maxFailures: 3, sleep: noSleep }

describe('pollUntil', () => {
  it('settles when done() holds', async () => {
    const s = script(['STARTING', 'STARTING', 'RUNNING'])
    expect(await pollUntil({ ...base, fetch: s.fetch, done: (v) => v === 'RUNNING' })).toBe(
      'settled',
    )
    expect(s.calls()).toBe(3)
  })
  it('retries through errors and still settles', async () => {
    const s = script<string>([new Error('net'), new Error('net'), 'RUNNING'])
    expect(await pollUntil({ ...base, fetch: s.fetch, done: (v) => v === 'RUNNING' })).toBe(
      'settled',
    )
  })
  it('gives up after maxFailures errors in a row', async () => {
    const s = script<string>([new Error('a')])
    expect(await pollUntil({ ...base, fetch: s.fetch, done: () => true })).toBe('failed')
    expect(s.calls()).toBe(3)
  })
  it('a success resets the failure count', async () => {
    const e = new Error('x')
    const s = script<string>([e, e, 'STARTING', e, e, 'RUNNING'])
    expect(await pollUntil({ ...base, fetch: s.fetch, done: (v) => v === 'RUNNING' })).toBe(
      'settled',
    )
  })
  it('stops at maxPolls', async () => {
    const s = script(['STARTING'])
    expect(await pollUntil({ ...base, maxPolls: 4, fetch: s.fetch, done: () => false })).toBe(
      'exhausted',
    )
    expect(s.calls()).toBe(4)
  })
  it('honours cancellation before fetching', async () => {
    const s = script(['STARTING'])
    expect(
      await pollUntil({ ...base, fetch: s.fetch, done: () => false, cancelled: () => true }),
    ).toBe('cancelled')
    expect(s.calls()).toBe(0)
  })
})
