// input: shared frontend mock route registration with the single-process scenario
// output: regression coverage that overview and the workers page both describe local pull
// pos: Playwright E2E coverage for single-process console copy
// note: if this file changes, update this header and frontend/tests/e2e/README.md

import { test, expect } from "@playwright/test";
import { registerMockRoutes } from "./fixtures/mock-routes";

test("single process overview and workers page agree", async ({ page }) => {
  await registerMockRoutes(page, { scenario: "single-process" });
  await page.goto("/");

  const summary = page.getByTestId("cluster-worker-summary");
  await expect(summary).toHaveText("单机，任务由本进程拉取");
  await expect(page.getByText("1 个 Worker")).toHaveCount(0);

  await page.getByTestId("view-nav-workers").click();
  await expect(page.getByTestId("workers-empty")).toContainText("单机，任务由本进程拉取");
  await expect(page.getByTestId("view-nav-workers").locator(".nav-badge")).toHaveText("0");
});
