/** Milliseconds for an API timestamp; 0 when missing or unparseable. */
export function toTimeMs(ts?: string) {
  if (!ts) return 0
  const ms = new Date(ts).getTime()
  return Number.isNaN(ms) ? 0 : ms
}
