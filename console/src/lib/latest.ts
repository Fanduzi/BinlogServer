// Only the latest of overlapping requests may write state. Two refreshes in
// flight (a Query then a Clear, a drawer switching tasks) can resolve out of
// order; the older answer is dropped instead of overwriting the newer one.
export function createLatest() {
  let seq = 0
  return {
    begin: () => ++seq,
    isLatest: (token: number) => token === seq,
    /** Invalidate everything in flight (unmount, task switch). */
    cancel: () => {
      seq++
    },
  }
}
