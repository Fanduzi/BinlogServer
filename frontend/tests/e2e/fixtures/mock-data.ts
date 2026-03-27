// input: shared frontend mock scenario data from src/mocks plus local test type aliases
// output: backward-compatible scenario exports for Playwright E2E fixtures
// pos: test-side re-export shim for frontend shared mock scenarios
// note: if this file changes, update this header and frontend/README.md

import { mockScenarios } from '../../../src/mocks/mock-data.js'

export type MockScenario =
  | 'empty'
  | 'healthy'
  | 'anomaly'
  | 'upload-failed'
  | 'auth-required'
  | 'cluster-degraded'
  | 'lease-risk'
  | 'control-plane-down-worker-running'

export interface MockTaskRow {
  task: any
  replication: any
}

export const scenarios = mockScenarios as Record<MockScenario, any>
