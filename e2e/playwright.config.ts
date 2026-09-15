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
  // Two projects, not one: monitoring-settings.spec.ts talks to the mon-server
  // contract (`GET /mon/v1/state` with a real token), and every authorised
  // contract request stamps the panel's monLastContact. The other specs assert
  // the "no monitoring data yet" state of a panel no mon-server has reached,
  // so the contract-touching spec must run after them, never beside them.
  // Project dependencies are Playwright's ordering guarantee across files;
  // fullyParallel still applies inside each project.
  projects: [
    {
      name: 'panel',
      testIgnore: /monitoring-settings\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'mon-server-contact',
      testMatch: /monitoring-settings\.spec\.ts/,
      dependencies: ['panel'],
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
