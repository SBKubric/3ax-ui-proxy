---
status: accepted
---

# mon-server is the registry and the single source of monitoring truth; the panel is a passive receiver

Inbound healthchecks need boxes in the regions where users actually are (mon-clients), something that tells those boxes what to probe and turns their probes into UP/DOWN, and a place where the owner sees the picture and gets Telegram alerts. While charting the monitoring map ([#20](https://github.com/SBKubric/3ax-ui-proxy/issues/20)) we decided that **one mon-server on its own box owns everything that changes**: it keeps the registry of mon-clients (registration requests, tokens, paths), asks the panel for probe accounts and their configs, assembles each mon-client's targets, runs the target state machine and decides when a transition happened. The **panel never calls out**: it exposes a bearer-protected `/mon/v1/*` API ([contract](../spec/monitoring-contract.md)) that mon-server polls once a minute for state and pushes events and 5-minute aggregates into, and it renders whatever arrived. The panel keeps only a cache of the registry (the snapshot mon-server sends with every `POST /probe/ensure`) and derives exactly one state of its own, STALE, from the time of the last authorized request.

## Considered options

- **Registry and state machine in the panel, mon-clients report to it directly.** Rejected: mon-clients would need panel credentials or a second auth scheme on the real server, every mon-client in a hostile region would learn the real server's address even for the `proxy` path, and the panel (an upstream-tracked codebase, see [ADR 0002](0002-additive-upstream-compatibility.md)) would grow a scheduler, a registry and a probe-config distributor. mon-server is a fresh repo where all of that is native.
- **Panel pushes to mon-server (webhooks on inbound change, host override change).** Rejected: the panel would have to know mon-server's address and reachability and retry on failure; a one-minute poll of `GET /state` with a deterministic revision hash gives the same freshness with no outbound state in the panel and no NAT concerns.
- **mon-server pulls the probe subscription like a user client (`/sub/<subId>`).** Kept at charting time, replaced in the contract by `GET /probe/configs[?host=]` behind the same bearer token: it does not depend on the sub server being enabled or reachable, and the panel, not mon-server, substitutes the address per path.
- **Panel computes target state from raw probe results.** Rejected: the state machine needs the mon-client heartbeat (to tell OFFLINE from DOWN) and the unverified-cycle rule (mon-server is the probe target, so its own outage must not count as failures); both live naturally where the heartbeats arrive.

## Consequences

- Deleting a mon-client, revoking a token, changing its paths or the probe parameters happens in mon-server's admin UI only; the panel learns about it through the next registry snapshot and `PAUSED`/`UNKNOWN` transitions.
- The panel stores the registry snapshot as a UI cache, never as truth: rows of `mon_targets` whose mon-client left the snapshot are deleted, history stays until retention.
- Telegram alerts normally go through the panel's bot, so one bot and one admin list serve everything; when the panel is unreachable (`PANEL_DOWN` after 3 failed requests) mon-server sends them itself with the same bot token and marks the events `notified=true`, so the panel never double-sends after the backlog arrives.
- STALE is the only state the panel computes, because it is the only thing the panel can observe alone: silence.
- Supporting several mon-servers per panel, or one mon-server per several panels, stays out of v1 (map fog); the contract's registry snapshot and single `monToken` assume one of each.
