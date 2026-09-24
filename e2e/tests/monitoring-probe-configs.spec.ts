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

// Contract v2 (SBKubric/3ax-ui-monitoring#80): ensure takes monClientIds of
// 1–32 characters of [A-Za-z0-9_-] (they name AmneziaWG probe peers) and
// answers with the mon-clients left without a peer, `unallocated`. The
// compose panel has no AmneziaWG server, so nobody is left out.
test.describe('probe ensure, contract v2', () => {
  test('ensure answers contract 2 with unallocated and refuses a bad monClientId', async ({ request }) => {
    const output = xuiSetting('-monEnable', 'true', '-resetMonToken');
    const token = output.match(/^monToken: (.*)$/m)?.[1].trim();
    expect(token, `no token in:\n${output}`).toHaveLength(32);
    const headers = { Authorization: `Bearer ${token}` };

    try {
      const ok = await request.post('/mon/v1/probe/ensure', {
        headers,
        data: { monClients: [{ id: 'e2e-ams_1', name: 'e2e', region: 'NL', state: 'NEVER', lastHeartbeat: 0 }] },
      });
      expect(ok.status()).toBe(200);
      expect(ok.headers()['x-mon-contract']).toBe('2');
      const body = await ok.json();
      expect(body.unallocated).toEqual([]);
      expect(body.subId).toHaveLength(16);

      const state = await (await request.get('/mon/v1/state', { headers })).json();
      expect(state.contract).toBe(2);
      expect(state.revision).toBe(body.revision);

      const bad = await request.post('/mon/v1/probe/ensure', {
        headers,
        data: { monClients: [{ id: 'e2e.ams.1', state: 'NEVER' }] },
      });
      expect(bad.status()).toBe(400);
      expect((await bad.json()).error).toBe('invalid_body');
    } finally {
      await request.delete('/mon/v1/probe', { headers });
      xuiSetting('-monEnable', 'false');
    }
  });
});
