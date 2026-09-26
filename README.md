[English](/README.md) | [Русский](/README.ru_RU.md)

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./media/sane-3x-ui-dark.svg">
    <img alt="sane-3x-ui" src="./media/sane-3x-ui-light.svg" width="480">
  </picture>
</p>

[![Release](https://img.shields.io/github/v/release/SBKubric/sane-3x-ui.svg?include_prereleases)](https://github.com/SBKubric/sane-3x-ui/releases)
[![Build](https://img.shields.io/github/actions/workflow/status/SBKubric/sane-3x-ui/release.yml.svg)](https://github.com/SBKubric/sane-3x-ui/actions)
[![GO Version](https://img.shields.io/github/go-mod/go-version/SBKubric/sane-3x-ui.svg)](#)
[![License](https://img.shields.io/badge/license-GPL%20V3-blue.svg?longCache=true)](https://www.gnu.org/licenses/gpl-3.0.en.html)

**sane-3x-ui** is a 3x-ui panel built to stay up without constant attention. It builds on [3AX-UI](https://github.com/coinman-dev/3ax-ui), which adds AmneziaWG, native WireGuard, MTProto and an nginx front on a single port 443 to [3x-ui](https://github.com/MHSanaei/3x-ui). On top of that, it adds what you need to keep a server reachable when it gets blocked and to know when it isn't:

- **Proxy chain.** The real server hides behind a chain of disposable proxy fronts. Clients only see the outermost one. When it gets blocked, you replace it and the panel, inbounds and clients stay where they are. See [Proxy chain](#1-proxy-chain-anti-blocking).
- **Inbound health monitoring.** An external [mon-server](https://github.com/SBKubric/3ax-ui-monitoring) probes every inbound from outside through probe accounts, both directly and through the chain. The panel shows each inbound's health and sends DOWN/UP alerts to Telegram. See [Monitoring](#2-inbound-health-monitoring-mon-server).
- **Deploy from bare VPS.** One Ansible playbook in [sane-3x-ui-orchestrator](https://github.com/SBKubric/sane-3x-ui-orchestrator) installs the panel, joins the chain hops and adds mon-server and mon-client when you want monitoring. See [Deploy with Ansible](#3-deploy-with-ansible).
- **Tested on a real stand.** The chain and monitoring releases are run end to end on a five-VPS stand (panel, two hops, mon-server, mon-client), and the bugs found there are fixed here. See [Fixes from the stand](#4-fixes-from-the-stand).

Everything from 3AX-UI and 3x-ui keeps working: VLESS, VMess, Trojan, Shadowsocks, WireGuard, AmneziaWG, MTProto, subscriptions and the Telegram bot.

> [!IMPORTANT]
> This project is intended for personal use only. Please do not use it for illegal purposes.

## Alternative versions

sane-3x-ui is one of three related panels. If you need neither a chain nor monitoring, one of the others may suit you better:

| Panel | What it is | Choose it when |
|-------|------------|----------------|
| [3x-ui](https://github.com/MHSanaei/3x-ui) by MHSanaei | The original Xray panel: VLESS, VMess, Trojan, Shadowsocks, WireGuard; the largest community | Xray protocols are all you need |
| [3AX-UI](https://github.com/coinman-dev/3ax-ui) by coinman-dev | 3x-ui plus AmneziaWG (through 3.1), native WireGuard with IPv6, MTProto, nginx camouflage on port 443 | you want those protocols on one server, without a chain or monitoring |
| **sane-3x-ui** (this repo) | 3AX-UI plus the proxy chain, inbound health monitoring and Ansible deployment | your server gets blocked and you want to swap fronts instead of moving the panel, and see when an inbound goes down |

The features listed under [Inherited from 3AX-UI](#inherited-from-3ax-ui) come from 3AX-UI.

## Quick Start

```bash
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/install.sh) --beta
```

> The chain and monitoring ship in the `v1.9.0-chain.*` pre-releases, hence `--beta`. Without it the installer takes the latest stable release (`v1.8.1.x`), which has neither.

To add proxy fronts, see [Proxy chain](#1-proxy-chain-anti-blocking). To deploy the panel, hops and monitoring from scratch, see [Deploy with Ansible](#3-deploy-with-ansible).

---

## Added in sane-3x-ui

### 1. Proxy chain (anti-blocking)

When a server's IP or domain gets blocked you normally have to migrate the whole panel. **sane-3x-ui** can instead hide the real server behind a **chain** of cheap, disposable **proxy fronts**: clients only ever see the outermost one, so when it gets blocked you throw it away and spin up a new one — the real server (with all your inbounds, clients and traffic history) keeps running untouched and its address is never exposed.

A **chain** is a list of hops between the clients and the real server. Each **hop** relays to the next one and pulls subscriptions down the same path:

```
clients ──▶ edge front ──▶ inner front ──▶ … ──▶ real server
```

The hop clients see is the **edge front**; the ones only their neighbours know are **inner fronts**. A hop knows **only its own next hop** — never what lies beyond it. One panel has one chain; a chain of one hop is the ordinary case, and that hop's next hop is the panel itself.

**a) The chain registry (on the real panel).** The chain lives in **Panel Settings → Subscription → Chain**: each hop's name, role (`inner` / `edge`), host and order, plus which edge is the **active** one. The active edge is what the panel substitutes into every generated client config and subscription link; SNI / TLS / Reality identity is left untouched. From the Telegram bot:

```
/proxy                 list the hops, their roles and states
/proxy <name>          make that edge the active one
/proxy off             stop substituting; links point at the real server
```

The registry also holds `chainExtraPorts` — the ports the real server serves *outside* xray (AmneziaWG / WireGuard listeners, the MTProto sidecar), which the panel cannot read out of its xray config. Everything else the hops relay, the panel works out itself.

**Prerequisite: the panel's subscription server must be on** (`subEnable`). The chain's own routes (`/chain/v1/*`) live on it, so with it off no hop can join or receive updates; the panel logs a WARN at start if the registry has hops and the subscription server is off. If the panel has a `subDomain` set, its domain check runs before the chain routes — hops must then point at that domain (`PROXY_NEXT_HOP=<subDomain>`), not at a bare IP.

**b) Proxy run mode (`x-ui proxy`).** A disposable box runs the same binary as one hop and does two things:

- **Relays traffic** — an xray `dokodemo-door` L4 passthrough forwards every relayed port to its next hop (raw TCP+UDP, dual-stack). TLS/Reality terminate on the real server, so **no keys ever live on a hop**. Which ports to relay arrives in the **chain document** the hop polls from its next hop — a truncated excerpt of the registry that shows the hop itself, everything outward of it and the port list, and nothing deeper.
- **Serves subscriptions** — it fetches `/sub` and `/json` from its next hop and re-serves them: apps get the raw subscription, browsers get a custom page (traffic stats, QR, a **Copy VLESS JSON** button, and a curated app list).

**Joining a hop to the chain.** Always work inwards-out: the panel first, then the innermost hop, then outwards, edge last. Creating the hop in the registry does **not** bump the chain revision — a `pending` hop is not in the document yet, so there is nothing in it to change; the revision moves once, when the box actually joins.

1. On the panel, **Settings → Subscription → Chain** → add the hop (name, role, host). The panel shows a one-time **join token** (32 characters, valid 24 hours) — once, and never again; if it expires or is lost, press *reissue token*.
2. On the box, run the installer in proxy mode with that token:

```bash
XUI_PROXY_MODE=1 \
PROXY_NEXT_HOP=<next-hop-ip-or-domain> \
PROXY_NEXT_HOP_SUB_PORT=2096 \
PROXY_JOIN_TOKEN=<token from step 1> \
PROXY_DOMAIN=edge.example.com \
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/install.sh)
```

The installer writes `/etc/x-ui/proxy.json`, issues TLS, **joins the chain before it starts the service**, and the footer prints `x-ui chain status`. Without `PROXY_JOIN_TOKEN` the box comes up in *bootstrap mode* instead: it serves only a one-time **join page**, whose link the footer prints and `x-ui chain join-url` prints again; the token goes into that page, and the relay starts the moment the join is accepted.

3. When the hop is an edge and should face clients, make it the active one (step **a**).

**Install variables** (proxy mode):

| Variable | Default | Meaning |
|---|---|---|
| `XUI_PROXY_MODE` | — | `1` installs this host as a chain hop |
| `PROXY_NEXT_HOP` | — | **required** — the next hop's address: an inner front, or the real server for the innermost hop |
| `PROXY_NEXT_HOP_SUB_PORT` | `2096` | the next hop's subscription port (subscriptions, `/chain/v1/*`) |
| `PROXY_NEXT_HOP_SCHEME` | `https` | `http` or `https` for that port |
| `PROXY_JOIN_TOKEN` | — | the one-time token from the registry; without it the box serves a join page |
| `PROXY_TLS` | `letsencrypt-ip` | how this hop gets TLS for its own subscription port: `letsencrypt-ip`, `none` or `manual` |
| `PROXY_TLS_IPV6` | off | `1` runs the ACME client over IPv6 as well; by default it is pinned to IPv4, because a dual-stack connect to the CA from a box without working IPv6 costs the whole connect timeout and acme.sh gives up. Spelled `XUI_TLS_IPV6=1` outside proxy mode — it is the same switch, and it governs the panel's domain certificate too |
| `PROXY_DOMAIN` | request host | this box's public host, used in subscription links and the join-page URL |
| `PROXY_SUB_PORT` | `2096` | this hop's own subscription port |
| `PROXY_SUB_LISTEN` | all interfaces | bind address for it |
| `PROXY_RELAY_LISTEN` | `::` | bind address of the relay (`0.0.0.0` on hosts without IPv6) |
| `PROXY_CERT` / `PROXY_KEY` | — | TLS paths, only meaningful with `PROXY_TLS=manual` |
| `PROXY_FRONT` | `off` | `only443` puts nginx on 443 in front of this hop: TCP on 443 only, split by SNI, and a firewall around it (see below) |

With the default `PROXY_TLS=letsencrypt-ip` the installer issues a Let's Encrypt certificate **for the box's own IP address** — a fresh disposable front has no domain, and Let's Encrypt only issues IP certificates under the `shortlived` profile, so it is valid for about six days and renewed automatically. That needs port 80 free, both at issue time and at every renewal; if it is not, the installer warns and the box runs without TLS, serving its join page over plain HTTP with a warning banner. A box that relays port 80 through the chain cannot hold such a certificate — install it with `PROXY_TLS=manual` or `none`.

**Front 443 on a hop** (`PROXY_FRONT=only443`, or `"front": {"mode": "only443"}` in `/etc/x-ui/proxy.json`): nginx takes 443 and splits it by SNI. An edge passes its neighbour target's server name raw to its next hop and an unknown name raw to the neighbour target itself; an inner passes the active edge's server name on and gives anything else a decoy page; a request by IP, without SNI, reaches the hop's subscriptions and `/chain/v1` through the IP certificate. The relay stops holding 443/tcp and other TCP ports (UDP is relayed as before), and the hop closes everything but 443/tcp, 80/tcp, SSH and the relayed UDP ports (`"firewall": false` leaves the ports alone). The hop tells the panel on every poll, the panel moves its sub port to 443 and the old sub port closes once the hops polling it have moved; an edge closes it at once, and client subscription links become `https://<edge>/…`. It needs the Let's Encrypt IP certificate — without one the front stays off. Details: `docs/runbooks/proxy-front.md` §3.5.

**CLI** (the same binary in both roles):

```
x-ui chain ports      # panel: the relayed ports the chain document will carry
x-ui chain join-url   # box:   the pending join-page link
x-ui chain status     # box:   name, role, next hop, revision, relayed ports, last wave
x-ui chain rejoin --next-hop <host> [--sub-port 2096] [--scheme https] --token <t>
                      # box:   point this hop at a new next hop with a fresh token
```

Settings live in `/etc/x-ui/proxy.json` (0600) and the last accepted chain document in `/etc/x-ui/chain/`; the box runs `x-ui proxy` as the `x-ui` service, and `update.sh` auto-detects a hop and updates it in proxy mode, leaving the config, the chain state and the certificate alone. A box still carrying a pre-chain `proxy.json` stops the update **before** the binary is replaced and keeps relaying on its current version until it is reinstalled as a hop. To *reinstall* over an existing box non-interactively, answer the "already installed" prompt with `2` (`printf '2\n' | XUI_PROXY_MODE=1 … bash <(curl …)`); the default switches to the update script instead.

Removing a hop, repairing the chain after an inner front dies and the full stand walkthrough are in [docs/runbooks/proxy-front.md](docs/runbooks/proxy-front.md).

**Monitoring.** A mon-server probes every inbound twice: `direct` (straight to the real server) and `proxy` (through the active edge and the whole chain), so "the chain is broken" and "the server is down" look different. Separate lines per hop (`edge:<name>` / `inner:<name>`), to tell "the edge is blocked" from "an inner hop died", are specified but not built yet ([#87](https://github.com/SBKubric/sane-3x-ui/issues/87)). See [Monitoring](#2-inbound-health-monitoring-mon-server).

> **Note:** the real server sees every relayed connection coming from the neighbouring hop's IP, so per-client IP-limit and the IP log won't reflect real client IPs for relayed traffic.

### 2. Inbound health monitoring (mon-server)

**sane-3x-ui** can report each inbound's health to an external **mon-server** — a separate project, [SBKubric/3ax-ui-monitoring](https://github.com/SBKubric/3ax-ui-monitoring) — whose own mon-clients probe the inbounds from outside through generated **probe accounts**. Results come back over a small bearer-token API under `/mon/v1/*`; the panel is passive and stores nothing until a mon-server talks to it.

This gives the panel a **Monitoring** page (per-inbound state, event feed, latency/availability sparklines), a **Health** column and a `down` filter in the Inbounds table, Telegram alerts on DOWN/UP plus a block in the daily digest, and a **STALE** flag once the mon-server goes silent past the configured threshold (15 minutes by default). Probe accounts carry a `probe-` prefix, show a `probe` badge, and are excluded from online counts, Telegram stats and LDAP sync.

Get the token in **Panel Settings → Monitoring** (enable switch, token with Copy / Regenerate, stale threshold, retention, probe-set line with Remove), or from the CLI:

```
x-ui setting -showMonToken
x-ui setting -resetMonToken
x-ui setting -monEnable true
```

— also menu item **27** in `x-ui`, or the `x-ui mon-token` shortcut. Regenerating invalidates the old token at once, so update the mon-server config right away.

See the repo above, the [monitoring panel spec](docs/spec/monitoring-panel.md) and the [wire contract](docs/spec/monitoring-contract.md).

> **Note:** monitoring is off by default — until enabled with a token issued, `/mon/v1` answers a bare 404.

### 3. Deploy with Ansible

[SBKubric/sane-3x-ui-orchestrator](https://github.com/SBKubric/sane-3x-ui-orchestrator) deploys a whole installation from an inventory: the panel, its chain hops and, optionally, [mon-server and mon-client](https://github.com/SBKubric/3ax-ui-monitoring). The profile is the inventory: `stand-chain` gives panel + chain, `stand-full` gives panel + chain + monitoring.

```sh
ansible-playbook -i inventories/stand-full site.yml --ask-vault-pass     # install or converge
ansible-playbook -i inventories/stand-full verify.yml --ask-vault-pass   # checks only
ansible-playbook -i inventories/stand-full wipe.yml -e wipe_confirm=yes --ask-vault-pass   # start from scratch
```

- `site.yml` installs pinned release tags, sets the panel credentials from ansible-vault, creates the inbounds listed in the inventory, and joins hops inside-out. The hop list in the inventory is the source of truth for the chain registry (add, reissue, set active, delete).
- It also configures mon-server through its admin API, approves mon-client automatically by pairing code, and sets up Telegram alerts.
- `verify.yml` finishes the run: panel reachable, hops joined, and every monitoring target UP.
- Target OS: Debian 12/13 or Ubuntu 22.04/24.04.

The details are in the orchestrator's README, including the runbook for building a stand from scratch.

### 4. Fixes from the stand

Running the chain and monitoring end-to-end on a real stand turned up bugs the unit tests never hit. Some of the fixes:

- **AWG MTU and AmneziaWG 2.0 padding.** The default MTU is now `1420 − S4`, so a full-size packet plus the transport padding still fits a 1500-byte link. Previously TLS through AWG stalled after the handshake.
- **Chain ports from the inbounds table.** The ports a hop relays come from the panel's inbounds, not from `bin/config.json`, so a newly added inbound reaches every hop.
- **Installer fixes.** ACME runs over IPv4 by default. A version tag passed without a TTY is no longer dropped. `--beta` no longer leaves a box without the service. An existing install is handed to this fork's `update.sh`, not upstream's.
- **Subscriptions.** VLESS users get `"encryption":"none"` in the JSON subscription. The profile page URL carries the proxy's own address and port.
- **UDP through WireGuard outbounds (WARP, NordVPN).** New WireGuard outbounds start with `noKernelTun: true`. xray runs as root under the panel and then picks a kernel TUN, and through a kernel TUN UDP fails (`use of WriteTo with pre-connected connection`) while TCP works. Outbounds created earlier are left as they are: switch **No Kernel Tun** on in the outbound's settings if UDP through it does not work.

---

## Inherited from 3AX-UI

The rest of the feature set comes from [3AX-UI](https://github.com/coinman-dev/3ax-ui) unchanged.

### Why 3AX-UI?

The original 3x-ui is built around the **Xray** core and supports VLESS, VMess, Trojan, Shadowsocks, and WireGuard. But the most useful DPI-circumvention tools today are missing from the original:

- **AmneziaWG** — a modified WireGuard with traffic obfuscation (every generation through 3.1);
- **native WireGuard** that hands clients a real public IPv6 address without NAT66;
- **MTProto** — a FakeTLS proxy for Telegram.

**3AX-UI** integrates all three directly into the panel: they are created and managed exactly like any other protocol through the familiar **Inbounds** page.

And then it hides them. An nginx front-end puts every protocol that announces a server name behind port 443, and answers anyone else there with an ordinary website — one open port instead of four.


### 1. Full AmneziaWG support (1.x, 2.0, 3.0 and 3.1)

AmneziaWG is WireGuard with added packet obfuscation. Standard WireGuard is easily detected and blocked by DPI systems (Russia, Iran, China). AmneziaWG makes traffic indistinguishable from random noise.

**What's added:**
- Dedicated AWG server settings page (network parameters, IPv4/IPv6 address pool, obfuscation parameters)
- AWG client management directly from the **Inbounds** page — just like VLESS or Trojan
- Per-client: automatic key generation (private, public, preshared), IP allocation from pool, QR code, `.conf` file download
- Traffic statistics collected every 10 seconds (upload/download per client)
- Traffic limits, expiry dates, auto-renew, IP limit — same as all other protocols

**AmneziaWG 2.0.** The panel supports the new 2.0-generation obfuscation set on top of classic 1.x:
- extra parameters **S3 / S4** (padding for cookie/transport packets) and **I1** (a CPS signature packet before the handshake);
- **H1–H4** now accept not just a single value but a range (`100000-800000`);
- a **Generate** button fills the form with a random valid 2.0 set (*default* and *mobile* presets);
- a **push configs** button sends every Telegram-linked client its config via the bot — so after switching to 2.0 they can re-import their profile in one tap;
- DNS is pushed to clients split by family (IPv4 / IPv6).

A fresh install configures the server in 2.0 mode right away; empty S3/S4/I1 keep classic 1.x output — backward compatibility is preserved.

**AmneziaWG 3.0 / 3.1.** The 3.0 generation answers the blocking wave of mid-2026, where masking individual packet traits stopped being enough: it encrypts the packet header and randomises the protocol timers, so a session keeps no stable profile to recognise. The panel supports the complete set:

- **HeaderProtectionKey** — ChaCha20 encryption of the packet header. The one 3.0 parameter that must match on both ends, generated together with the rest;
- **ContentPaddingAddition** — extra padding inside the encrypted part;
- **RekeyAfterTime**, **RekeyTimeout**, **RejectAfterTime**, **KeepaliveTimeout**, **MaxHandshakeAttempts** — protocol timers, each taking a range (`100-130`) from which the kernel draws a fresh value per session;
- **RandomTrailers** and **DisableCookies** — the two parameters 3.1 added;
- **I2–I5** — the remaining 2.0 signature packets, alongside I1.

The generation is picked from a dropdown next to the **Generate** button: 2.0 or 3.x. Choosing 3.x also produces the 2.0 set, because header protection needs S1–S4 wide enough to carry its nonce and the panel keeps the two halves consistent. The 3.x option is disabled, with the installed version shown next to it, when the server's `amneziawg-tools` or kernel module predate 3.0 — writing those keys there would produce a config that refuses to load. Every field is optional and an unset one is not written at all, so a 1.x or 2.0 server keeps producing exactly the config it produced before.

> Clients must support 3.0 too: after switching, everyone re-imports their config, and an older AmneziaVPN app will not read it.

### 2. Nginx camouflage — everything behind port 443

**The problem it solves.** A panel on one port, Reality on 443, subscriptions on a third, MTProto on a fourth — that is a shape. A scanner finds several ports speaking TLS to nobody in particular, and a browser opening the domain finds nothing at all. Meanwhile plenty of networks let 443 through and very little else.

**What it does.** nginx takes port 443 and reads the server name straight out of the TLS handshake without decrypting anything (`stream` + `ssl_preread`), then hands the connection to whichever inbound announced that name. Reality's TLS stays end to end — the panel never sits in the middle of it. Anyone who arrives without a name the server recognises is given an ordinary website.

**Three modes** on the **Nginx camouflage** page:

| Mode | What happens |
| --- | --- |
| **Off** | Every inbound keeps its own port. Nothing on the page is applied. |
| **Dual mode** | The inbound that was on 443 moves to the loopback. Every other inbound keeps its own port **and** answers on 443 as well, so links already handed out go on working — nothing has to be re-issued. |
| **Port 443 only** | Nothing is left on a port of its own, and the ports nobody needs any more are closed. |

**A cover page, not an empty port.** The domain is served a real website: one of three built-in templates or your own single-file HTML, chosen from a gallery of live previews, edited in the panel and stored in the database — so it rides along in the backup and survives a reinstall. Only the active one is written to disk for nginx to serve.

**The panel and the subscriptions can live there too.** Both can be published on the same domain over 443, each at its own path — two fewer ports to explain. The subscription server keeps its own port as well, so links already handed out do not break; new ones carry the new address.

**Closing ports is a lease, not a leap.** SSH stays open, on whatever port sshd's own configuration says. So do the UDP tunnels, DHCP, anything arriving through a tunnel interface, the replies to whatever the server itself asked for, and any extra ports you name. Then you have two minutes to confirm from the panel that it is still reachable — if nobody does, it all comes back on its own. The deadline is stored in the database, so restarting the panel does not lose it.

**Nothing moves until you have read what moves.** Applying shows a plan first: which inbound goes where, which links change port, whether the subscription address changes, what stays open, and what would be cut off because it announces no server name at all.

**AmneziaWG and WireGuard are UDP** and cannot share a TCP port, so they keep their own ports in every mode.

Needs nginx with the `stream` module — the panel checks and says so — and a certificate for the domain.

### 3. MTProto — Telegram proxy (FakeTLS)

The new **MTProto** protocol is a FakeTLS proxy for Telegram, run as a standalone **mtg / mtg-multi** process (not Xray) and managed from the **Inbounds** page like any other protocol.

**How it works:**
- Pick a public port and a **FakeTLS fronting domain** — the connection is disguised as TLS 1.3 to that domain (e.g. `www.cloudflare.com`); a **↻** button next to the field fills in a random domain from a curated list.
- The FakeTLS secret is **generated automatically** and shared as a **`tg://proxy` deep link** + QR — clicking it opens the Telegram app and prompts "Enable proxy?" directly.
- The details window shows the protocol's real security: **FakeTLS**, **MTProto 2.0 (AES-256-IGE)** encryption, the cover domain, and the **mtg/mtg-multi sidecar version**.

**Many users per port (multi-user).** On `amd64` / `arm64` the panel uses the **[mtg-multi](https://github.com/dolonet/mtg-multi)** fork, which serves many clients on a single port — handy behind NAT where you don't want one port-forward per user:
- each client has a **unique UID** and a **free-form name** (several clients may share a name — like AmneziaWG);
- its own FakeTLS secret, link/QR, traffic, quota, expiry, and online status;
- on other architectures the panel transparently falls back to single-secret **mtg** (one client per port). The right binary is fetched by `install.sh` / `update.sh` automatically.

**Anti-block egress.** An optional **Route through Xray** toggle: instead of dialing Telegram directly, mtg goes through a loopback SOCKS bridge that the panel injects into the running Xray config, routed to an **outbound of your choice** (e.g. a chain to a server where Telegram is reachable). Useful when Telegram is blocked on the panel host itself.

Existing MTProto inbounds are **migrated automatically** on the first start after an update — secrets, settings, and recorded traffic are preserved, and old links keep working.

### 4. Native WireGuard with native IPv6

A separate **native WireGuard** protocol (no obfuscation) for when you want clean, maximum-speed WireGuard rather than AmneziaWG. Managed the same way from the **Inbounds** page: multi-client, automatic key generation, QR, `.conf`, statistics, limits, and expiry per peer.

Clients can likewise be given a **native public IPv6** address from the server without NAT66 (via NDP proxy) and have ports forwarded (see sections 5 and 6). Compatible with standard WireGuard clients.

### 5. AmneziaWG obfuscation parameters

The AWG settings page lets you configure packet obfuscation parameters:

| Parameter | Description |
|-----------|-------------|
| `Jc` | Number of junk packets before handshake |
| `Jmin` / `Jmax` | Minimum and maximum size of junk packets |
| `S1` / `S2` | Size of init/response headers |
| `S3` / `S4` | (2.0) padding for cookie and transport packets |
| `H1` – `H4` | Magic headers; in 2.0 they accept a range of values |
| `I1` – `I5` | (2.0) signature packets sent before the handshake |
| `HeaderProtectionKey` | (3.0) ChaCha20 encryption of the packet header — must match on both ends |
| `ContentPaddingAddition` | (3.0) extra padding inside the encrypted part |
| `RekeyAfterTime` / `RekeyTimeout` | (3.0) when a rekey starts, and the pause between handshake attempts |
| `RejectAfterTime` / `KeepaliveTimeout` | (3.0) when a session expires, and the pause before a keepalive |
| `MaxHandshakeAttempts` | (3.0) handshake attempts before giving up |
| `RandomTrailers` | (3.1) random tail appended to packets |
| `DisableCookies` | (3.1) turn off cookie replies |

These parameters are automatically written into each client's config — no manual configuration needed.

**MTU and S4.** `S4` pads every data packet, so it comes out of the MTU. The panel's default is `1420 − S4` — a full-size packet then fits a 1500-byte link even when the client reaches the server over IPv6 (IPv6 40 + UDP 8 + header 16 + auth tag 16 + S4). No other parameter grows data packets: S1–S3 pad handshake and cookie messages, H1–H4 only change a header value, junk and I-packets are separate datagrams, and the 3.x ContentPaddingAddition/RandomTrailers never pad past the largest packet already sent. The same MTU goes into every client config, probe peers included.

- A server still on the old default 1420 (or with no MTU) while S4 is set is lowered to `1420 − S4` when the panel starts, and the running interface picks it up without a restart. Clients need to re-import their config for their own direction; the server side is fixed at once.
- While the MTU is the default, it follows S4 when you **Generate** a new set.
- An MTU of your own is kept, but one above `1440 − S4` (the most a full-size packet can carry over IPv4) is refused on save: every full-size packet would be lost.

### 6. Native IPv6 support without NAT

AWG / native WireGuard clients can be assigned a **native public IPv6 address** from the server — without NAT66. This works via NDP proxy (ndppd or a built-in fallback using `ip -6 neigh add proxy`). Clients receive a real IPv6 address, which matters for services that require it.

#### If IPv6 doesn't work: provider-side limitations

NDP proxy may not work on a VPS for reasons outside your server's control:

**1. Hypervisor blocks NDP packets (MAC filtering)**

Many providers allow a VPS to send packets only from its own network interface MAC address. When `ndppd` forwards a Neighbor Advertisement on behalf of a client, the hypervisor treats this as IP spoofing and drops the packet. Everything looks correct inside the VPS, but client IPv6 traffic never reaches the internet.

**2. Provider assigns a "link prefix" instead of a "routed prefix"**

NDP proxy only works when the IPv6 block is **routed directly to your VPS**. Many providers connect multiple VPSes to a shared virtual network and assign addresses from a common pool — in this case, NDP proxy at the VPS level won't help.

#### What to do

Contact your provider's support. You need to find out:
- **IPv6 allocation type:** is it a fully routed /64 prefix (routed to your VM) or an address from a shared pool (link prefix)? Only a routed prefix allows NDP proxy to work.
- **Hypervisor-level NDP proxy:** does the control panel have an option to enable NDP proxy / Neighbor Discovery at the host level?
- **IP spoofing allowance:** ask them to allow NDP packet forwarding from your VPS (disable MAC filtering for your interface at the hypervisor level).

> **Message template for provider support:**
> *"I'm running a server with multiple virtual network interfaces and need to assign individual public IPv6 addresses from my /64 block to each of them using NDP proxy. Could you please confirm whether my IPv6 allocation is a fully routed /64 prefix routed to my VM directly, and whether NDP Neighbor Advertisement packets originated from my VM are allowed through the hypervisor — or if they are dropped by MAC/ARP filtering on the host node?"*

### 7. Per-client port forwarding for AmneziaWG / native WireGuard

Each peer can forward arbitrary external ports straight to its tunnel IP for both **TCP and UDP** simultaneously — designed for game servers, P2P, voice apps, anything that needs an inbound port.

**Input format** (free-form, validated):
- single ports: `80, 443, 22`
- ranges with a dash: `8000-8100`
- mix freely, separated by `,` or `;`: `80, 443; 27015-27030`

**How it works.** For each enabled client with non-empty forwarded ports the panel emits `iptables` DNAT + FORWARD rules (TCP and UDP) into wg-quick's `PostUp`/`PostDown`. Updates apply **live** via `iptables -A`/`-D` without restarting the tunnel — peer sessions are not interrupted. Each rule carries a unique `3ax-fwd-<uuid>` comment so removing one client's forwards never touches another's.

The forwarded ports are visible in three places:
- the client edit form (with format hint),
- a dedicated "Mapping" column in the inbound's peer table,
- a row in the details modal directly under "Port".

### 8. SOCKS5 and HTTP proxies with full per-user infrastructure

xray-core's `mixed` (SOCKS5) and `http` inbounds now share the **same VLESS-style stack** as VLESS / VMess / Trojan / Shadowsocks:
- expandable peer table with per-client traffic, expiry, quota, IP limit, enable toggle;
- standard rich client edit modal (auto-generated 6-character username + 16-character password, regenerable);
- per-user traffic stats flow through xray's standard `user>>>EMAIL>>>traffic>>>...` keys, so the existing traffic and disable-on-quota / disable-on-expiry jobs handle MIXED/HTTP automatically;
- "Add Client" entry in the inbound action menu, just like VLESS.

The username remains editable after creation — renaming a client doesn't reset its traffic counters because the backend renames the underlying `client_traffic` row in place.

### 9. Automatic protocol installation

The install script (`install.sh`) automatically:
- Installs the AmneziaWG kernel module via PPA `ppa:amnezia/ppa`, plus `awg-tools` and `ndppd`
- Detects the server's external interface and configures PostUp/PostDown rules
- Fetches the MTProto sidecar binary (mtg-multi on amd64/arm64, otherwise mtg) from the official releases
- Installs nginx together with its `stream` module — a separate package on Debian and its derivatives, and without it port 443 cannot be split by server name
- Sets up AWG autostart after server reboot
- Detects Secure Boot and warns about potential DKMS module issues

### 10. Install / update from a local git clone

Both `install.sh` and `update.sh` detect when they are being run from inside a cloned repository (file presence + a BASH_SOURCE safety check) and **build the panel binary on the spot from the local source** instead of downloading the pre-built release tarball.

```bash
git clone https://github.com/SBKubric/sane-3x-ui.git
cd sane-3x-ui
sudo bash install.sh
```

If Go ≥ 1.21 isn't on the host, the script downloads Go 1.26.2 from go.dev automatically. With Go ≥ 1.21 the build self-bootstraps the toolchain pinned in `go.mod`. The remote-pipe flows (`bash <(curl ...)`, `curl ... | bash`) keep the existing GitHub-release behavior — the safety check rejects them so a user happening to be inside a clone of the repo while piping the script can't accidentally hit the local-build path.

`x-ui.db` and `bin/` survive across re-installs and updates, so re-running the installer does not wipe the panel database.

### 11. Debug / diagnostic install mode

A first prompt at install time:

```
Install panel in debug / diagnostic mode (localhost only)? [y/N]
(HTTP only, listen=127.0.0.1, default port 8080, no SSL or IPv6)
```

On `y` the panel binds to `127.0.0.1`, runs over plain HTTP on the chosen port, and skips the SSL prompt, the public-IP detection, and IPv6 work. Activate non-interactively with `XUI_DEBUG_MODE=1` (and optional `XUI_DEBUG_PORT=NNNN`).

`update.sh` **doesn't ask** the question — it auto-detects whether the existing install is in debug mode (`listenIP == 127.0.0.1` and no SSL cert configured) and inherits the same setup with the existing port, so updates are non-interactive on a debug box.

Protocol stacks (AmneziaWG, native WireGuard, MTProto, xray) install normally in debug mode — only the panel's web access is restricted to the loopback.


### 12. Proxy chain (anti-blocking)

When a server's IP or domain gets blocked you normally have to migrate the whole panel. **3AX-UI** can instead hide the real server behind a **chain** of cheap, disposable **proxy fronts**: clients only ever see the outermost one, so when it gets blocked you throw it away and spin up a new one — the real server (with all your inbounds, clients and traffic history) keeps running untouched and its address is never exposed.

A **chain** is a list of hops between the clients and the real server. Each **hop** relays to the next one and pulls subscriptions down the same path:

```
clients ──▶ edge front ──▶ inner front ──▶ … ──▶ real server
```

The hop clients see is the **edge front**; the ones only their neighbours know are **inner fronts**. A hop knows **only its own next hop** — never what lies beyond it. One panel has one chain; a chain of one hop is the ordinary case, and that hop's next hop is the panel itself.

**a) The chain registry (on the real panel).** The chain lives in **Panel Settings → Subscription → Chain**: each hop's name, role (`inner` / `edge`), host and order, plus which edge is the **active** one. The active edge is what the panel substitutes into every generated client config and subscription link; SNI / TLS / Reality identity is left untouched. From the Telegram bot:

```
/proxy                 list the hops, their roles and states
/proxy <name>          make that edge the active one
/proxy off             stop substituting; links point at the real server
```

The registry also holds `chainExtraPorts` — the ports the real server serves *outside* xray (AmneziaWG / WireGuard listeners, the MTProto sidecar), which the panel cannot read out of its xray config. Everything else the hops relay, the panel works out itself.

**Prerequisite: the panel's subscription server must be on** (`subEnable`). The chain's own routes (`/chain/v1/*`) live on it, so with it off no hop can join or receive updates; the panel logs a WARN at start if the registry has hops and the subscription server is off. If the panel has a `subDomain` set, its domain check runs before the chain routes — hops must then point at that domain (`PROXY_NEXT_HOP=<subDomain>`), not at a bare IP.

**b) Proxy run mode (`x-ui proxy`).** A disposable box runs the same binary as one hop and does two things:

- **Relays traffic** — an xray `dokodemo-door` L4 passthrough forwards every relayed port to its next hop (raw TCP+UDP, dual-stack). TLS/Reality terminate on the real server, so **no keys ever live on a hop**. Which ports to relay arrives in the **chain document** the hop polls from its next hop — a truncated excerpt of the registry that shows the hop itself, everything outward of it and the port list, and nothing deeper.
- **Serves subscriptions** — it fetches `/sub` and `/json` from its next hop and re-serves them: apps get the raw subscription, browsers get a custom page (traffic stats, QR, a **Copy VLESS JSON** button, and a curated app list).

**Joining a hop to the chain.** Always work inwards-out: the panel first, then the innermost hop, then outwards, edge last. Creating the hop in the registry does **not** bump the chain revision — a `pending` hop is not in the document yet, so there is nothing in it to change; the revision moves once, when the box actually joins.

1. On the panel, **Settings → Subscription → Chain** → add the hop (name, role, host). The panel shows a one-time **join token** (32 characters, valid 24 hours) — once, and never again; if it expires or is lost, press *reissue token*.
2. On the box, run the installer in proxy mode with that token:

```bash
XUI_PROXY_MODE=1 \
PROXY_NEXT_HOP=<next-hop-ip-or-domain> \
PROXY_NEXT_HOP_SUB_PORT=2096 \
PROXY_JOIN_TOKEN=<token from step 1> \
PROXY_DOMAIN=edge.example.com \
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)
```

The installer writes `/etc/x-ui/proxy.json`, issues TLS, **joins the chain before it starts the service**, and the footer prints `x-ui chain status`. Without `PROXY_JOIN_TOKEN` the box comes up in *bootstrap mode* instead: it serves only a one-time **join page**, whose link the footer prints and `x-ui chain join-url` prints again; the token goes into that page, and the relay starts the moment the join is accepted.

3. When the hop is an edge and should face clients, make it the active one (step **a**).

**Install variables** (proxy mode):

| Variable | Default | Meaning |
|---|---|---|
| `XUI_PROXY_MODE` | — | `1` installs this host as a chain hop |
| `PROXY_NEXT_HOP` | — | **required** — the next hop's address: an inner front, or the real server for the innermost hop |
| `PROXY_NEXT_HOP_SUB_PORT` | `2096` | the next hop's subscription port (subscriptions, `/chain/v1/*`) |
| `PROXY_NEXT_HOP_SCHEME` | `https` | `http` or `https` for that port |
| `PROXY_JOIN_TOKEN` | — | the one-time token from the registry; without it the box serves a join page |
| `PROXY_TLS` | `letsencrypt-ip` | how this hop gets TLS for its own subscription port: `letsencrypt-ip`, `none` or `manual` |
| `PROXY_TLS_IPV6` | off | `1` runs the ACME client over IPv6 as well; by default it is pinned to IPv4, because a dual-stack connect to the CA from a box without working IPv6 costs the whole connect timeout and acme.sh gives up. Spelled `XUI_TLS_IPV6=1` outside proxy mode — it is the same switch, and it governs the panel's domain certificate too |
| `PROXY_DOMAIN` | request host | this box's public host, used in subscription links and the join-page URL |
| `PROXY_SUB_PORT` | `2096` | this hop's own subscription port |
| `PROXY_SUB_LISTEN` | all interfaces | bind address for it |
| `PROXY_RELAY_LISTEN` | `::` | bind address of the relay (`0.0.0.0` on hosts without IPv6) |
| `PROXY_CERT` / `PROXY_KEY` | — | TLS paths, only meaningful with `PROXY_TLS=manual`; there they are required — absolute paths to readable PEM files, or the install stops before changing anything |
| `PROXY_FRONT` | `off` | `only443` puts nginx on 443 in front of this hop: TCP on 443 only, split by SNI, and a firewall around it (see below) |

With the default `PROXY_TLS=letsencrypt-ip` the installer issues a Let's Encrypt certificate **for the box's own IP address** — a fresh disposable front has no domain, and Let's Encrypt only issues IP certificates under the `shortlived` profile, so it is valid for about six days and renewed automatically. That needs port 80 free, both at issue time and at every renewal; if it is not, the installer warns and the box runs without TLS, serving its join page over plain HTTP with a warning banner. A box that relays port 80 through the chain cannot hold such a certificate — install it with `PROXY_TLS=manual` or `none`. A reinstall keeps the IP certificate already on the box (in `/root/cert/ip`, valid for more than a day and renewed by acme.sh) instead of issuing a new one.

**Front 443 on a hop** (`PROXY_FRONT=only443`, or `"front": {"mode": "only443"}` in `/etc/x-ui/proxy.json`): nginx takes 443 and splits it by SNI. An edge passes its neighbour target's server name raw to its next hop and an unknown name raw to the neighbour target itself; an inner passes the active edge's server name on and gives anything else a decoy page; a request by IP, without SNI, reaches the hop's subscriptions and `/chain/v1` through the IP certificate. The relay stops holding 443/tcp and other TCP ports (UDP is relayed as before), and the hop closes everything but 443/tcp, 80/tcp, SSH and the relayed UDP ports (`"firewall": false` leaves the ports alone). The hop tells the panel on every poll, the panel moves its sub port to 443 and the old sub port closes once the hops polling it have moved; an edge closes it at once, and client subscription links become `https://<edge>/…`. It needs the Let's Encrypt IP certificate — without one the front stays off. Details: `docs/runbooks/proxy-front.md` §3.5.

**CLI** (the same binary in both roles):

```
x-ui chain ports      # panel: the relayed ports the chain document will carry
x-ui chain join-url   # box:   the pending join-page link
x-ui chain status     # box:   name, role, next hop, revision, relayed ports, last wave
x-ui chain rejoin --next-hop <host> [--sub-port 2096] [--scheme https] --token <t>
                      # box:   point this hop at a new next hop with a fresh token
```

Settings live in `/etc/x-ui/proxy.json` (0600) and the last accepted chain document in `/etc/x-ui/chain/`; the box runs `x-ui proxy` as the `x-ui` service, and `update.sh` auto-detects a hop and updates it in proxy mode, leaving the config, the chain state and the certificate alone. A box still carrying a pre-chain `proxy.json` stops the update **before** the binary is replaced and keeps relaying on its current version until it is reinstalled as a hop. To *reinstall* over an existing box non-interactively, answer the "already installed" prompt with `2` (`printf '2\n' | XUI_PROXY_MODE=1 … bash <(curl …)`); the default switches to the update script instead.

Removing a hop, repairing the chain after an inner front dies and the full stand walkthrough are in [docs/runbooks/proxy-front.md](docs/runbooks/proxy-front.md).

**Monitoring.** A mon-server probes each hop separately, so a chain shows up as `direct` plus one line per hop — which is how you tell "the edge is blocked" from "the inner one died". See section 12.

> **Note:** the real server sees every relayed connection coming from the neighbouring hop's IP, so per-client IP-limit and the IP log won't reflect real client IPs for relayed traffic.

### 12. Inbound health monitoring (mon-server)

**3AX-UI** can report each inbound's health to an external **mon-server** — a separate project, [SBKubric/3ax-ui-monitoring](https://github.com/SBKubric/3ax-ui-monitoring) — whose own mon-clients probe the inbounds from outside through generated **probe accounts**. Results come back over a small bearer-token API under `/mon/v1/*`; the panel is passive and stores nothing until a mon-server talks to it.

This gives the panel a **Monitoring** page (per-inbound state, event feed, latency/availability sparklines), a **Health** column and a `down` filter in the Inbounds table, Telegram alerts on DOWN/UP plus a block in the daily digest, and a **STALE** flag once the mon-server goes silent past the configured threshold (15 minutes by default). Probe accounts carry a `probe-` prefix, show a `probe` badge, and are excluded from online counts, Telegram stats and LDAP sync.

Get the token in **Panel Settings → Monitoring** (enable switch, token with Copy / Regenerate, stale threshold, retention, probe-set line with Remove), or from the CLI:

```
x-ui setting -showMonToken
x-ui setting -resetMonToken
x-ui setting -monEnable true
```

— also menu item **27** in `x-ui`, or the `x-ui mon-token` shortcut. Regenerating invalidates the old token at once, so update the mon-server config right away.

See the repo above, the [monitoring panel spec](https://github.com/SBKubric/3ax-ui-proxy/blob/29-monitoring-spec/docs/spec/monitoring-panel.md) and the [wire contract](https://github.com/SBKubric/3ax-ui-proxy/blob/29-monitoring-spec/docs/spec/monitoring-contract.md).

> **Note:** monitoring is off by default — until enabled with a token issued, `/mon/v1` answers a bare 404.

---


### 13. Extras

- **Telegram bot:** sends connection links and QR codes straight to the client's chat on creation.
- **Configurable QR code size:** 300 / 450 (default) / 600 px.
- **Secure subscription URL by default:** on install the subscription path is generated with a random 12-character suffix (e.g. `/sub-Xk92mPqLvzRt/`) instead of `/sub/`.

## Server requirements

- **OS:** Ubuntu 22.04+ / Debian 11+
- **Linux kernel:** 5.6+ (for built-in WireGuard), or an installed AmneziaWG DKMS module
- **RAM:** 1024 MB or more
- **Architecture:** amd64 / arm64 (multi-user MTProto is available only on these; on other arches MTProto runs in single-secret mode)

> **Secure Boot:** If Secure Boot is enabled on the server, the AmneziaWG DKMS module may fail to load. The install script will warn you automatically.

---

## Installation

```bash
# Stable release (v1.8.1.x: no chain, no monitoring)
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/install.sh)

# Latest pre-release
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/install.sh) --beta

# Specific version
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/install.sh) v1.9.0-chain.6
```

## Panel Update

```bash
# Stable release
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/update.sh)

# Latest pre-release
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/sane-3x-ui/main/update.sh) --beta
```

---

## AmneziaWG quick start

1. Log into the panel → **AWG Settings**
2. Configure network parameters and obfuscation settings (or click **Generate** for a 2.0 set)
3. Go to **Inbounds** → **Add Inbound**
4. Select the **amneziawg** protocol, enter a client email, and click **Create**
5. In the client table, click the QR code icon and scan it in the AmneziaVPN app

## MTProto (Telegram) quick start

1. **Inbounds** → **Add Inbound** → **MTProto (Telegram)** protocol
2. Set the port and a fronting domain (or click **↻** for a random one), name the first client → **Create**
3. Open the client's QR / link in the table — clicking the `tg://` link opens Telegram and offers to enable the proxy
4. To add more users on the same port, use the add-client button in the inbound's row (on amd64/arm64)

---

## Compatible clients

| Protocol | Client | Platforms |
|----------|--------|-----------|
| AmneziaWG | AmneziaVPN — [amnezia.org](https://amnezia.org) | Android, iOS, Windows, macOS, Linux |
| native WireGuard | Official WireGuard | All platforms |
| MTProto | Telegram (built-in proxy support) | All platforms |

> Standard WireGuard clients are **not compatible** with AmneziaWG — they do not support obfuscation parameters.

---

## Based on

sane-3x-ui is a fork of **[3AX-UI](https://github.com/coinman-dev/3ax-ui)** by [coinman-dev](https://github.com/coinman-dev), which in turn is based on **[3x-ui](https://github.com/MHSanaei/3x-ui)** by [MHSanaei](https://github.com/MHSanaei). All original features (VLESS, VMess, Trojan, Shadowsocks, WireGuard, Xray, subscriptions, Telegram bot, etc.) are fully preserved, as are 3AX-UI's AmneziaWG, native WireGuard and MTProto.

Monitoring (mon-server and mon-client) lives in [SBKubric/3ax-ui-monitoring](https://github.com/SBKubric/3ax-ui-monitoring), deployment in [SBKubric/sane-3x-ui-orchestrator](https://github.com/SBKubric/sane-3x-ui-orchestrator).

The MTProto proxy runs on the **[mtg](https://github.com/9seconds/mtg)** sidecar (single-secret) and its **[mtg-multi](https://github.com/dolonet/mtg-multi)** fork (multi-user).

## Acknowledgements

- [coinman-dev](https://github.com/coinman-dev) — author of 3AX-UI, the fork this one builds on
- [MHSanaei](https://github.com/MHSanaei/) — author of the original 3x-ui
- [alireza0](https://github.com/alireza0/) — author of the original x-ui
- [9seconds/mtg](https://github.com/9seconds/mtg) and [dolonet/mtg-multi](https://github.com/dolonet/mtg-multi) — MTProto sidecars
- [Iran v2ray rules](https://github.com/chocolate4u/Iran-v2ray-rules) (GPL-3.0)
- [Russia v2ray rules](https://github.com/runetfreedom/russia-v2ray-rules-dat) (GPL-3.0)

---

## License

This project is distributed under the same license as the original 3x-ui — [GNU GPL v3](LICENSE).
