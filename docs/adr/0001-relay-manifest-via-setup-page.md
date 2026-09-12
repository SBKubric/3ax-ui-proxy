---
status: accepted
---

# Relay manifest instead of the raw panel config, delivered through a setup page

The relay on the proxy front only needs the real server's public inbound ports, yet the first cut of proxy mode told the owner to copy the panel's whole `bin/config.json` — Reality private key, Shadowsocks passwords and all — onto a box that exists to be blocked and thrown away. We decided that the real server exports a **relay manifest** (a whitelisted subset of the xray config: `listen`, `port`, `protocol`, `tag`, the TPROXY markers, plus a `relayManifest` version marker) via `x-ui relay-manifest` and a button in the panel, and that `x-ui proxy` refuses any file that carries fields outside that whitelist. The manifest is delivered by pasting it into a one-time **setup page** the proxy front serves on its subscription port under a random single-use token whenever no manifest is present; a file path stays supported for scripted installs.

## Considered options

- **Strip secrets on the proxy box** (in `install.sh` or at `x-ui proxy` start). Rejected: the raw config would still transit the disposable box's disk, so "no keys on the proxy front" would hold only after the fact.
- **Warn-and-continue on a raw config**, or sanitise it in place. Rejected: the acceptance criterion is enforced nowhere, and an in-place rewrite is not a shred.
- **File-only delivery via scp**. Kept as the scripted path, but the interactive path is the setup page so the owner never needs SSH to two boxes at once and the installer no longer blocks on a file that is not there yet.
- **A dedicated port-list format**. Rejected in favour of the xray-config subset: the relay reads it unchanged and the skip rules (api tunnel, loopback, TPROXY) stay on the proxy side.

## Consequences

- The proxy input is renamed without aliases (`relayManifestPath`, `/etc/x-ui/relay-manifest.json`, `PROXY_RELAY_MANIFEST`); an old `proxy.json` stops loading after the update and must be re-pointed.
- A proxy front with no manifest still runs as a service, in bootstrap mode, serving only the setup page; the relay and subscription server start in the same process once a manifest is accepted.
- Replacing the manifest later (after the panel's inbound set changes) is a separate question; the setup page is only offered while no manifest exists.
