import { type APIRequestContext, type Page } from '@playwright/test';
import { expect, test } from '../fixtures/panel';

/**
 * The chain editor of Settings → Subscription (docs/spec/proxy-chain.md §7)
 * and the registry API under it (§2.4).
 *
 * Both tests drive the one registry of the one panel under test, so they run
 * one after the other rather than beside each other.
 */
test.describe.configure({ mode: 'serial' });

async function openSubscriptionTab(page: Page) {
  await page.goto('/panel/settings');
  // Two tabs carry the word: "Subscription" and "Subscription (Formats)".
  // The editor lives in the first, and only its name ends there.
  await page.getByRole('tab', { name: /Subscription$/ }).click();
  await expect(page.getByTestId('chain-editor')).toBeVisible();
}

/** The antd select of a form item, opened and picked by the option's text. */
async function pickOption(page: Page, testId: string, option: string) {
  await page.getByTestId(testId).click();
  await page.locator('.ant-select-dropdown:visible').getByText(option, { exact: true }).click();
}

/** An <a-input>'s real input, whichever shape the antd build renders. */
function field(page: Page, testId: string) {
  return page.locator(`input[data-testid="${testId}"], [data-testid="${testId}"] input`).first();
}

/** Removes a hop through the API, so a failed test cannot poison the next one. */
async function removeHop(request: APIRequestContext, name: string) {
  const list = await (await request.get('/panel/api/chain/list')).json();
  const hop = (list.obj?.hops || []).find((h: { name: string }) => h.name === name);
  if (hop) await request.post(`/panel/api/chain/del/${hop.id}`, { data: { force: true } });
}

test.describe('chain editor', () => {
  test('adding a hop hands out its join token once, and reissue replaces it', async ({
    authedPage,
    authedRequest,
  }) => {
    await removeHop(authedRequest, 'e2e-inner');
    await openSubscriptionTab(authedPage);

    // A panel nobody has built a chain on says so rather than showing an empty
    // list that could be mistaken for a chain that is down.
    await expect(authedPage.getByTestId('chain-empty')).toBeVisible();
    // The Proxy front row of the general section is read-only now: the host
    // clients get is the active edge's, and there is no active edge yet.
    await expect(authedPage.getByTestId('chain-override-readonly')).toContainText('—');

    await authedPage.getByTestId('chain-add-hop').click();
    await field(authedPage, 'chain-add-hop-name').fill('e2e-inner');
    await field(authedPage, 'chain-add-hop-host').fill('inner.e2e.example');
    await pickOption(authedPage, 'chain-add-hop-role', 'inner');
    // The position selector only exists for an inner front: every edge hangs
    // off the last inner, which the panel works out itself (§7.2).
    await expect(authedPage.getByTestId('chain-add-hop-position')).toBeVisible();
    await authedPage.getByRole('button', { name: 'OK' }).click();

    const row = authedPage.getByTestId('chain-hop-e2e-inner');
    await expect(row).toBeVisible();
    await expect(row).toContainText('pending');
    // A pending hop has no traffic through it and no monitoring data either.
    await expect(authedPage.getByTestId('chain-hop-e2e-inner-health')).toHaveText('UNKNOWN');
    // The token is shown whole exactly once: the panel keeps only its hash.
    const token = authedPage.getByTestId('chain-hop-e2e-inner-token');
    await expect(token).toBeVisible();
    const shown = (await token.textContent())?.trim() || '';
    expect(shown).toMatch(/[0-9a-zA-Z]{32}/);

    await authedPage.getByTestId('chain-hop-e2e-inner-token-copy').click();

    // Reloading the page loses it — the registry cannot hand it out again, so
    // the row falls back to the expiry and the Reissue button.
    await openSubscriptionTab(authedPage);
    await expect(authedPage.getByTestId('chain-hop-e2e-inner-token')).not.toContainText(shown.slice(0, 16));
    await authedPage.getByTestId('chain-hop-e2e-inner-reissue').click();
    await authedPage.getByRole('button', { name: 'Sure' }).click();
    const reissued = authedPage.getByTestId('chain-hop-e2e-inner-token');
    await expect(reissued).toBeVisible();
    expect((await reissued.textContent())?.trim()).not.toContain(shown.slice(0, 16));

    // Delete is a confirmed action, and the row goes with it.
    await authedPage.getByTestId('chain-hop-e2e-inner-delete').click();
    await authedPage.getByRole('button', { name: 'Sure' }).click();
    await expect(authedPage.getByTestId('chain-hop-e2e-inner')).toHaveCount(0);
  });

  test('the registry API hands the token out once and refuses what the invariants forbid', async ({
    authedRequest,
  }) => {
    await removeHop(authedRequest, 'e2e-edge');

    const added = await (
      await authedRequest.post('/panel/api/chain/add', {
        data: { name: 'e2e-edge', host: 'edge.e2e.example', role: 'edge' },
      })
    ).json();
    expect(added.success).toBe(true);
    expect(added.obj.joinToken).toHaveLength(32);
    expect(added.obj.hop.state).toBe('pending');
    const id = added.obj.hop.id;

    // GET list is what the editor repeats; it carries no token and no hash.
    const listBody = await (await authedRequest.get('/panel/api/chain/list')).text();
    expect(listBody).not.toContain(added.obj.joinToken);
    expect(listBody).not.toContain('joinTokenHash');
    expect(listBody).not.toContain('secretHash');
    const list = JSON.parse(listBody);
    expect(list.obj.hops.some((h: { name: string }) => h.name === 'e2e-edge')).toBe(true);
    // Nothing is wrong with the port composition of a panel nobody has broken.
    expect(list.obj.portsProblem).toBeNull();

    // A pending hop cannot be made active (invariant 1): the override may only
    // point at a box that has really entered the chain. The refusal travels as
    // success:false inside a 200, the way the whole panel API says no.
    const setActive = await authedRequest.post(`/panel/api/chain/setActive/${id}`);
    expect(setActive.status()).toBe(200);
    const refused = await setActive.json();
    expect(refused.success).toBe(false);
    expect(refused.msg).toContain('hop_not_joined');

    // An unknown hop is a refusal too, not a status code.
    const unknown = await (await authedRequest.post('/panel/api/chain/del/999999')).json();
    expect(unknown.success).toBe(false);
    expect(unknown.msg).toContain('unknown_hop');

    // The relayed ports the editor shows are the panel's own listeners.
    const ports = await (await authedRequest.get('/panel/api/chain/ports')).json();
    expect(ports.success).toBe(true);
    expect(Array.isArray(ports.obj)).toBe(true);

    const deleted = await (await authedRequest.post(`/panel/api/chain/del/${id}`)).json();
    expect(deleted.success).toBe(true);
  });

  test('the registry is invisible without a session', async ({ request }) => {
    const list = await request.get('/panel/api/chain/list');
    expect(list.status()).toBe(404);
    expect((await list.body()).length).toBe(0);
  });
});
