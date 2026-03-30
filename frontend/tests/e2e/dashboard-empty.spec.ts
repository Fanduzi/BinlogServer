// input: empty dashboard mock scenario with split-view navigation
// output: regression coverage for zero-state KPIs plus workers/sources/tasks empty views
// pos: Playwright E2E coverage for empty-state rendering across overview and workspaces
// note: if this file changes, update this header and frontend/README.md

import { test, expect } from "@playwright/test";
import { registerMockRoutes } from "./fixtures/mock-routes";

test("dashboard empty shows zero KPIs and empty states", async ({ page }) => {
  await registerMockRoutes(page, { scenario: "empty" });
  await page.goto("/");

  await expect(page.getByTestId("kpi-abnormal-value")).toHaveText("0");
  await expect(page.getByTestId("kpi-failed-value")).toHaveText("0");
  await expect(page.getByTestId("kpi-delayed-value")).toHaveText("0");
  await expect(page.getByText("0 个 Worker")).toBeVisible();

  await page.getByTestId("view-nav-workers").click();
  await expect(page.getByTestId("workers-empty")).toBeVisible();

  await page.getByTestId("view-nav-sources").click();
  await expect(page.getByTestId("sources-empty")).toBeVisible();

  await page.goto("/#/tasks");
  await expect(page.getByTestId("task-table-empty")).toBeVisible();
});
