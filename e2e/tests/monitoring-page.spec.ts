import { createInbound } from '../fixtures/inbound';
import { expect, test } from '../fixtures/panel';

// The Monitoring page (docs/spec/monitoring-panel.md §7.1). Nothing here feeds
// mon-server data in — the panel under test has never been contacted by one —
// so these specs pin down what the page says when it knows nothing, which is
// the state an operator sees first and the one most easily got wrong: "no
// monitoring data yet" is not the same as "everything is UP".
test.describe('monitoring page', () => {
  test('the sidebar opens the Monitoring page', async ({ authedPage }) => {
    // The sider and the mobile drawer render the same menu, so the link exists
    // twice; the first is the one on screen at desktop width.
    const link = authedPage.getByRole('menuitem', { name: 'Monitoring' }).first();
    await expect(link).toBeVisible();

    await link.click();

    await expect(authedPage).toHaveURL(/\/panel\/monitoring$/);
    await expect(authedPage.getByTestId('mon-title')).toBeVisible();
  });

  test('with no mon-server the page says so rather than showing health', async ({ authedPage }) => {
    await authedPage.goto('/panel/monitoring');

    await expect(authedPage.getByTestId('mon-title')).toHaveText('Monitoring');
    // The live pill is the page's one claim about mon-server. Never contacted
    // means neither live nor stale: it reads "no monitoring data yet".
    await expect(authedPage.getByTestId('mon-live')).toContainText('no monitoring data yet');
    await expect(authedPage.getByTestId('mon-live')).not.toContainText('monitoring live');
    // The feed is always present, empty or not.
    await expect(authedPage.getByTestId('mon-events')).toContainText('no events yet');
  });

  test('an inbound gets a card whose badge is an em dash until a target reports', async ({
    authedPage,
    authedRequest,
  }) => {
    const remark = 'e2e-mon-page';
    const id = await createInbound(authedRequest, remark, 24101);

    await authedPage.goto('/panel/monitoring');

    const card = authedPage.getByTestId(`mon-inbound-xray-${id}`);
    await expect(card).toBeVisible();
    await expect(card).toContainText(remark);
    await expect(card).toContainText('vless');
    // No mon-client has ever reported on this inbound, so the worst-state badge
    // has no state to show. An em dash, not UP.
    await expect(card.getByText('—', { exact: true })).toBeVisible();
  });

  test('a chain hop gets a path chip, and no summary while no edge is active', async ({
    authedPage,
    authedRequest,
  }) => {
    const name = 'e2e-mon-chip';
    const added = await (
      await authedRequest.post('/panel/api/chain/add', { data: { name, host: 'chip.e2e.example', role: 'inner' } })
    ).json();
    expect(added.success).toBe(true);
    try {
      await authedPage.goto('/panel/monitoring');
      // Per hop (proxy-chain.md §6.4): one chip per hop beside all and direct.
      await expect(authedPage.getByTestId('mon-path-filter')).toBeVisible();
      await expect(authedPage.getByTestId('mon-path-all')).toBeVisible();
      await expect(authedPage.getByTestId('mon-path-direct')).toBeVisible();
      await authedPage.getByTestId(`mon-path-inner:${name}`).click();
      // The summary speaks about the active edge, and there is none.
      await expect(authedPage.getByTestId('mon-chain-summary')).toHaveCount(0);
    } finally {
      await authedRequest.post(`/panel/api/chain/del/${added.obj.hop.id}`, { data: { force: true, skipDrain: true } });
    }
  });
});
