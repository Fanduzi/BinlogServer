// input: shared frontend mock route registration with the cluster-degraded scenario
// output: regression coverage for degraded cluster overview and multi-risk task surfacing
// pos: Playwright-backed scenario test for degraded cluster operations visibility
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from "@playwright/test";
import { registerMockRoutes } from "./fixtures/mock-routes";

test("cluster degraded scenario shows offline worker and mixed-risk tasks", async ({
  page,
}) => {
  await registerMockRoutes(page, { scenario: "cluster-degraded" });
  await page.goto("/");

  await expect(page.getByText("2 个 Worker")).toBeVisible();
  await page.getByTestId("view-nav-workers").click();
  await expect(
    page.locator(".cluster-worker-item", { hasText: "worker-b" }),
  ).toBeVisible();
  await expect(
    page
      .locator(".cluster-worker-item", { hasText: "worker-b" })
      .getByText("离线"),
  ).toBeVisible();
  await expect(page.getByTestId("kpi-abnormal-value")).toHaveText("1");
  await expect(page.getByTestId("kpi-delayed-value")).toHaveText("1");

  await page.goto("/#/tasks");
  await expect(page.getByTestId("task-row-401")).toBeVisible();
  await expect(page.getByTestId("task-row-402")).toBeVisible();
  await expect(page.getByTestId("task-row-403")).toBeVisible();
});
