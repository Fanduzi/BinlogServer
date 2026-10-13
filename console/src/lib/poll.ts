// Poll until a condition holds. Used wherever the Console waits for the server
// to settle (drawer Start/Stop). Errors are retried, not fatal: after
// maxFailures in a row it gives up with 'failed' so the caller can still do its
// final reload.
export type PollOutcome = 'settled' | 'exhausted' | 'failed' | 'cancelled'

export interface PollOptions<T> {
  fetch: () => Promise<T>
  done: (value: T) => boolean
  intervalMs: number
  maxPolls: number
  maxFailures: number
  cancelled?: () => boolean
  sleep?: (ms: number) => Promise<void>
}

const defaultSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms))

export async function pollUntil<T>(opts: PollOptions<T>): Promise<PollOutcome> {
  const sleep = opts.sleep ?? defaultSleep
  const cancelled = opts.cancelled ?? (() => false)
  let failures = 0
  for (let i = 0; i < opts.maxPolls; i++) {
    await sleep(opts.intervalMs)
    if (cancelled()) return 'cancelled'
    let value: T
    try {
      value = await opts.fetch()
      failures = 0
    } catch {
      if (++failures >= opts.maxFailures) return 'failed'
      continue
    }
    if (cancelled()) return 'cancelled'
    if (opts.done(value)) return 'settled'
  }
  return 'exhausted'
}
