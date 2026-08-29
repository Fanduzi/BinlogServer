// input: Playwright Page route interception, shared mock session options, and optional first-start failure injection
// output: browser request interception with JSON bodies wired to the shared frontend mock handler
// pos: Playwright adapter layer between browser requests and frontend shared mock responses
// note: if this file changes, update this header and frontend/README.md

import type { Page } from '@playwright/test'
import { createMockSession } from '../../../src/mocks/mock-handler.js'
import type { MockScenario } from './mock-data'

export interface MockRouteOptions {
  scenario: MockScenario
  auth401Paths?: string[]
  onRetryUpload?: () => void
  failFirstStart?: boolean
}

export async function registerMockRoutes(page: Page, options: MockRouteOptions) {
  const auth401Paths = options.auth401Paths || []
  let failedFirstStart = false
  const session = createMockSession({
    scenario: options.scenario,
    onRetryUpload: options.onRetryUpload,
  })

  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const method = request.method()

    if (auth401Paths.some((target) => path === target)) {
      return route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'unauthorized' }),
      })
    }

    if (options.failFirstStart && method === 'POST' && /^\/api\/tasks\/[^/]+\/start$/.test(path) && !failedFirstStart) {
      failedFirstStart = true
      return route.fulfill({
        status: 400,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'start failed', code: 'INVALID_REQUEST' }),
      })
    }

    const response = session.request({
      method,
      path,
      query: url.searchParams,
      body: request.postData() ? request.postDataJSON() : undefined,
    })
    return route.fulfill({
      status: response.status,
      contentType: 'application/json',
      body:
        response.status === 204 && response.body === ''
          ? ''
          : JSON.stringify(response.body),
    })
  })
}
