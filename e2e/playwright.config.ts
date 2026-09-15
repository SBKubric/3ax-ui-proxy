import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright config for the 3AX-UI panel e2e harness.
 *
 * The app under test is the repo's own Docker image, started by
 * `e2e/docker-compose.yml` (see `make e2e`). `E2E_BASE_URL` lets a spec
 * author point at a panel already running elsewhere; it defaults to the
 * port `docker-compose.yml` publishes.
 */
export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL: process.env.E2E_BASE_URL || 'http://127.0.0.1:2053',
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
