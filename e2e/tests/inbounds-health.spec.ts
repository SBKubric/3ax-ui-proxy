import { createInbound } from '../fixtures/inbound';
import { expect, test } from '../fixtures/panel';

// The Health column and the "down" chip on the inbounds page
// (docs/spec/monitoring-panel.md §7.2).
test.describe('inbounds Health column', () => {
  test('an inbound no mon-client has reported on reads as an em dash', async ({
    authedPage,
    authedRequest,
  }) => {
    const id = await createInbound(authedRequest, 'e2e-health-dash', 24102);

    await authedPage.goto('/panel/inbounds');

    // Not UP: the panel has no monitoring data for this inbound, and the column
    // says that rather than guessing.
    await expect(authedPage.getByTestId(`health-${id}`)).toHaveText('—');
  });

  test('the down chip keeps only inbounds whose worst target is DOWN or FLAPPING', async ({
    authedPage,
    authedRequest,
  }) => {
    const id = await createInbound(authedRequest, 'e2e-health-filter', 24103);

    await authedPage.goto('/panel/inbounds');
    const health = authedPage.getByTestId(`health-${id}`);
    await expect(health).toBeVisible();

    // The chips live behind the search/filter switch.
    await authedPage.getByTestId('filter-switch').click();
    const down = authedPage.getByTestId('filter-down');
    await expect(down).toBeVisible();

    await down.click();

    // Unlike the other chips, this one filters whole inbounds rather than an
    // inbound's clients — and nothing here is down, so the row goes away.
    await expect(health).toHaveCount(0);
  });
});
