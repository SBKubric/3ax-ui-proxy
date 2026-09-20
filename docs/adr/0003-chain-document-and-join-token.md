---
status: proposed
---

# Chain document and join token instead of the relay manifest pasted into the setup page

[ADR 0001](0001-relay-manifest-via-setup-page.md) settled how one proxy front learns which ports to relay: the real server exports a **relay manifest** — a whitelisted subset of the xray config, no keys — and the owner pastes it into a one-time setup page the box serves while no manifest exists. That works for a single box in front of the panel, and it stops as soon as there is a **chain** of them. A chain needs each hop to know its *next hop* rather than the real server, the port list to change whenever the panel's inbounds change (a manifest pasted once goes stale silently), a standby edge to be introduced without a second manual paste, and the panel to keep describing the topology without ever calling out to a disposable box. Manual manifest delivery cannot carry any of that: it is a one-shot, unversioned, unauthenticated file.

## Decision

The chain is described by a **chain registry** on the panel (hops, roles, order, next hop, active edge) and carried to the boxes by a versioned **chain document** that every hop polls from its next hop and passes outward **truncated**: a hop sees itself, everything outside it, and the relayed ports — never anything deeper. A box is introduced with a **join token** issued by the registry, given either in the box's **join page** (the setup page's replacement, same random single-use path token) or as `PROXY_JOIN_TOKEN` at install; the join request is forwarded hop-by-hop inward to the panel, which answers with a **per-hop secret** the box then uses as the bearer for its polls. Relayed ports are computed by the panel — xray inbound ports by the ADR 0001 skip rules, tunnel and MTProto ports from its own services, plus manual `chainExtraPorts` — and travel in the document with an explicit `network`. There is no manifest file, no `relayManifestPath`, no `PROXY_RELAY_MANIFEST` and no pasting of anything into a box.

Supersedes the delivery part of [ADR 0001](0001-relay-manifest-via-setup-page.md); its "no keys on the proxy front" principle and the whitelisted port subset stay.

## Considered options

- **Keep the manifest and add chain fields to it.** Rejected: the manifest is a sanitised xray config, and the chain needs things an xray config has no place for — next hop, subscription paths, per-hop secrets, an active-edge marker, a revision. Stretching it would have produced a second, parallel document format that still had to be delivered by hand.
- **Push the configuration from the panel to the boxes.** Rejected: the panel never calls out. Boxes are disposable, often behind blocked routes, and reachable only from inside the chain; an outbound push from the panel would put the real server's address in every box's inbound firewall rule and invert the one direction we rely on. Every hop polls its next hop instead, so traffic and configuration take the same path.
- **One shared chain secret for all hops.** Rejected: the whole point of the outer hops is that they get blocked and seized. A shared secret means one seized edge hands over the whole chain's credential and revocation means re-keying every box; a per-hop secret means a seized edge leaks its own bearer and its next hop's address, and revocation is deleting one row from the registry.
- **Sign the chain document instead of authenticating the poll.** Rejected for v1: signatures would let a hop verify a document that arrived over an untrusted path, but the path is already authenticated hop-by-hop by the hop secret, and the document carries `secretHash` for each hop it lists, so a hop can check its outer neighbour. Key distribution and rotation for signing keys buy nothing until we allow relays we do not install ourselves.

## Consequences

- **Existing proxy fronts are reinstalled, not migrated.** A v1 `proxy.json` loads with a warning per legacy key and drops the box into join mode; `update.sh` stops before replacing the binary on such a box so the current relay keeps running until the owner reinstalls it as a hop.
- The `relaymanifest` package is repurposed as the panel-side port-list builder (`chainports`): `Build` and the skip rules stay, the whitelist validation of pasted files and the `relayManifest` version marker are deleted.
- The setup page becomes the **join page**: same single-use token scheme and same "it disappears once spent" behaviour, but it asks for a join token and a next hop instead of a file, and `PROXY_JOIN_TOKEN` + `PROXY_NEXT_HOP` skip it entirely.
- `x-ui relay-manifest` is demoted to a debug export of the computed port list and renamed `x-ui chain ports`; `x-ui proxy-setup-url` becomes `x-ui chain join-url`. The old names are **removed outright, with no deprecated aliases**: both only ever made sense in the ADR 0001 install flow, and a box from that flow has to be re-installed as a hop anyway, so nothing that still works keeps calling them.
- Monitoring gains a compatible extension rather than a new contract: `/state` reports the chain and its active edge, probes render per edge, and the result `path` accepts `edge:<name>` next to the existing `proxy`.
- A stale port list stops being a silent failure: the document is versioned, a hop that cannot reach its next hop keeps relaying the last known revision and reports itself `stale`.
