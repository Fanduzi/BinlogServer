import type { Page, Route } from '@playwright/test'
import { scenarios, type MockScenario } from './mock-data'

export interface MockRouteOptions {
  scenario: MockScenario
  auth401Paths?: string[]
  onRetryUpload?: () => void
}

function json(route: Route, body: unknown, status = 200) {
  return route.fulfill({
    status,
    contentType: 'application/json',
    body: JSON.stringify(body),
  })
}

function matchTaskPath(path: string) {
  return /^\/api\/tasks\/[^/]+$/.test(path)
}

export async function registerMockRoutes(page: Page, options: MockRouteOptions) {
  const auth401Paths = options.auth401Paths || []
  let retryDone = false

  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const method = request.method()

    if (auth401Paths.some((target) => path === target)) {
      return json(route, { error: 'unauthorized' }, 401)
    }

    if (options.scenario === 'auth-required' && ['GET'].includes(method)) {
      if (path === '/api/dashboard' || path === '/api/cluster/overview' || path === '/api/workers') {
        return json(route, { error: 'unauthorized' }, 401)
      }
    }

    const scenario = options.scenario === 'auth-required' ? scenarios.healthy : scenarios[options.scenario]

    if (path === '/api/dashboard' && method === 'GET') {
      return json(route, {
        generated_at: '2026-03-25T08:00:00Z',
        threshold_seconds: 30,
        summary: scenario.summary,
        tasks: scenario.tasks,
        sources: scenario.sources,
      })
    }
    if (path === '/api/cluster/overview' && method === 'GET') {
      return json(route, scenario.clusterOverview)
    }
    if (path === '/api/workers' && method === 'GET') {
      return json(route, scenario.workers)
    }
    if (matchTaskPath(path) && method === 'GET') {
      return json(route, 'taskDetail' in scenario ? scenario.taskDetail : scenario.tasks[0]?.task || {})
    }
    if (/\/api\/tasks\/[^/]+\/checkpoint$/.test(path) && method === 'GET') {
      return json(route, 'checkpoint' in scenario ? scenario.checkpoint : null)
    }
    if (/\/api\/tasks\/[^/]+\/replication$/.test(path) && method === 'GET') {
      return json(route, 'replication' in scenario ? scenario.replication : scenario.tasks[0]?.replication || null)
    }
    if (/\/api\/tasks\/[^/]+\/lease$/.test(path) && method === 'GET') {
      return json(route, 'lease' in scenario ? scenario.lease : null)
    }
    if (/\/api\/tasks\/[^/]+\/runs$/.test(path) && method === 'GET') {
      return json(route, 'runs' in scenario ? scenario.runs : [])
    }
    if (/\/api\/tasks\/[^/]+\/events$/.test(path) && method === 'GET') {
      return json(route, 'events' in scenario ? scenario.events : [])
    }
    if (/\/api\/tasks\/[^/]+\/files$/.test(path) && method === 'GET') {
      if (options.scenario === 'upload-failed') {
        return json(route, retryDone ? scenarios['upload-failed'].filesAfterRetry : scenarios['upload-failed'].filesBeforeRetry)
      }
      return json(route, 'files' in scenario ? scenario.files : [])
    }
    if (/\/api\/tasks\/[^/]+\/files\/retry-upload$/.test(path) && method === 'POST') {
      retryDone = true
      options.onRetryUpload?.()
      return json(route, { retried: 1, failed: 0, skipped: 0 })
    }
    if (/\/api\/tasks\/[^/]+\/(start|stop)$/.test(path) && method === 'POST') {
      return json(route, { ok: true })
    }
    if (matchTaskPath(path) && method === 'DELETE') {
      return route.fulfill({ status: 204, body: '' })
    }

    return route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: `unmocked api request: ${method} ${path}` }),
    })
  })
}
