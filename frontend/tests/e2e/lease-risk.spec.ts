// input: shared frontend mock route registration with the lease-risk scenario
// output: regression coverage for stale lease labeling in task list and detail drawer
// pos: Playwright-backed scenario test for cluster lease risk visibility
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from "@playwright/test";
import { registerMockRoutes } from "./fixtures/mock-routes";

test("lease risk scenario surfaces stale lease labels in list and detail views", async ({
  page,
}) => {
  await registerMockRoutes(page, { scenario: "lease-risk" });
  await page.goto("/#/tasks");

  await expect(
    page
      .locator("tr", { hasText: "task-stale-lease" })
      .getByText("风险", { exact: true }),
  ).toBeVisible();
  await page.getByTestId("task-detail-trigger-501").click();
  await expect(page.getByTestId("task-drawer")).toBeVisible();
  await expect(
    page
      .locator(".detail-panel", { hasText: "Lease 与 Worker" })
      .getByText("Lease 状态"),
  ).toBeVisible();
  await expect(
    page
      .locator(".detail-panel", { hasText: "Lease 与 Worker" })
      .getByText("worker-stale"),
  ).toBeVisible();
});
