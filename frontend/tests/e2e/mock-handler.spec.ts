// input: shared frontend mock handler module under src/mocks
// output: regression coverage for dashboard pagination metadata, strict limit validation, and lookup/list SameSourceHost accept/reject filters
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

test('shared mock handler uses the same source identity for lookup and list filters', async () => {
  const moduleUrl = new URL('../../src/mocks/mock-handler.js', import.meta.url).href
  const { createMockSession } = await import(moduleUrl)
  const session = createMockSession({ scenario: 'empty' })

  session.request({
    method: 'POST',
    path: '/api/tasks',
    body: { name: 'loopback-ip', cluster_key: 'loopback-ip', source: { host: '127.0.0.1', port: 3306 } },
  })
  session.request({
    method: 'POST',
    path: '/api/tasks',
    body: { name: 'loopback-name', cluster_key: 'loopback-name', source: { host: 'localhost', port: 3306 } },
  })
  session.request({
    method: 'POST',
    path: '/api/tasks',
    body: { name: 'other-port', cluster_key: 'other-port', source: { host: '127.0.0.1', port: 3307 } },
  })
  session.request({
    method: 'POST',
    path: '/api/tasks',
    body: { name: 'primary', cluster_key: 'primary', source: { host: 'db-primary.example', port: 3306 } },
  })

  const lookupLocal = session.request({
    method: 'GET',
    path: '/api/sources/lookup',
    query: new URLSearchParams('host=localhost&port=3306'),
  })
  const lookupIP = session.request({
    method: 'GET',
    path: '/api/sources/lookup',
    query: new URLSearchParams('host=127.0.0.1&port=3306'),
  })
  const dashLocal = session.request({
    method: 'GET',
    path: '/api/dashboard',
    query: new URLSearchParams('host=localhost&port=3306'),
  })
  const dashIP = session.request({
    method: 'GET',
    path: '/api/dashboard',
    query: new URLSearchParams('host=127.0.0.1&port=3306'),
  })
  const listPrimary = session.request({
    method: 'GET',
    path: '/api/tasks',
    query: new URLSearchParams('host=db-primary.example&port=3306'),
  })
  const listPrimaryCase = session.request({
    method: 'GET',
    path: '/api/tasks',
    query: new URLSearchParams('host=DB-PRIMARY.EXAMPLE&port=3306'),
  })

  expect(lookupLocal.status).toBe(200)
  expect(lookupIP.status).toBe(200)
  expect(lookupLocal.body.count).toBe(2)
  expect(lookupIP.body.count).toBe(2)
  expect(lookupLocal.body.exists).toBe(true)
  expect(dashLocal.body.total).toBe(2)
  expect(dashIP.body.total).toBe(2)
  expect(dashLocal.body.tasks.map((row) => row.task.source.host).sort()).toEqual(['127.0.0.1', 'localhost'])
  expect(dashIP.body.tasks.map((row) => row.task.source.host).sort()).toEqual(['127.0.0.1', 'localhost'])
  expect(listPrimary.body.total).toBe(1)
  expect(listPrimary.body.items[0].source.host).toBe('db-primary.example')
  expect(listPrimaryCase.body.total).toBe(0)
})

test('shared mock handler loopback identity matches SameSourceHost accept/reject set', async () => {
  const moduleUrl = new URL('../../src/mocks/mock-handler.js', import.meta.url).href
  const { createMockSession } = await import(moduleUrl)
  const session = createMockSession({ scenario: 'empty' })

  const stored = [
    { name: 'ipv4', host: '127.0.0.1' },
    { name: 'name', host: 'localhost' },
    { name: 'expanded', host: '0:0:0:0:0:0:0:1' },
    { name: 'padded-groups', host: '0000:0000:0000:0000:0000:0000:0000:0001' },
    { name: 'primary', host: 'db-primary.example' },
  ]
  for (const item of stored) {
    const created = session.request({
      method: 'POST',
      path: '/api/tasks',
      body: { name: item.name, cluster_key: item.name, source: { host: item.host, port: 3306 } },
    })
    expect(created.status).toBe(201)
  }

  const countFor = (host) => {
    const query = new URLSearchParams({ host, port: '3306' })
    const lookup = session.request({
      method: 'GET',
      path: '/api/sources/lookup',
      query,
    })
    const list = session.request({
      method: 'GET',
      path: '/api/tasks',
      query,
    })
    expect(lookup.status).toBe(200)
    expect(list.status).toBe(200)
    expect(list.body.total).toBe(lookup.body.count)
    return lookup.body.count
  }

  expect(countFor('localhost')).toBe(4)
  expect(countFor('127.0.0.1')).toBe(4)
  expect(countFor('[::1]')).toBe(4)
  expect(countFor('0:0:0:0:0:0:0:1')).toBe(4)
  expect(countFor('0000:0000:0000:0000:0000:0000:0000:0001')).toBe(4)
  expect(countFor('::ffff:127.0.0.1')).toBe(4)
  expect(countFor('::ffff:7f00:1')).toBe(4)
  expect(countFor('127.000.0.1')).toBe(0)
  expect(countFor('[127.0.0.1]')).toBe(0)
  expect(countFor('db-primary.example')).toBe(1)
})
