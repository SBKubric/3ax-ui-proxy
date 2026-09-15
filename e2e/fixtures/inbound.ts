import { expect, type APIRequestContext } from '@playwright/test';

/**
 * Creates one inbound through the panel's own API (`POST
 * /panel/api/inbounds/add`, web/controller/inbound.go) and returns its id.
 *
 * The inbound is created disabled. A disabled inbound never reaches xray —
 * `InboundService.AddInbound` only calls the xray API when `enable` is true —
 * so the harness does not need a working xray process to have a row in the
 * table. It is still a monitoring target: §3.5 counts disabled inbounds as
 * PAUSED rather than absent, so it gets its card on the Monitoring page and its
 * cell in the Health column, which is exactly what these specs are here for.
 *
 * Specs run against one shared panel (`fullyParallel`), so every caller passes
 * its own remark and port — the panel refuses a duplicate port and the tag is
 * derived from it.
 */
export async function createInbound(
  request: APIRequestContext,
  remark: string,
  port: number,
): Promise<number> {
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
      // No clients: AddInbound's "empty client ID" guard only runs per client,
      // so the smallest settings body the panel accepts has none.
      settings: JSON.stringify({ clients: [], decryption: 'none', fallbacks: [] }),
      streamSettings: JSON.stringify({
        network: 'tcp',
        security: 'none',
        tcpSettings: { header: { type: 'none' } },
      }),
      sniffing: JSON.stringify({ enabled: false, destOverride: [] }),
    },
  });
  expect(res.status()).toBe(200);

  const body = await res.json();
  expect(body.success, `inbound "${remark}" was not created: ${body.msg}`).toBe(true);
  return body.obj.id as number;
}
