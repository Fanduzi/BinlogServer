// One way to turn a failed request into operator text, for every page.
// Order: the server's own words (plain-text body, {error}, {message}),
// then the response body as JSON, then the client error message.
type Rec = Record<string, unknown>

/** Text carried by a response body, or '' when it has none. */
export function bodyText(data: unknown): string {
  if (data == null) return ''
  if (typeof data === 'string') return data.trim()
  if (typeof data !== 'object') return String(data)
  const d = data as Rec
  for (const key of ['error', 'message'] as const) {
    if (typeof d[key] === 'string' && (d[key] as string).trim()) return (d[key] as string).trim()
  }
  if (d.error && typeof d.error === 'object') {
    const nested = bodyText(d.error)
    if (nested) return nested
  }
  const json = JSON.stringify(data)
  return json === '{}' ? '' : json
}

const response = (err: unknown) => (err as Rec | null)?.response as Rec | undefined

export function errorStatus(err: unknown): number {
  return Number(response(err)?.status || 0)
}

export function isUnauthorized(err: unknown) {
  return errorStatus(err) === 401
}

export function errorText(err: unknown, fallback = ''): string {
  const fromBody = bodyText(response(err)?.data)
  if (fromBody) return fromBody
  const message = (err as Rec | null)?.message
  if (typeof message === 'string' && message.trim()) return message.trim()
  return fallback || String(err)
}

/** Same as errorText, but reads a Blob body (responseType: 'blob' downloads). */
export async function errorTextAsync(err: unknown, fallback = ''): Promise<string> {
  const data = response(err)?.data
  if (typeof Blob !== 'undefined' && data instanceof Blob) {
    try {
      const text = (await data.text()).trim()
      if (text) {
        try {
          return bodyText(JSON.parse(text)) || text
        } catch {
          return text
        }
      }
    } catch {
      // fall through to the generic text
    }
    const message = (err as Rec | null)?.message
    return (typeof message === 'string' && message) || fallback || String(err)
  }
  return errorText(err, fallback)
}
