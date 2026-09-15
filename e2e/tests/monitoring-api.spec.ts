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
