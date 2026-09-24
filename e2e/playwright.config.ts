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
  //
  // monitoring-api.spec.ts posts events and stats through the contract, so it
  // comes after them: it turns monitoring on, issues its own token and leaves
  // targets behind. monitoring-probe-configs.spec.ts drives the same
  // monEnable/monToken, so it follows the events spec.
  projects: [
    {
      name: 'panel',
      testIgnore: /(monitoring-(settings|cli|api|probe-configs)|inbounds-probe-guard)\.spec\.ts/,
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
    {
      name: 'mon-server-events',
      testMatch: /monitoring-api\.spec\.ts/,
      dependencies: ['mon-server-contact-cli'],
      workers: 1,
      use: { ...devices['Desktop Chrome'] },
    },
    // Turns monitoring on and issues its own token through `x-ui setting`, as
    // the events spec does through the settings form, so it runs after it
    // rather than beside it: two specs flipping one monEnable/monToken would
    // pull the token out from under each other.
    {
      name: 'mon-server-contact-probe-configs',
      testMatch: /monitoring-probe-configs\.spec\.ts/,
      dependencies: ['mon-server-events'],
      workers: 1,
      use: { ...devices['Desktop Chrome'] },
    },
    // Runs POST /probe/ensure (a real probe to rename, #115) with a token of
    // its own through `x-ui setting` — the same monEnable/monToken again, so
    // it follows the probe-configs spec. The ensure also stamps monLastContact
    // and creates a probe in every inbound, which the panel project must not
    // see.
    {
      name: 'mon-server-probe-guard',
      testMatch: /inbounds-probe-guard\.spec\.ts/,
      dependencies: ['mon-server-contact-probe-configs'],
      workers: 1,
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
