// input: shared frontend mock handler module under src/mocks
// output: regression coverage for dashboard pagination metadata and strict limit validation
// pos: Playwright-backed regression test for the shared mock handler contract
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from '@playwright/test'

test('shared mock handler returns healthy dashboard payload', async () => {
  const moduleUrl = new URL('../../src/mocks/mock-handler.js', import.meta.url).href
  const { handleMockRequest } = await import(moduleUrl)

  const response = handleMockRequest({
    scenario: 'healthy',
    method: 'GET',
    path: '/api/dashboard',
  })

  expect(response.status).toBe(200)
  expect(response.body).toMatchObject({
    summary: {
      total: 1,
      running: 1,
      normal: 1,
    },
  })
  expect(Array.isArray(response.body.tasks)).toBe(true)
  expect(response.body.tasks).toHaveLength(1)
})

test('shared mock handler returns server-paged task list metadata', async () => {
  const moduleUrl = new URL('../../src/mocks/mock-handler.js', import.meta.url).href
  const { handleMockRequest } = await import(moduleUrl)

  const response = handleMockRequest({
    scenario: 'pagination',
    method: 'GET',
    path: '/api/tasks',
    query: new URLSearchParams('limit=1&offset=20'),
  })

  expect(response.status).toBe(200)
  expect(response.body).toMatchObject({ total: 25, limit: 1, offset: 20 })
  expect(response.body.items).toHaveLength(1)

  const invalid = handleMockRequest({
    scenario: 'pagination',
    method: 'GET',
    path: '/api/tasks',
    query: new URLSearchParams('limit=501'),
  })
  expect(invalid.status).toBe(400)
  expect(invalid.body).toEqual({ error: 'invalid limit' })
})
