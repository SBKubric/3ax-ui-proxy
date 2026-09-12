# Runbook: стенд proxy front (dokodemo relay → xray → WARP)

Как поднять и проверить связку из двух VPS: **real server** с панелью 3AX-UI (форк с proxy-фичей), у которого весь исходящий трафик уходит через встроенный WARP, и одноразовый **proxy front**, который клиенты видят вместо real server. Термины — по [CONTEXT.md](../../CONTEXT.md); решение о relay manifest и setup page — [ADR 0001](../adr/0001-relay-manifest-via-setup-page.md). Runbook собран по реально пройденному стенду (issue #2 форка и его тикеты); все команды ниже выполнялись.

```
клиент ──VLESS-Reality / AmneziaWG──▶ proxy front ──dokodemo-door (L4)──▶ real server ──xray outbound warp──▶ Cloudflare WARP ──▶ интернет
        (в конфиге только IP прокси)   (ключей нет)                         (TLS/Reality, AWG, панель)
```

## 1. Предусловия

- **Два VPS.** На стенде: `real` — Ubuntu 24.04, 870 MB RAM (хватает: панель + xray + awg ≈ 400 MB used); `proxy` — Debian 13, 1 vCPU. IPv6 необязателен.
- **Порты снаружи и с proxy → real:** 443 tcp+udp (VLESS-Reality), 51820 udp (AmneziaWG), 2096 tcp (подписки), порт панели (только админу). Провайдерский файрвол не должен их резать.
- **Доступ:** SSH-хосты `real` и `proxy` в `~/.ssh/config` рабочей машины.
- **Релиз форка.** Бинарники ставятся штатным `install.sh` **из форка** (`https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh`); `XUI_REPO` в нём по умолчанию `SBKubric/3ax-ui-proxy`. Схема тегов — `v<upstream>.<N>` без дефиса (`v1.8.1.5`): тег с дефисом GitHub считает pre-release, и `releases/latest` его не отдаёт.
- **На рабочей машине:** `docker` (для e2e-клиентов: `ghcr.io/xtls/xray-core`, `amneziavpn/amneziawg-go`, `golang:1.26` для тестов). Rootless podman без subuid/subgid образы не распаковывает.
- **Debian-бокс без curl:** `apt-get update && apt-get install -y curl` — без `update` установка тихо падает.

## 2. Real server

### 2.1 Установка панели

```bash
ssh real 'bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)'
x-ui settings          # порт, webBasePath, креды (без TTY они случайные)
```

Грабли:
- **Let's Encrypt для IP проигрывает гонку за порт 80** апстримному `install_nginx`: панель откатывается на self-signed (`/root/cert/self-signed/`). Для стенда достаточно; proxy front ходит к панели с `InsecureSkipVerify`.
- Свежая установка ставит `nginxMode=shared`, и панель отвергает inbound на 443 («Port already exists»). Стенд живёт **без** nginx-режима «всё за 443»:

```bash
ssh real 'sqlite3 /etc/x-ui/x-ui.db "update settings set value=\"off\" where key=\"nginxMode\";" && systemctl restart x-ui'
```

- Для AWG с «Route via Xray» нужен `iptables` (на Ubuntu 24.04 его может не быть): `apt-get install -y iptables`.
- Включите JSON-подписку: Settings → Subscription → `subJsonEnable` (без неё proxy отдаёт 502 на `/json`).

### 2.2 Inbound'ы

| Inbound | Параметры стенда |
|---|---|
| VLESS + Reality | TCP 443, flow `xtls-rprx-vision`, **dest `www.apple.com:443`** — с `www.microsoft.com` на Xray 26.3.27 handshake не проходит (пост-квантовый key share в ServerHello). Рабочие альтернативы: `www.cloudflare.com`, `dl.google.com`, `gateway.icloud.com`. |
| AmneziaWG | UDP 51820, `10.66.66.1/24`, **Route via Xray = on** (панель добавляет `awg-tproxy-in`, TPROXY 12345). |

### 2.3 WARP и routing

1. Xray Settings → WARP → бесплатная регистрация; панель создаёт outbound `wireguard` с тегом `warp`.
2. В outbound `warp` поставьте **`"noKernelTun": true`**: от root xray поднимает kernel-TUN, в котором UDP не ходит (`proxy/wireguard: … use of WriteTo with pre-connected connection`); TCP работает и так, поэтому баг незаметен до первого UDP.
3. Routing: `geoip:private → direct/blocked` выше, затем catch-all `network: tcp,udp → warp`. Сохраните Xray Settings (именно «Сохранить» кладёт outbound в конфиг) и перезапустите Xray: `POST /panel/api/server/restartXrayService` (`/panel/xray/update` сам Xray не перезапускает).
4. Проверка с сервера: временный xray с socks → outbound `warp`, `curl --socks5 … https://cloudflare.com/cdn-cgi/trace` → `warp=on`, чужой `ip=`; напрямую с сервера `warp=off`. Панельный `testOutbound` для `warp` должен давать 204.

### 2.4 Relay manifest

```bash
ssh real 'cd /usr/local/x-ui && ./x-ui relay-manifest'            # в stdout
ssh real 'cd /usr/local/x-ui && ./x-ui relay-manifest -o /root/relay-manifest.json'
```

или Settings → Subscription → **Show manifest** → Copy. В манифесте только `listen/port/protocol/tag` и TPROXY-маркеры плюс маркер `relayManifest`; `grep -c 'privateKey\|password\|"id"' relay-manifest.json` → 0.

## 3. Proxy front

### 3.1 Установка через setup page

```bash
ssh proxy 'XUI_PROXY_MODE=1 \
PROXY_UPSTREAM_HOST=<real-ip> \
PROXY_UPSTREAM_BASE=https://<real-ip>:2096 \
PROXY_SUB_PATH=/<subPath панели>/ PROXY_JSON_PATH=/json/ \
PROXY_EXTRA_PORTS=51820/udp \
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)'
```

Футер печатает одноразовую ссылку `http://<proxy-ip>:2096/setup/<token>`; повторно: `x-ui proxy-setup-url` (файл `/etc/x-ui/proxy-setup.url`). До вставки бокс слушает **только** 2096.

Вставьте манифест в браузере или curl'ом с рабочей машины:

```bash
URL=$(ssh proxy 'cat /etc/x-ui/proxy-setup.url')
curl -s -w 'status=%{http_code}\n' --data-urlencode "manifest@relay-manifest.json" "$URL"   # 200, «ports 443, 51820/udp»
curl -s -o /dev/null -w '%{http_code}\n' "$URL"                                             # 404 — ссылка погашена
```

Сырой `config.json` страница отвергает (400, «not a relay manifest»), файл не пишется, ссылка остаётся.

Скриптованный вариант без страницы: положить манифест на бокс и добавить `PROXY_RELAY_MANIFEST=/root/relay-manifest.json` (файл без маркера отклоняется и не копируется).

**Переустановка поверх существующего бокса без TTY:** ответьте `2` на вопрос «already installed»: `printf '2\n' | XUI_PROXY_MODE=1 … bash <(curl …)`. Иначе установщик уходит в путь обновления.

### 3.2 Проверка

```bash
ssh proxy 'systemctl is-active x-ui; ss -ltnup | grep -E ":443 |:51820 |:2096 "; journalctl -u x-ui -n 5 --no-pager'
# ожидаем: udp+tcp 443, udp 51820, tcp 2096; в логе «relaying ports [443 51820] -> <real-ip>»,
# «not relaying "api": internal api inbound», «not relaying "awg-tproxy-in": transparent-proxy (TPROXY) inbound»
ssh proxy 'grep -rIl "privateKey\|PrivateKey" /etc/x-ui /usr/local/x-ui; ls -la /etc/x-ui'   # ключей нет; relay-manifest.json 0600
curl -s -H 'User-Agent: v2rayNG/1.9' http://<proxy-ip>:2096/json/<subId> | grep -c '<real-ip>'   # 0 (после override)
```

Порт 12345 (TPROXY) на прокси открываться **не должен**.

## 4. Host override

Settings → Subscription → *Proxy front*: включить и указать IP/домен proxy front; либо в Telegram-боте `/proxy <ip>` (`/proxy` — состояние, `/proxy off`). После override подписки, JSON и AWG-конфиги (`Endpoint = <proxy-ip>:51820`) указывают на прокси; заголовок `Profile-Web-Page-Url` proxy переписывает на себя (v1.8.1.2+).

Грабли: сохранение настроек в панели — **полная замена** всех полей состоянием открытой страницы. После изменений через API или бота перезагрузите страницу настроек, прежде чем что-то сохранять, иначе override откатится.

## 5. E2E-чеклист (шесть критериев приёмки)

Клиенты запускаются на рабочей машине в docker.

**VLESS через relay (критерии 1, 2, 4):**

```bash
curl -s -H 'User-Agent: v2rayNG/1.9' http://<proxy-ip>:2096/json/<subId> -o sub.json
grep -c '"address": "<proxy-ip>"' sub.json; grep -c '<real-ip>' sub.json          # 1 / 0
docker run --rm -v $PWD/sub.json:/etc/xray/sub.json:ro ghcr.io/xtls/xray-core:latest run -test -c /etc/xray/sub.json   # Configuration OK
docker run -d --name xray-e2e --network host -v $PWD/sub.json:/etc/xray/sub.json:ro ghcr.io/xtls/xray-core:latest run -c /etc/xray/sub.json
curl -s https://cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp)='                          # с машины: warp=off
curl -s --socks5-hostname 127.0.0.1:10808 https://cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp|colo)='   # warp=on, ip Cloudflare
docker rm -f xray-e2e
```

Пока трафик идёт, на real `ss -tn | grep ':443 '` показывает только `<proxy-ip>` (критерий 4). Access-log на панели по умолчанию выключен.

**AmneziaWG через relay, UDP (критерии 4, 5):** конфиг клиента — из панели (`GET /panel/api/awg/client/<id>/config`, поле `obj`), в нём `Endpoint = <proxy-ip>:51820`.

```bash
docker run -d --name awg-e2e --cap-add NET_ADMIN --device /dev/net/tun \
  -v $PWD/awg0.conf:/etc/amnezia/amneziawg/awg0.conf:ro --entrypoint sleep amneziavpn/amneziawg-go infinity
docker exec awg-e2e sh -c '
GW=$(ip route | awk "/default/ {print \$3; exit}")
awg-quick strip /etc/amnezia/amneziawg/awg0.conf > /tmp/awg0.conf
amneziawg-go awg0 2>/dev/null || true          # при модуле ядра на хосте интерфейс поднимет ядро
awg setconf awg0 /tmp/awg0.conf
ip addr add 10.66.66.2/32 dev awg0; ip link set mtu 1420 up dev awg0
ip route add <proxy-ip>/32 via $GW dev eth0    # ОБЯЗАТЕЛЬНО до /1-маршрутов, иначе туннель заворачивается сам в себя
ip route add 0.0.0.0/1 dev awg0; ip route add 128.0.0.0/1 dev awg0
echo "nameserver 1.1.1.1" > /etc/resolv.conf; sleep 3
awg show awg0 latest-handshakes
wget -qO- https://cloudflare.com/cdn-cgi/trace | grep -E "^(ip|warp|colo)="   # warp=on
nslookup example.com 1.1.1.1 | head -3                                          # UDP-DNS через туннель'
docker rm -f awg-e2e; shred -u awg0.conf
```

На real `awg show awg0 endpoints` показывает `<proxy-ip>:<port>`; в xray-логе панели при `loglevel=info` — `[awg-tproxy-in -> warp]`. `awg-quick` в контейнере падает на sysctl (`/proc/sys` read-only) — поэтому интерфейс собирается вручную.

**Proxy front без ключей (критерий 3):** раздел 3.2.

**Telegram (критерий 6):** `/proxy` в боте показывает состояние и отдаёт proxy-подписку; проверяет владелец.

## 6. Обновления

- **real:** `bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/update.sh) </dev/null` — проверено 1.8.1.1 → 1.8.1.3 → 1.8.1.4: xray держит 443, `nginxMode=off` и override сохраняются.
- **proxy:** тот же `update.sh`; бокс определяется по `/etc/x-ui/proxy.json`, `proxy.json` и unit сохраняются (проверено 1.8.1.1 → 1.8.1.2). Если в `proxy.json` остался старый ключ `xrayConfigPath`, `update.sh` предупредит: нужны `relayManifestPath` и манифест.
- Панельная «проверка обновлений» смотрит на `coinman-dev/3ax-ui`, а не на форк — ориентируйтесь на `x-ui -v` и релизы форка.

## 7. Известные ограничения и открытые вопросы

Вне рамок стенда: PROXY protocol / реальные client-IP за relay (IP-лимит и IP-лог видят IP прокси); nginx-режим «всё за 443» на real; TLS/домен для подписок на proxy; WARP+; Hysteria2.

Не решено (см. карту #2): гонка LE за порт 80 в `install.sh`; дефолт `noKernelTun` в WARP-модалке; дефолтный Reality `dest` в GUI; прятать ли веб-UI панели с публичного IP; замена relay manifest после смены inbound'ов (setup page доступна только пока манифеста нет — пока: удалить `/etc/x-ui/relay-manifest.json` и `systemctl restart x-ui`, бокс снова в bootstrap-режиме); общие разделы README всё ещё ссылаются на апстримный `install.sh`.

## 8. API-шпаргалка панели

```bash
P=https://<real-ip>:<port>/<webBasePath>
curl -sk -c cookies -X POST $P/login --data-urlencode username=… --data-urlencode password=…
curl -sk -b cookies $P/panel/api/server/getRelayManifest          # {"success":true,"obj":"<манифест>"}
curl -sk -b cookies $P/panel/api/awg/clients                       # AWG-клиенты
curl -sk -b cookies $P/panel/api/awg/client/<id>/config            # клиентский .conf в obj
curl -sk -b cookies -X POST $P/panel/api/server/restartXrayService # перезапуск Xray после правок конфига
curl -sk -b cookies -X POST $P/panel/setting/all                   # все настройки (proxyOverrideEnable/Host, subJsonEnable, nginxMode)
```

`POST /panel/setting/update` принимает **полный** объект настроек — перед отправкой возьмите актуальный из `/panel/setting/all`.
