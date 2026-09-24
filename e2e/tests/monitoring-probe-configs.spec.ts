import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { expect, test } from '@playwright/test';

/**
 * `GET /mon/v1/probe/configs?hop=` and its synonym `?edge=`
 * (docs/spec/proxy-chain.md §6.1). Until per-hop probing lands every hop name
 * is unknown: 409 unknown_hop (unknown_edge through ?edge= alone), never the
 * proxy path a mon-server could take for a hop.
 *
 * API-only path: Playwright's `request` fixture per docs/agents/testing.md.
 * The token is issued through `x-ui setting` in the compose container, as in
 * monitoring-cli.spec.ts, and monitoring is closed again at the end.
 */

const COMPOSE_FILE = path.resolve(__dirname, '..', 'docker-compose.yml');
const COMPOSE_BASE_URL = 'http://127.0.0.1:2053';

test.skip(
  !!process.env.E2E_BASE_URL && process.env.E2E_BASE_URL !== COMPOSE_BASE_URL,
  'E2E_BASE_URL points at a panel outside this compose project — there is no container to exec `x-ui setting` in.',
);

function xuiSetting(...args: string[]): string {
  return execFileSync(
    'docker',
    ['compose', '-f', COMPOSE_FILE, 'exec', '-T', 'panel', '/app/x-ui', 'setting', ...args],
    { encoding: 'utf8' },
  );
}

test.describe('probe configs per hop', () => {
  test('?hop= and ?edge= are 409 until per-hop probing', async ({ request }) => {
    const output = xuiSetting('-monEnable', 'true', '-resetMonToken');
    const token = output.match(/^monToken: (.*)$/m)?.[1].trim();
    expect(token, `no token in:\n${output}`).toHaveLength(32);
    const headers = { Authorization: `Bearer ${token}` };

    try {
      for (const [query, code] of [
        ['?hop=ams-1', 'unknown_hop'],
        ['?edge=ams-1', 'unknown_edge'],
        ['?hop=ams-1&edge=core-1', 'unknown_hop'],
        ['?host=203.0.113.10&hop=ams-1', 'unknown_hop'],
      ]) {
        const res = await request.get(`/mon/v1/probe/configs${query}`, { headers });
        expect(res.status(), query).toBe(409);
        expect((await res.json()).error, query).toBe(code);
      }
    } finally {
      xuiSetting('-monEnable', 'false');
    }
  });
});
