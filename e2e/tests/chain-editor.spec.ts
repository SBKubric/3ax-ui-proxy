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
  // skipDrain, because a leftover row that is draining would keep the name
  // taken for the next test (§4.5.5).
  if (hop) {
    await request.post(`/panel/api/chain/del/${hop.id}`, { data: { force: true, skipDrain: true } });
  }
}

/** The panel's sub server, where boxes join (docker-compose.yml publishes it). */
const SUB_URL = process.env.E2E_SUB_URL || 'http://127.0.0.1:2096';

/** Creates a hop through the registry API and returns it with its join token. */
async function addHop(request: APIRequestContext, hop: object) {
  const added = await (await request.post('/panel/api/chain/add', { data: hop })).json();
  expect(added.success, added.msg).toBe(true);
  return { id: added.obj.hop.id as number, joinToken: added.obj.joinToken as string };
}

/**
 * A disabled VLESS + Reality inbound whose cover is www.original.example,
 * created through the API the inbound form posts to.
 */
async function createRealityInbound(request: APIRequestContext, remark: string, port: number, followChain: boolean) {
  const res = await request.post('/panel/api/inbounds/add', {
    data: {
      up: 0,
      down: 0,
      total: 0,
      remark,
      enable: false,
      expiryTime: 0,
      listen: '',
      port,
      protocol: 'vless',
      followChain,
      settings: JSON.stringify({ clients: [], decryption: 'none', fallbacks: [] }),
      streamSettings: JSON.stringify({
        network: 'tcp',
        security: 'reality',
        tcpSettings: { header: { type: 'none' } },
        realitySettings: {
          show: false,
          xver: 0,
          target: 'www.original.example:443',
          serverNames: ['www.original.example'],
          privateKey: 'yBaw532IIUNuQWDTncozoBaLJmcd1JZzvsHUgVPxMk8',
          shortIds: ['ab12'],
          settings: {
            publicKey: 'wdZxPhgkfTXCJ3Mn6WgLyCMJv0Z6Lm1kqvZnxj6Q6hk',
            fingerprint: 'chrome',
            serverName: '',
            spiderX: '/',
          },
        },
      }),
      sniffing: JSON.stringify({ enabled: false, destOverride: [] }),
    },
  });
  const body = await res.json();
  expect(body.success, `inbound "${remark}" was not created: ${body.msg}`).toBe(true);
  return body.obj.id as number;
}

async function getInbound(request: APIRequestContext, id: number) {
  const got = await (await request.get(`/panel/api/inbounds/get/${id}`)).json();
  expect(got.success, got.msg).toBe(true);
  return got.obj;
}

/** The target and serverNames an inbound's Reality settings carry. */
async function realityOf(request: APIRequestContext, id: number) {
  const reality = JSON.parse((await getInbound(request, id)).streamSettings).realitySettings;
  return { target: reality.target, serverNames: reality.serverNames };
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
    // With an empty registry the Proxy front row keeps the two legacy fields
    // editable (§2.3): a panel upgraded without a chain has no active edge for
    // the host to come from, and taking the old inputs away would leave it with
    // no override at all.
    await expect(authedPage.getByTestId('chain-override-legacy')).toBeVisible();
    await expect(authedPage.getByTestId('chain-override-enable')).toBeVisible();
    await expect(authedPage.getByTestId('chain-override-active')).toHaveCount(0);

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
    // A pending hop has no traffic through it and no monitoring data either:
    // a grey "no data", not a target's UNKNOWN (proxy-chain.md §6.4).
    await expect(authedPage.getByTestId('chain-hop-e2e-inner-health')).toHaveText('no data');
    // The token is shown whole exactly once: the panel keeps only its hash.
    const token = authedPage.getByTestId('chain-hop-e2e-inner-token');
    await expect(token).toBeVisible();
    const shown = (await token.textContent())?.trim() || '';
    expect(shown).toMatch(/[0-9a-zA-Z]{32}/);

    await authedPage.getByTestId('chain-hop-e2e-inner-token-copy').click();

    // Reloading the page loses it — the registry cannot hand it out again, so
    // the row falls back to the expiry and the Reissue button.
    await openSubscriptionTab(authedPage);
    // And now that a hop exists the registry owns the override: the legacy
    // inputs are gone and the row is read-only.
    await expect(authedPage.getByTestId('chain-override-active')).toBeVisible();
    await expect(authedPage.getByTestId('chain-override-enable')).toHaveCount(0);
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

    // del answers the envelope of §4.5.1. Nothing hangs off this pending edge
    // — no box ever entered in this harness — so the row goes at once:
    // state "deleted", no deadline and nobody to wait for. The draining half
    // of the same envelope needs two real boxes and lives on the stand (§10
    // step 8); what an API test can pin down is the shape.
    const deleted = await (await authedRequest.post(`/panel/api/chain/del/${id}`)).json();
    expect(deleted.success).toBe(true);
    expect(deleted.obj.state).toBe('deleted');
    expect(deleted.obj.hop).toBe('e2e-edge');
    expect(deleted.obj.drainUntil).toBe(0);
    expect(deleted.obj.safeToPowerOffWhen.hops).toEqual([]);
    expect(deleted.obj.safeToPowerOffWhen.revision).toBeGreaterThan(0);

    // And the registry says the same: the row is gone, and no departure is
    // under way for the editor to show.
    const after = await (await authedRequest.get('/panel/api/chain/list')).json();
    expect(after.obj.hops.some((h: { name: string }) => h.name === 'e2e-edge')).toBe(false);
    expect(after.obj.draining).toEqual([]);
  });

  test('del of a pending inner deletes it at once and skipDrain is accepted', async ({
    authedRequest,
  }) => {
    await removeHop(authedRequest, 'e2e-drain-inner');

    const added = await (
      await authedRequest.post('/panel/api/chain/add', {
        data: { name: 'e2e-drain-inner', host: 'inner.e2e.example', role: 'inner' },
      })
    ).json();
    expect(added.success).toBe(true);
    const id = added.obj.hop.id;

    // A hop that was created and never entered has no box of its own, so
    // there is nobody for it to hand over to (§4.5.1 step 3).
    const deleted = await (
      await authedRequest.post(`/panel/api/chain/del/${id}`, { data: { skipDrain: true } })
    ).json();
    expect(deleted.success).toBe(true);
    expect(deleted.obj.state).toBe('deleted');
    expect(deleted.obj.safeToPowerOffWhen.hops).toEqual([]);
  });

  test('clearActive answers success on an empty registry', async ({ authedRequest }) => {
    // No box in this harness ever really joins the chain, so there is no
    // active edge to switch off here — this asserts the shape of the route
    // the editor's "Turn override off" button calls: it is the same
    // DisableProxyOverride path the bot's "/proxy off" already exercises
    // (web/service/setting_chain_override_test.go), and it is not an error to
    // call it with nothing active, which is the only state this harness can
    // reach.
    const cleared = await (await authedRequest.post('/panel/api/chain/clearActive')).json();
    expect(cleared.success).toBe(true);

    const list = await (await authedRequest.get('/panel/api/chain/list')).json();
    expect(list.obj.activeEdge).toBe('');
  });

  test('an edge carries its neighbour target, and a switch with chain-following inbounds is confirmed first', async ({
    authedPage,
    authedRequest,
    request,
  }) => {
    // #139, ADR 0005. The neighbour target is what the orchestrator writes
    // through the registry API; the chain editor shows and edits it on the
    // edge card; Reality inbounds marked in the inbound form follow the active
    // edge's; and a switch that rewrites them asks first.
    for (const name of ['e2e-nb-a', 'e2e-nb-b']) await removeHop(authedRequest, name);
    const inboundIds: number[] = [];
    try {
      const a = await addHop(authedRequest, {
        name: 'e2e-nb-a',
        host: 'a.e2e.example',
        role: 'edge',
        realityTarget: 'www.neighbour-a.example:443',
      });
      const b = await addHop(authedRequest, { name: 'e2e-nb-b', host: 'b.e2e.example', role: 'edge' });
      // Only a hop whose box has entered can be made active: both enter the
      // way a real box does, with their join token on the panel's sub port.
      for (const token of [a.joinToken, b.joinToken]) {
        const joined = await request.post(`${SUB_URL}/chain/v1/join`, { data: { token } });
        expect(joined.status()).toBe(200);
      }

      // One Reality inbound follows the chain, one does not.
      inboundIds.push(await createRealityInbound(authedRequest, 'e2e-follower', 24211, true));
      inboundIds.push(await createRealityInbound(authedRequest, 'e2e-follower-later', 24212, false));

      await openSubscriptionTab(authedPage);
      await expect(authedPage.getByTestId('chain-hop-e2e-nb-a-neighbour')).toContainText(
        'www.neighbour-a.example:443 · SNI www.neighbour-a.example',
      );
      await expect(authedPage.getByTestId('chain-hop-e2e-nb-b-neighbour')).toHaveText('no neighbour target');

      // The edge card edits it: an address target with the name its site answers to.
      await authedPage.getByTestId('chain-hop-e2e-nb-b-neighbour-edit').click();
      const modal = authedPage.getByTestId('chain-neighbour-modal');
      await field(authedPage, 'chain-neighbour-target').fill('198.51.100.20:443');
      await field(authedPage, 'chain-neighbour-server-name').fill('www.neighbour-b.example');
      await authedPage.getByRole('button', { name: 'OK' }).click();
      await expect(authedPage.getByTestId('chain-hop-e2e-nb-b-neighbour')).toContainText(
        '198.51.100.20:443 · SNI www.neighbour-b.example',
      );
      await expect(modal).toBeHidden();

      // With an inbound following the chain, "Make active" says what the
      // switch does to the links clients hold — and cancelling changes nothing.
      await authedPage.getByTestId('chain-hop-e2e-nb-a-make-active').click();
      const confirm = authedPage.getByRole('dialog').filter({ hasText: 'e2e-nb-a' });
      await expect(confirm).toContainText('Clients must refresh their subscription');
      await expect(confirm).toContainText('the old links will stop working');
      await confirm.getByRole('button', { name: 'Cancel' }).click();
      await expect(authedPage.getByTestId('chain-hop-e2e-nb-a-active-marker')).toHaveCount(0);

      await authedPage.getByTestId('chain-hop-e2e-nb-a-make-active').click();
      await authedPage.getByRole('dialog').filter({ hasText: 'e2e-nb-a' }).getByRole('button', { name: 'Sure' }).click();
      await expect(authedPage.getByTestId('chain-hop-e2e-nb-a-active-marker')).toBeVisible();

      // The follower took edge-a's neighbour; the other inbound kept its own.
      expect(await realityOf(authedRequest, inboundIds[0])).toEqual({
        target: 'www.neighbour-a.example:443',
        serverNames: ['www.neighbour-a.example'],
      });
      expect((await realityOf(authedRequest, inboundIds[1])).target).toBe('www.original.example:443');

      // The inbound form marks the second one: saved while edge-a is active,
      // it takes edge-a's neighbour at once.
      await authedPage.goto('/panel/inbounds');
      await authedPage.getByTestId(`inbound-actions-${inboundIds[1]}`).click();
      await authedPage.getByRole('menuitem', { name: 'Edit' }).click();
      const follow = authedPage.getByTestId('inbound-follow-chain');
      // antd's switch sets aria-checked only while it is on.
      await expect(follow).not.toHaveAttribute('aria-checked', 'true');
      await follow.click();
      await expect(follow).toHaveAttribute('aria-checked', 'true');
      await authedPage.getByRole('button', { name: 'Update' }).click();
      await expect
        .poll(async () => (await getInbound(authedRequest, inboundIds[1])).followChain)
        .toBe(true);
      expect(await realityOf(authedRequest, inboundIds[1])).toEqual({
        target: 'www.neighbour-a.example:443',
        serverNames: ['www.neighbour-a.example'],
      });
    } finally {
      for (const id of inboundIds) await authedRequest.post(`/panel/api/inbounds/del/${id}`);
      await authedRequest.post('/panel/api/chain/clearActive');
      for (const name of ['e2e-nb-a', 'e2e-nb-b']) await removeHop(authedRequest, name);
    }
  });

  test('the registry is invisible without a session', async ({ request }) => {
    const list = await request.get('/panel/api/chain/list');
    expect(list.status()).toBe(404);
    expect((await list.body()).length).toBe(0);
  });
});
