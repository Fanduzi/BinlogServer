import { describe, expect, it } from 'vitest'
import { bodyText, errorText, errorTextAsync, inlineErrorText, isUnauthorized } from './errors'

const httpErr = (
  status: number,
  data: unknown,
  message = `Request failed with status code ${status}`,
) => ({
  message,
  response: { status, data },
})

describe('errorText', () => {
  it('prefers a plain-text body, trimmed', () => {
    expect(errorText(httpErr(404, 'task not found\n'))).toBe('task not found')
  })
  it('reads {error} and {message} of a structured body', () => {
    expect(
      errorText(httpErr(400, { error: 'source.password is required', code: 'INVALID_REQUEST' })),
    ).toBe('source.password is required')
    expect(errorText(httpErr(429, { message: 'slow down', code: 429 }))).toBe('slow down')
  })
  it('falls back to JSON for other bodies, then the client message, then the fallback', () => {
    expect(errorText(httpErr(500, { code: 'X' }))).toBe('{"code":"X"}')
    expect(errorText(httpErr(500, ''))).toBe('Request failed with status code 500')
    expect(errorText({}, 'Unknown error')).toBe('Unknown error')
    expect(errorText(new Error('Network Error'))).toBe('Network Error')
  })
  it('ignores an empty object body', () => {
    expect(errorText(httpErr(502, {}), 'x')).toBe('Request failed with status code 502')
  })
})

describe('bodyText', () => {
  it('handles nested error objects and non-strings', () => {
    expect(bodyText({ error: { error: 'cluster_key already exists' } })).toBe(
      'cluster_key already exists',
    )
    expect(bodyText('  ')).toBe('')
    expect(bodyText(null)).toBe('')
    expect(bodyText(42)).toBe('42')
  })
})

describe('errorTextAsync', () => {
  it('reads a Blob body as text or JSON', async () => {
    const plain = httpErr(404, new Blob(['file not found']))
    expect(await errorTextAsync(plain)).toBe('file not found')
    const json = httpErr(400, new Blob([JSON.stringify({ error: 'bad name' })]))
    expect(await errorTextAsync(json)).toBe('bad name')
    const empty = httpErr(500, new Blob([]))
    expect(await errorTextAsync(empty)).toBe('Request failed with status code 500')
  })
})

it('isUnauthorized', () => {
  expect(isUnauthorized(httpErr(401, ''))).toBe(true)
  expect(isUnauthorized(httpErr(403, ''))).toBe(false)
  expect(isUnauthorized(new Error('x'))).toBe(false)
})

it('inlineErrorText hides a 401 and keeps other errors', () => {
  expect(inlineErrorText({ response: { status: 401, data: 'unauthorized' } }, 'x')).toBe('')
  expect(inlineErrorText({ response: { status: 500, data: 'boom' } }, 'x')).toBe('boom')
})
