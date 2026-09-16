// input: shared frontend mock route registration with the lease-risk scenario
// output: regression coverage for stale lease labeling from the task copy in the list, plus single-task GET /lease in the detail drawer
// pos: Playwright-backed scenario test for cluster lease risk visibility
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from "@playwright/test";
import { registerMockRoutes } from "./fixtures/mock-routes";

test("lease risk scenario surfaces stale lease labels in list and detail views", async ({
  page,
}) => {
  const leaseRequests: string[] = [];
  page.on("request", (request) => {
    if (/\/api\/tasks\/[^/]+\/lease$/.test(new URL(request.url()).pathname)) {
      leaseRequests.push(request.url());
    }
  });

  await registerMockRoutes(page, { scenario: "lease-risk" });
  await page.goto("/#/tasks");

  await expect(
    page
      .locator("tr", { hasText: "task-stale-lease" })
      .getByText("风险", { exact: true }),
  ).toBeVisible();
  expect(leaseRequests).toEqual([]);
  await page.getByTestId("task-detail-trigger-501").click();
  await expect(page.getByTestId("task-drawer")).toBeVisible();
  await expect.poll(() => leaseRequests.map((url) => new URL(url).pathname)).toEqual([
    "/api/tasks/501/lease",
  ]);
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
