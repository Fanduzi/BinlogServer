// input: stopped-task mock with LATEST, FILE_POS, and GTID starts plus checkpoint resume positions
// output: proof the task drawer shows the configured start and the resume file:pos / GTID
// pos: Playwright coverage for the operator-visible resume identity
// note: if this file changes, update this header and console/tests/e2e/README.md

import { test, expect } from '@playwright/test'
import { registerMockRoutes } from './fixtures/mock-routes'

const GTID = '24bc785e-9a61-11e1-8a5d-080027635ef5:1-20'

test('stopped task detail shows start and resume identity in Chinese', async ({ page }) => {
  await registerMockRoutes(page, { scenario: 'resume-identity' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-41').click()
  const latest = page.getByTestId('task-drawer')
  await expect(latest).toBeVisible()
  await expect(page.getByTestId('task-drawer-status')).toContainText('已停止')
  await expect(latest.getByText('起点', { exact: true }).first()).toBeVisible()
  await expect(latest.getByText('续传', { exact: true }).first()).toBeVisible()
  await expect(page.getByTestId('task-drawer-start')).toHaveText('LATEST')
  await expect(page.getByTestId('task-drawer-resume')).toHaveText(`mysql-bin.000003:154 GTID ${GTID}`)
  await expect(page.getByTestId('task-drawer-checkpoint')).toHaveText(`mysql-bin.000003:154 GTID ${GTID}`)

  await page.keyboard.press('Escape')
  await expect(page.getByTestId('task-drawer')).toBeHidden()
  await page.getByTestId('task-detail-trigger-42').click()
  await expect(page.getByTestId('task-drawer-start')).toHaveText('FILE_POS mysql-bin.000008:128')
  await expect(page.getByTestId('task-drawer-resume')).toContainText('暂无，下次 Start 使用起点')

  await page.keyboard.press('Escape')
  await expect(page.getByTestId('task-drawer')).toBeHidden()
  await page.getByTestId('task-detail-trigger-43').click()
  await expect(page.getByTestId('task-drawer-start')).toHaveText('GTID 3e11fa47-71ca-11e1-9e33-c80aa9429562:1-100')
  await expect(page.getByTestId('task-drawer-resume')).toHaveText('mysql-bin.000011:88')
})

test('stopped task detail shows start and resume identity in English', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('locale', 'en'))
  await registerMockRoutes(page, { scenario: 'resume-identity' })
  await page.goto('/#/tasks')

  await page.getByTestId('task-detail-trigger-41').click()
  const drawer = page.getByTestId('task-drawer')
  await expect(drawer.getByText('Start', { exact: true }).first()).toBeVisible()
  await expect(drawer.getByText('Resume', { exact: true }).first()).toBeVisible()
  await expect(page.getByTestId('task-drawer-start')).toHaveText('LATEST')
  await expect(page.getByTestId('task-drawer-resume')).toHaveText(`mysql-bin.000003:154 GTID ${GTID}`)

  await page.keyboard.press('Escape')
  await expect(page.getByTestId('task-drawer')).toBeHidden()
  await page.getByTestId('task-detail-trigger-42').click()
  await expect(page.getByTestId('task-drawer-start')).toHaveText('FILE_POS mysql-bin.000008:128')
  await expect(page.getByTestId('task-drawer-resume')).toContainText('None. The next Start uses the configured start.')
})
