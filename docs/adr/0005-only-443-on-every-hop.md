---
status: accepted
---

# Every box of the chain shows one port, 443: the upstream nginx front, split by SNI, with a Reality target next to the active edge

Whoever learns the address of any box in the chain, including the real server, used to find several open ports: the panel UI on 2053, the sub server on 2096, one port per inbound. A panel that answers on its own port fingerprints the box as a 3x-ui VPN server, and a login page on the public internet invites brute force. While working the proxy-front map ([#2](https://github.com/SBKubric/sane-3x-ui/issues/2), ticket [#132](https://github.com/SBKubric/sane-3x-ui/issues/132)) we decided that **every box (real server, inner fronts, edge fronts) accepts TCP only on 443** (plus SSH and the AmneziaWG UDP port). The front is **the nginx subsystem inherited from upstream**, in its `only443` mode, extended from the panel to the proxy boxes. nginx reads the SNI and:

- **the Reality target's SNI of this box** goes on as a raw stream, to xray on the real server or to the next hop;
- **the panel's own domain** (with the certificate from the panel settings) and **requests by IP without SNI** (with a short-lived Let's Encrypt IP certificate issued through nginx's webroot on :80) are terminated by the HTTP side, which serves the sub paths, the panel API (`login` and `panel/api/…` only), `/chain/v1` and `/mon/v1` by path, behind `limit_req` and fail2ban;
- **an unknown SNI** on an edge goes as a raw stream to that edge's Reality target, so a prober sees the neighbour's site exactly as an unauthenticated Reality client does; on the real server it goes to the Reality fallback when the server has a Reality inbound, as it always did, and gets the decoy site otherwise; on inner fronts, which have no Reality to hide behind, it gets the decoy site ([#140](https://github.com/SBKubric/sane-3x-ui/issues/140)).

The panel UI listens on 127.0.0.1 only and is reached through an SSH tunnel. The client protocol is VLESS + XHTTP + Reality. Its **target is a site in the same network as the active edge's address**, found by the orchestrator per edge and kept in the chain registry. On a switch of the active edge, the panel rewrites target and `serverNames` of the Reality inbounds marked as following the chain. `serverNames` holds only the active edge's name, so clients refresh their subscription after a switch, and the bot and the chain editor say so.

## Considered options

- **Caddy instead of nginx.** Built-in ACME for IP addresses is its strength, but SNI passthrough needs the experimental `caddy-l4` plugin and rate limiting needs the unofficial `caddy-ratelimit`: a custom binary to build and ship. It also means rewriting the panel's nginx subsystem and conflicting with upstream on every merge ([ADR 0002](0002-additive-upstream-compatibility.md)). Rejected once the owner saw that upstream already implements the SNI split.
- **Traefik.** SNI passthrough, rate limiting and ACME are all built in and stable, so it is simpler than Caddy. It still means replacing the subsystem, and it uses tens of MB of RAM on small VPSes where nginx uses a few. Rejected for the same upstream reason.
- **Sub and API on a special path with SNI = the Reality target.** Impossible: nginx sees the path only after terminating TLS, and TLS for the neighbour's name needs the neighbour's key. Such connections either are Reality clients or are passed on to the neighbour. Sub and API are reachable by IP and by the panel's own domain instead.
- **`serverNames` = the names of all edges, so old links keep working after a switch.** Rejected by the owner in favour of a strict single name: one plausible name per edge address, at the cost of a subscription refresh.
- **One target for the whole chain, or one inbound per edge.** A single target would sit next to one edge's address and look out of place behind the others. An inbound per edge multiplies the client's links. Instead, target per edge, switched by the panel.

## Consequences

- The orchestrator enables `only443` by default. `install.sh` without the orchestrator keeps today's behaviour, and the mode is an option there.
- Port 80 belongs to nginx on every box (ACME webroot plus a decoy/redirect). This ends the old race between acme.sh standalone and `install_nginx`.
- mon-server reaches `/mon/v1` on 443 with a publicly trusted IP certificate, so `panelCa` is no longer needed for a panel behind the front.
- Picking a neighbour target scans the /24 around the edge's address from the machine that runs the orchestrator, never from a VPS. Candidates must pass TLS 1.3 + h2, no redirect, not a CDN, and a real Reality handshake. If none qualifies, a known-good shared target is used and the run warns.
- A switch of the active edge now changes client links (the SNI). It is a deliberate, visible step, not a silent host swap.
