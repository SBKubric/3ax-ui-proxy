import { expect, test } from '../fixtures/panel';
import { createInbound } from '../fixtures/inbound';

/**
 * Probe accounts (`probe-*`) come from `POST /mon/v1/probe/ensure` only
 * (docs/spec/monitoring-panel.md §3): the inbound add and edit paths refuse a
 * probe email the inbound did not already have. Walked through the panel API
 * the inbound form posts to — Playwright's request context per
 * docs/agents/testing.md.
 */

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
});
