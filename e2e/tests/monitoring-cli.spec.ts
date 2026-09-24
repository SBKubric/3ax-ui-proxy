import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { expect, test } from '@playwright/test';

/**
 * The `x-ui setting` monitoring flags (docs/spec/monitoring-panel.md §7.3):
 * the CLI duplicate of the Monitoring settings tab, for an operator who only
 * has a shell. This spec drives the real binary inside the compose container
 * and then checks, over HTTP, that what the CLI printed is exactly what
 * /mon/v1 accepts.
 *
 * API-only path: no page, no UI selectors — Playwright's `request` fixture
 * per docs/agents/testing.md.
 */

const COMPOSE_FILE = path.resolve(__dirname, '..', 'docker-compose.yml');
// The URL `playwright.config.ts` falls back to, i.e. the port
// `docker-compose.yml` publishes. Anything else means the panel under test is
// not the container this spec would exec into.
const COMPOSE_BASE_URL = 'http://127.0.0.1:2053';

test.skip(
  !!process.env.E2E_BASE_URL && process.env.E2E_BASE_URL !== COMPOSE_BASE_URL,
  'E2E_BASE_URL points at a panel outside this compose project — there is no container to exec `x-ui setting` in.',
);

/** Runs `/app/x-ui setting <args>` in the compose panel container and returns its stdout. */
function xuiSetting(...args: string[]): string {
  return execFileSync(
    'docker',
    ['compose', '-f', COMPOSE_FILE, 'exec', '-T', 'panel', '/app/x-ui', 'setting', ...args],
    { encoding: 'utf8' },
  );
}

/** The value of a `monEnable: ` / `monToken: ` line of the CLI output. */
function field(output: string, name: 'monEnable' | 'monToken'): string {
  const match = output.match(new RegExp(`^${name}: (.*)$`, 'm'));
  expect(match, `no "${name}: " line in:\n${output}`).not.toBeNull();
  return match![1].trim();
}

function issuedToken(output: string): string {
  const token = field(output, 'monToken');
  expect(token).not.toBe('(not issued)');
  expect(token).toHaveLength(32);
  return token;
}

test.describe('x-ui setting monitoring flags', () => {
  test('a token issued from the CLI is what /mon/v1 accepts, and the CLI can close it again', async ({
    request,
  }) => {
    // enable → reset: the final state, printed once (two lines, no repeats).
    const enabled = xuiSetting('-monEnable', 'true', '-resetMonToken');
    expect(enabled.trimEnd().split('\n')).toHaveLength(2);
    expect(field(enabled, 'monEnable')).toBe('true');
    const first = issuedToken(enabled);

    const state = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${first}` },
    });
    expect(state.status()).toBe(200);
    expect(state.headers()['x-mon-contract']).toBe('2');
    expect((await state.json()).contract).toBe(2);

    // The panel reads monToken on every request, so a fresh reset locks the
    // previous token out immediately — with a bare 404, not a 401.
    const second = issuedToken(xuiSetting('-resetMonToken'));
    expect(second).not.toBe(first);

    const withOld = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${first}` },
    });
    expect(withOld.status()).toBe(404);

    const withNew = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${second}` },
    });
    expect(withNew.status()).toBe(200);

    // -showMonToken reports the state without changing it.
    const shown = xuiSetting('-showMonToken');
    expect(field(shown, 'monEnable')).toBe('true');
    expect(field(shown, 'monToken')).toBe(second);

    // Closing monitoring shuts the contract for every token, valid one included.
    const disabled = xuiSetting('-monEnable', 'false');
    expect(field(disabled, 'monEnable')).toBe('false');

    const whenDisabled = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${second}` },
    });
    expect(whenDisabled.status()).toBe(404);
  });
});
