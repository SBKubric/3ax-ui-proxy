import { randomUUID } from 'node:crypto';
import { type APIRequestContext } from '@playwright/test';
import { createInbound } from '../fixtures/inbound';
import { expect, test } from '../fixtures/panel';

// API-only feature (docs/spec/monitoring-panel.md §7.4) — walked through
// Playwright's request context per docs/agents/testing.md, not through the UI.
test.describe('monitoring API', () => {
  test('authenticated GET targets returns success with an inbounds array', async ({ authedRequest }) => {
    const res = await authedRequest.get('/panel/api/monitoring/targets');
    expect(res.status()).toBe(200);

    const body = await res.json();
    expect(body.success).toBe(true);
    expect(Array.isArray(body.obj.inbounds)).toBe(true);
  });

  test('unauthenticated GET targets is a 404', async ({ request }) => {
    const res = await request.get('/panel/api/monitoring/targets');
    expect(res.status()).toBe(404);
  });
});

/**
 * Switches monitoring on through the settings form's own round trip and
 * issues a fresh token the way the Regenerate button does. Returns the token.
 */
async function openContract(authedRequest: APIRequestContext): Promise<string> {
  const all = await (await authedRequest.post('/panel/setting/all')).json();
  expect(all.success).toBe(true);
  const saved = await authedRequest.post('/panel/setting/update', { data: { ...all.obj, monEnable: true } });
  expect((await saved.json()).success).toBe(true);

  const reset = await (await authedRequest.post('/panel/api/monitoring/token/reset')).json();
  expect(reset.success).toBe(true);
  return reset.obj.token as string;
}

async function closeContract(authedRequest: APIRequestContext): Promise<void> {
  const all = await (await authedRequest.post('/panel/setting/all')).json();
  await authedRequest.post('/panel/setting/update', { data: { ...all.obj, monEnable: false } });
}

// The events contract as mon-server speaks it (SBKubric/3ax-ui-monitoring#50):
// a batch is validated element by element, unknown fields are ignored, and a
// hop of the chain is an ordinary path. This spec talks to /mon/v1 with a real
// token, so it runs in its own project after the other contract specs
// (playwright.config.ts).
test.describe('monitoring events contract', () => {
  test('a mixed batch keeps the good elements and names the bad ones by index', async ({
    authedPage,
    authedRequest,
    request,
  }) => {
    const remark = 'e2e-mon-events';
    const inboundId = await createInbound(authedRequest, remark, 24201);
    const token = await openContract(authedRequest);
    const auth = { Authorization: `Bearer ${token}` };

    try {
      const good = randomUUID();
      const bad = randomUUID();
      const now = Date.now();
      const events = await request.post('/mon/v1/events', {
        headers: auth,
        data: {
          batchId: 'from-a-newer-mon-server',
          events: [
            {
              id: good, ts: now, kind: 'target', monClientId: 'e2e-client', inboundKind: 'xray', inboundId,
              path: 'edge:e2e-x', from: '', to: 'DOWN', reason: 'tcp_refused', notified: true,
              hopRole: 'edge',
            },
            { id: bad, ts: now, kind: 'target', monClientId: 'e2e-client', inboundKind: 'xray', inboundId,
              path: 'tunnel', to: 'DOWN', notified: true },
          ],
        },
      });
      expect(events.status()).toBe(200);
      const res = await events.json();
      expect(res.accepted).toBe(1);
      expect(res.rejected).toHaveLength(1);
      expect(res.rejected[0]).toMatchObject({ index: 1, id: bad });
      expect(res.rejected[0].error).toContain('events[1].path');

      const bucketStart = Math.floor(now / 300000) * 300000;
      const stats = await request.post('/mon/v1/stats', {
        headers: auth,
        data: {
          stats: [
            { monClientId: 'e2e-client', inboundKind: 'xray', inboundId, path: 'edge:e2e-x', bucketStart,
              nOk: 0, nFail: 5, latencyMinMs: null, latencyAvgMs: null, latencyMaxMs: null, handshakeMs: null,
              jitterMs: 1 },
            { monClientId: 'e2e-client', inboundKind: 'xray', inboundId, path: 'edge:e2e-x', bucketStart: 7,
              nOk: 1, nFail: 0 },
          ],
        },
      });
      expect(stats.status()).toBe(200);
      const statsRes = await stats.json();
      expect(statsRes.accepted).toBe(1);
      expect(statsRes.rejected).toEqual([expect.objectContaining({ index: 1 })]);

      // Only a body that cannot be read at all is a 400 for the whole batch.
      const unreadable = await request.post('/mon/v1/events', {
        headers: { ...auth, 'Content-Type': 'application/json' },
        data: '{"events": {',
      });
      expect(unreadable.status()).toBe(400);
      expect((await unreadable.json()).error).toBe('invalid_body');

      // The hop path is stored and shown as it came.
      const targets = await (await authedRequest.get('/panel/api/monitoring/targets')).json();
      const inbound = targets.obj.inbounds.find((ib: { inboundId: number }) => ib.inboundId === inboundId);
      expect(inbound.targets).toEqual([expect.objectContaining({ path: 'edge:e2e-x', state: 'DOWN' })]);

      await authedPage.goto('/panel/monitoring');
      const card = authedPage.getByTestId(`mon-inbound-xray-${inboundId}`);
      await expect(card.getByText('edge:e2e-x', { exact: true })).toBeVisible();
    } finally {
      await closeContract(authedRequest);
    }
  });
});
