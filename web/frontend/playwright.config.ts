import { defineConfig, devices } from "@playwright/test";

// Phase 8 E2E targets a fully wired stack (postgres + main-api + ssh-proxy +
// compute-agent). Default points at the h20a deployment via qlaud.net; flip
// BASE_URL to http://localhost:3000 to drive the local dev stack instead.
const BASE_URL = process.env.BASE_URL ?? "http://qlaud.net";

export default defineConfig({
  testDir: "./e2e",
  // The full journey provisions a real VM, so it can take 2-5 min on first
  // run. 8 minutes leaves room for slow boots without flagging spurious
  // failures.
  timeout: 8 * 60 * 1000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: process.env.CI ? "list" : [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: BASE_URL,
    trace: "retain-on-failure",
    video: "retain-on-failure",
    // The dashboard is Korean, but Playwright's default locale is fine; we
    // match elements by Korean labels directly.
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
});
