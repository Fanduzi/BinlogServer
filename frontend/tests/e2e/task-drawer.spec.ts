// input: healthy dashboard mock scenario with task list and detail endpoints
// output: regression coverage for task detail drawer content and action affordances
// pos: Playwright E2E coverage for opening task detail from the task workspace
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

test('task drawer shows operator details and action area', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'healthy' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-100').click()
  await expect(page.getByTestId('task-drawer')).toBeVisible()
  await expect(page.getByTestId('task-drawer-status')).toContainText('运行中')
  await expect(page.getByTestId('task-drawer-replication')).toContainText('正常')
  await expect(page.getByTestId('task-drawer-checkpoint')).toContainText('mysql-bin.000001:12345')
  await expect(page.getByTestId('task-drawer-worker')).toContainText('worker-a')
  await expect(page.getByTestId('task-drawer-events')).toBeVisible()
  await expect(page.getByTestId('task-drawer-runs')).toBeVisible()
  await expect(page.getByTestId('task-drawer-actions')).toBeVisible()
  await expect(page.getByTestId('task-action-edit')).toBeVisible()
  await expect(page.getByTestId('task-action-start')).toBeVisible()
  await expect(page.getByTestId('task-action-stop')).toBeVisible()
  await expect(page.getByTestId('task-action-delete')).toBeVisible()
})
