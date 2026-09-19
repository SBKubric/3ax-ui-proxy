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
  // Three projects, not one: monitoring-settings.spec.ts and
  // monitoring-cli.spec.ts talk to the mon-server contract (`GET /mon/v1/state`
  // with a real token), and every authorised contract request stamps the
  // panel's monLastContact. The other specs assert the "no monitoring data yet"
  // state of a panel no mon-server has reached, so the contract-touching specs
  // must run after them, never beside them. Project dependencies are
  // Playwright's ordering guarantee across files; fullyParallel still applies
  // inside each project.
  //
  // The two contract specs are also split from each other, and in this order:
  // both drive the one panel's monEnable and monToken (one through the settings
  // form, one through `x-ui setting`), so running them together would have each
  // pull the token out from under the other; and monitoring-settings.spec.ts
  // asserts the never-issued token of a fresh database, which the CLI spec
  // issues. Within a project, file order is not a guarantee Playwright gives —
  // a dependency is.
  projects: [
    {
      name: 'panel',
      testIgnore: /monitoring-(settings|cli)\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'mon-server-contact',
      testMatch: /monitoring-settings\.spec\.ts/,
      dependencies: ['panel'],
      workers: 1,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'mon-server-contact-cli',
      testMatch: /monitoring-cli\.spec\.ts/,
      dependencies: ['mon-server-contact'],
      workers: 1,
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
