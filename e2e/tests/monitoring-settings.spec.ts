import { type Page } from '@playwright/test';
import { expect, test } from '../fixtures/panel';

// Both tests drive the one settings form of the one panel under test, so they
// must not run at the same time as each other.
test.describe.configure({ mode: 'serial' });

/**
 * The token field is an `<a-input>`; depending on the antd build the
 * data-testid lands either on the real `<input>` or on a wrapper around it.
 * Same shape as the login fixture's selector, and still keyed by testid.
 */
function tokenField(page: Page) {
  return page.locator('input[data-testid="mon-token"], [data-testid="mon-token"] input').first();
}

async function openMonitoringTab(page: Page) {
  await page.goto('/panel/settings');
  await page.getByRole('tab', { name: 'Monitoring' }).click();
}

// The Monitoring settings tab (docs/spec/monitoring-panel.md §7.3).
test.describe('monitoring settings', () => {
  test('regenerating the token is what /mon/v1 accepts, and the old one stops working', async ({
    authedPage,
    request,
  }) => {
    await openMonitoringTab(authedPage);

    const token = tokenField(authedPage);
    // A fresh database has monToken = "" (web/service/setting.go), so the field
    // starts empty and the operator's first act is to issue one.
    await expect(token).toHaveValue('');
    await expect(token).not.toBeEditable();

    // /mon/v1 is closed while monitoring is off, whatever the token is.
    await authedPage.getByTestId('mon-enable').click();

    // Issuing a token is a confirmed action: it invalidates whatever the
    // mon-server is currently using.
    await authedPage.getByTestId('mon-token-regenerate').click();
    await authedPage.getByRole('button', { name: 'Sure' }).click();
    await expect(token).not.toHaveValue('');
    const first = await token.inputValue();

    // The switch rides the page's common Save; the token does not, and this is
    // the interesting part — Save must not post the empty token the form was
    // loaded with back over the one just issued.
    await authedPage.getByRole('button', { name: 'Save', exact: true }).click();
    await openMonitoringTab(authedPage);
    await expect(tokenField(authedPage)).toHaveValue(first);

    const state = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${first}` },
    });
    expect(state.status()).toBe(200);
    expect((await state.json()).contract).toBe(2);

    // Regenerate again: the panel reads the setting on every request, so the
    // previous token is refused from this moment, with a bare 404 rather than
    // a 401 — the contract does not admit the endpoint exists.
    await authedPage.getByTestId('mon-token-regenerate').click();
    await authedPage.getByRole('button', { name: 'Sure' }).click();
    await expect(tokenField(authedPage)).not.toHaveValue(first);
    const second = await tokenField(authedPage).inputValue();

    const withOld = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${first}` },
    });
    expect(withOld.status()).toBe(404);

    const withNew = await request.get('/mon/v1/state', {
      headers: { Authorization: `Bearer ${second}` },
    });
    expect(withNew.status()).toBe(200);
  });

  test('with no probe set the line says so and Remove is disabled', async ({ authedPage }) => {
    await openMonitoringTab(authedPage);

    // Probe accounts are created by the mon-server's ensure call, never by the
    // panel, so a panel no mon-server has talked to has none.
    await expect(authedPage.getByTestId('mon-probe-line')).toContainText('no probe set yet');
    await expect(authedPage.getByTestId('mon-probe-remove')).toBeDisabled();
  });
});
