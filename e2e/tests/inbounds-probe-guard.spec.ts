import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { expect, test } from '../fixtures/panel';
import { createInbound } from '../fixtures/inbound';

/**
 * Probe accounts (`probe-*`) come from `POST /mon/v1/probe/ensure` only
 * (docs/spec/monitoring-panel.md §3): the inbound add and edit paths refuse a
 * probe email the inbound did not already have. Walked through the panel API
 * the inbound form posts to — Playwright's request context per
 * docs/agents/testing.md.
 *
 * Nor may a user take a probe's identity under another name (#115): renaming
 * `probe-<id>` into a user, or giving a user the probe's uuid or the panel's
 * probe subId, is refused too. That case needs a real probe, so the spec runs
 * `POST /mon/v1/probe/ensure` with a token issued through `x-ui setting` in the
 * compose container — which is why it lives in its own Playwright project
 * after the other contract specs (playwright.config.ts).
 */

const COMPOSE_FILE = path.resolve(__dirname, '..', 'docker-compose.yml');
const COMPOSE_BASE_URL = 'http://127.0.0.1:2053';

function xuiSetting(...args: string[]): string {
  return execFileSync(
    'docker',
    ['compose', '-f', COMPOSE_FILE, 'exec', '-T', 'panel', '/app/x-ui', 'setting', ...args],
    { encoding: 'utf8' },
  );
}

function inboundBody(remark: string, port: number, emails: string[]) {
  return {
    up: 0,
    down: 0,
    total: 0,
    remark,
    enable: false,
    expiryTime: 0,
    listen: '',
    port,
    protocol: 'vless',
    settings: JSON.stringify({
      clients: emails.map((email, i) => ({
        id: `aaaaaaaa-0000-0000-0000-${String(port).padStart(8, '0')}${String(i).padStart(4, '0')}`,
        email,
        enable: true,
      })),
      decryption: 'none',
      fallbacks: [],
    }),
    streamSettings: JSON.stringify({ network: 'tcp', security: 'none', tcpSettings: { header: { type: 'none' } } }),
    sniffing: JSON.stringify({ enabled: false, destOverride: [] }),
  };
}

test.describe('probe guard on inbounds', () => {
  test('adding an inbound with a probe-* client is refused', async ({ authedRequest }) => {
    const res = await authedRequest.post('/panel/api/inbounds/add', {
      data: inboundBody('e2e-probe-guard-add', 24111, ['e2e-guard-alice', 'probe-1']),
    });
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body.success).toBe(false);
    expect(body.msg).toContain('reserved for monitoring probes');
  });

  test('editing an inbound cannot add a probe-* client', async ({ authedRequest }) => {
    const id = await createInbound(authedRequest, 'e2e-probe-guard-edit', 24112);
    const res = await authedRequest.post(`/panel/api/inbounds/update/${id}`, {
      data: inboundBody('e2e-probe-guard-edit', 24112, ['e2e-guard-bob', 'probe-2']),
    });
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body.success).toBe(false);
    expect(body.msg).toContain('reserved for monitoring probes');
  });

  test('a probe renamed into a user keeps none of its identity', async ({ authedRequest, request }) => {
    test.skip(
      !!process.env.E2E_BASE_URL && process.env.E2E_BASE_URL !== COMPOSE_BASE_URL,
      'E2E_BASE_URL points at a panel outside this compose project — there is no container to exec `x-ui setting` in.',
    );
    const port = 24113;
    const created = await authedRequest.post('/panel/api/inbounds/add', {
      data: inboundBody('e2e-probe-guard-rename', port, ['e2e-guard-alice3']),
    });
    const createdBody = await created.json();
    expect(createdBody.success, createdBody.msg).toBe(true);
    const id = createdBody.obj.id as number;

    const output = xuiSetting('-monEnable', 'true', '-resetMonToken');
    const token = output.match(/^monToken: (.*)$/m)?.[1].trim();
    expect(token, `no token in:\n${output}`).toHaveLength(32);
    try {
      const ensured = await request.post('/mon/v1/probe/ensure', {
        headers: { Authorization: `Bearer ${token}` },
        data: { monClients: [] },
      });
      expect(ensured.status()).toBe(200);
      const probeSubId = (await ensured.json()).subId as string;
      expect(probeSubId).not.toBe('');

      const got = await (await authedRequest.get(`/panel/api/inbounds/get/${id}`)).json();
      expect(got.success, got.msg).toBe(true);
      const settings = JSON.parse(got.obj.settings);
      const probe = settings.clients.find((c: { email: string }) => c.email === `probe-${id}`);
      expect(probe, `no probe-${id} after ensure`).toBeTruthy();
      const others = settings.clients.filter((c: { email: string }) => c !== probe);

      const update = (clients: object[]) =>
        authedRequest.post(`/panel/api/inbounds/update/${id}`, {
          data: { ...inboundBody('e2e-probe-guard-rename', port, []), settings: JSON.stringify({ ...settings, clients }) },
        });
      const refused = async (res: Awaited<ReturnType<typeof update>>, what: string) => {
        expect(res.status(), what).toBe(200);
        const body = await res.json();
        expect(body.success, what).toBe(false);
        expect(body.msg, what).toContain('identity of a monitoring probe');
      };

      // The rename: probe-<id> becomes carol with the probe's uuid and subId.
      await refused(await update([...others, { ...probe, email: 'e2e-guard-carol' }]), 'rename');
      // A user on the probe subId, beside the probe it does not replace.
      await refused(
        await update([...others, probe, { id: 'aaaaaaaa-0000-0000-0000-000024113099', email: 'e2e-guard-dave', enable: true, subId: probeSubId }]),
        'user on the probe subId',
      );
      // The add-client path: the probe subId, or the probe's uuid.
      for (const [what, client] of [
        ['addClient with the probe subId', { id: 'aaaaaaaa-0000-0000-0000-000024113098', email: 'e2e-guard-erin', enable: true, subId: probeSubId }],
        ['addClient with the probe uuid', { id: probe.id, email: 'e2e-guard-erin', enable: true, subId: 'e2eguarderin' }],
      ] as const) {
        const res = await authedRequest.post('/panel/api/inbounds/addClient', {
          data: { id, settings: JSON.stringify({ clients: [client] }) },
        });
        await refused(res, what);
      }

      // Dropping the probe stays allowed (the next ensure recreates it).
      const dropped = await (await update(others)).json();
      expect(dropped.success, dropped.msg).toBe(true);
    } finally {
      xuiSetting('-monEnable', 'false');
    }
  });
});
