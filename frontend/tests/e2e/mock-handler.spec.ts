// input: shared frontend mock handler module under src/mocks
// output: regression coverage for dashboard one-read page/summary/source counts, strict limit validation, and lookup/dashboard SameSourceHost accept/reject filters
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

test('shared mock handler returns one dashboard materialization for page, summary, and source counts', async () => {
  const moduleUrl = new URL('../../src/mocks/mock-handler.js', import.meta.url).href
  const { handleMockRequest } = await import(moduleUrl)

  const response = handleMockRequest({
    scenario: 'pagination',
    method: 'GET',
    path: '/api/dashboard',
    query: new URLSearchParams('limit=1&offset=20'),
  })

  expect(response.status).toBe(200)
  expect(response.body).toMatchObject({ total: 25, limit: 1, offset: 20, summary: { total: 25 } })
  expect(response.body.tasks).toHaveLength(1)
  const sourceTotal = (response.body.sources || []).reduce((sum, source) => sum + Number(source.task_count || 0), 0)
  expect(sourceTotal).toBe(25)

  const invalid = handleMockRequest({
    scenario: 'pagination',
    method: 'GET',
    path: '/api/dashboard',
    query: new URLSearchParams('limit=501'),
  })
  expect(invalid.status).toBe(400)
  expect(invalid.body).toEqual({ error: 'invalid limit' })
})

test('shared mock handler uses the same source identity for lookup and dashboard filters', async () => {
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
  const dashPrimary = session.request({
    method: 'GET',
    path: '/api/dashboard',
    query: new URLSearchParams('host=db-primary.example&port=3306'),
  })
  const dashPrimaryCase = session.request({
    method: 'GET',
    path: '/api/dashboard',
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
  expect(dashPrimary.body.total).toBe(1)
  expect(dashPrimary.body.summary.total).toBe(1)
  expect(dashPrimary.body.tasks[0].task.source.host).toBe('db-primary.example')
  expect(dashPrimaryCase.body.total).toBe(0)
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
    const dashboard = session.request({
      method: 'GET',
      path: '/api/dashboard',
      query,
    })
    expect(lookup.status).toBe(200)
    expect(dashboard.status).toBe(200)
    expect(dashboard.body.total).toBe(lookup.body.count)
    expect(dashboard.body.summary.total).toBe(lookup.body.count)
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
