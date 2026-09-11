# Встроенный WARP: outbound, routing и проверка egress через Cloudflare (вкл. AWG TPROXY)

Исследование по тикету [#6](https://github.com/SBKubric/3ax-ui-proxy/issues/6) (часть карты [#2](https://github.com/SBKubric/3ax-ui-proxy/issues/2)).
Дата: 2026-09-11. Ссылки на код даны относительно HEAD `b1e67026` ветки `main`.

## TL;DR

- Бэкенд (`web/service/warp.go`) только регистрирует устройство в `api.cloudflareclient.com` и хранит `access_token / device_id / license_key / private_key` в настройке `warp`. **Outbound генерирует фронтенд** (`web/html/modals/warp_modal.html`): протокол `wireguard`, **тег `warp`**, `mtu: 1420`, `domainStrategy: "ForceIP"`, `noKernelTun: false`, peer `engage.cloudflareclient.com:2408`. Он добавляется в `templateSettings.outbounds` и попадает в БД только после «Сохранить» на странице Xray Settings.
- Готового переключателя «весь трафик → WARP» в панели **нет**. Quick-toggle «Правила WARP» (Basics) пишет только `domain`-правила. Нужное правило создаётся в Xray Settings → Routing (модалка правила умеет `inboundTag` = `awg-tproxy-in` и `outboundTag` = `warp`) или в Advanced-JSON. Ключевое: правило `geoip:private` из шаблона по умолчанию ведёт в **`blocked`** и должно стать `direct` и стоять **выше** правила на `warp`.
- Проверка: с сервера `curl https://www.cloudflare.com/cdn-cgi/trace` даёт `warp=off` (базовая линия); с клиента AWG — `warp=on` (`warp=plus` при лицензии) и `ip=` ≠ IP сервера. Серверное доказательство для AWG: строки `[awg-tproxy-in -> warp]` в access.log Xray, счётчики `outbound>>>warp>>>traffic>>>*` и `tcpdump udp port 2408`.
- Каверзы: панель ставит MTU 1420, тогда как официальные клиенты и wgcf используют **1280**; IPv6 через WARP работает только если у AWG-сервера включён IPv6; лицензия WARP+ — 5 устройств, реферальные ключи не принимаются, API неофициальный; по сообщениям сообщества, WARP+ поверх WireGuard может показывать `warp=on`, а не `warp=plus`.

---

## 1. Что делает встроенный WARP панели

### 1.1 Бэкенд: только регистрация и хранение учётки

| Действие | Код | Что происходит |
|---|---|---|
| Регистрация | `web/service/warp.go:71-123` | `POST https://api.cloudflareclient.com/v0a2158/reg` (`:76`) с телом `{"key":<pubkey>,"tos":<now>,"type":"PC","model":"x-ui","name":<hostname>}` (`:74`), заголовок `CF-Client-Version: a-7.21-0721` (`:83`). Из ответа берутся `id`, `token`, `account.license` (`:106-109`). В настройку `warp` сохраняется JSON `{access_token, device_id, license_key, private_key}` (`:115-118`; `web/service/setting.go:701-705`). |
| Получить конфиг устройства | `warp.go:37-69` | `GET /v0a2158/reg/{device_id}` с `Authorization: Bearer <access_token>`. Ответ (peers, interface.addresses, client_id) отдаётся фронту как есть. |
| Установить лицензию WARP+ | `warp.go:125-179` | `PUT /v0a2158/reg/{device_id}/account` с `{"license": ...}`, при `success:false` — ошибка с кодом Cloudflare. |
| Удалить | `warp.go:29-35` | Очищает настройку `warp`. |

HTTP-маршрут: `POST /panel/xray/warp/:action` (`web/controller/xray_setting.go:38`), действия `data / del / config / reg / license` (`:133-152`).

Ключевой факт: **бэкенд не пишет ничего в xray-шаблон**. Единственные упоминания `"warp"` в Go-коде — это ключ настройки (`web/service/setting.go:84,701,705`).

Пара ключей WireGuard генерируется в браузере (`Wireguard.generateKeypair()`, `warp_modal.html:179`) и отправляется на `/panel/xray/warp/reg` (`:180`); приватный ключ хранится в БД панели.

### 1.2 Фронтенд: сборка outbound с тегом `warp`

`web/html/modals/warp_modal.html`, метод `collectConfig()` (`:131-153`):

```js
warpModal.warpOutbound = Outbound.fromJson({
    tag: 'warp',                          // :136
    protocol: Protocols.Wireguard,        // :137
    settings: {
        mtu: 1420,                        // :139
        secretKey: warpModal.warpData.private_key,
        address: this.getAddresses(config.interface.addresses), // v4/32 + v6/128, :155-160
        reserved: this.getResolved(config.client_id),           // base64 client_id → [b0,b1,b2], :161-176
        domainStrategy: 'ForceIP',        // :145
        peers: [{ publicKey: peer.public_key, endpoint: peer.endpoint.host }],
        noKernelTun: false,               // :150
    }
});
```

`Outbound.WireguardSettings.toJson()` (`web/assets/js/model/outbound.js:1968-1979`) дополняет это `workers: 2` и для peer — `allowedIPs: ["0.0.0.0/0","::/0"]`, `keepAlive: 0` (`:1983-2018`). `ForceIP` входит в допустимый список `WireguardDomainStrategy` (`outbound.js:67-73`).

Итоговый объект, который панель кладёт в список outbounds (значения — плейсхолдеры):

```json
{
  "tag": "warp",
  "protocol": "wireguard",
  "settings": {
    "mtu": 1420,
    "secretKey": "<private_key>",
    "address": ["172.16.0.2/32", "2606:4700:110:....../128"],
    "workers": 2,
    "domainStrategy": "ForceIP",
    "reserved": [0, 0, 0],
    "peers": [
      {
        "publicKey": "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
        "allowedIPs": ["0.0.0.0/0", "::/0"],
        "endpoint": "engage.cloudflareclient.com:2408",
        "keepAlive": 0
      }
    ],
    "noKernelTun": false
  }
}
```

Где это оказывается:

1. Кнопка «Add outbound» в модалке → `addOutbound()` (`warp_modal.html:220-224`): `app.templateSettings.outbounds.push(...)` и `app.outboundSettings = JSON.stringify(...)`. «Reset» (`:225-229`) **перезаписывает** outbound с тегом `warp` дефолтным набором (то есть стирает ручные правки MTU и т.п.); удаление учётки удаляет и outbound (`:230-236`).
2. Пользователь нажимает «Сохранить» на странице Xray Settings → `SaveXraySetting` (`web/service/xray_setting.go:17-31`) → настройка `xrayTemplateConfig`. Единственная нормализация при сохранении — `EnsureStatsRouting` поднимает правило `api → api` на первое место (`:111-160`), остальной порядок правил не трогается.
3. При запуске Xray `GetXrayConfig` (`web/service/xray.go:135-145`) десериализует шаблон; `outbounds` и `routing` копируются как `RawMessage` без изменений (`xray/config.go:13,16`), к `inbounds` дописываются пользовательские инбаунды и синтетические TPROXY-инбаунды (`xray.go:281-286`).

Наличие outbound определяется исключительно по тегу: `WarpExist` (`web/html/xray.html:1702-1707`) и `warpOutboundIndex` (`warp_modal.html:239-244`) ищут `o.tag == "warp"`. Переименовывать тег нельзя — панель «потеряет» WARP.

Кнопки «WARP» находятся во вкладках Outbounds (`web/html/settings/xray/outbounds.html:10`) и Basics (`basics.html:281`).

### 1.3 Что говорят первоисточники о полях outbound

Документация Xray по WireGuard outbound (<https://xtls.github.io/en/config/outbounds/wireguard.html>):

- `mtu` — по умолчанию **1420**; «calculate as encrypted data length minus headers».
- `domainStrategy` — по умолчанию `"ForceIP"`; WireGuard требует IP, поэтому домены всегда резолвятся (варианты `ForceIPv4/ForceIPv6/ForceIPv4v6/ForceIPv6v4`).
- `reserved` — «WireGuard reserved bytes» (3 байта из `client_id` WARP; руководство Xray по WARP отмечает, что для части PoP «Hong Kong / Los Angeles» без `reserved` соединение не устанавливается — <https://xtls.github.io/en/document/level-2/warp.html>).
- `noKernelTun` — по умолчанию `false`; «On Linux with `CAP_NET_ADMIN`, the system kernel TUN interface is used by default (higher performance); otherwise gVisor is employed. Kernel TUN uses routing table 10230 (incremented per additional outbound)». В контейнере без `NET_ADMIN` Xray молча уйдёт в gVisor.

Cloudflare (<https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/warp/deployment/firewall/>): endpoint `engage.cloudflareclient.com` резолвится в диапазон WARP-ingress; для потребительского клиента (1.1.1.1 with WARP) это `162.159.192.0/24`; WireGuard — `UDP 2408`, запасные `UDP 500/1701/4500`. С сервера должен быть открыт исходящий UDP на эти порты.

---

## 2. Routing: весь трафик (включая `awg-tproxy-in`) → `warp`, private → `direct`

### 2.1 Как выглядит синтетический TPROXY-инбаунд

`buildTproxyInbound` (`web/service/xray.go:449-462`):

```json
{
  "tag": "awg-tproxy-in",
  "listen": "::",
  "port": 12345,
  "protocol": "dokodemo-door",
  "settings": {"network": "tcp,udp", "followRedirect": true},
  "streamSettings": {"sockopt": {"tproxy": "tproxy"}},
  "sniffing": {"enabled": true, "destOverride": ["http","tls","quic"], "routeOnly": true}
}
```

- Тег и порт берутся из `AwgServer.XrayInboundTag` / `XrayTproxyPort` (`database/model/awg.go:70-72`, дефолты `awg-tproxy-in` / `12345`; для native WG — `wg-tproxy-in` / `12346`, `tunnel/kind.go:65-67, 86-88`). Инбаунд создаётся только для серверов с `enable=1 AND route_via_xray=1` (`xray.go:412-413`).
- `followRedirect: true` — «tunnel will recognize data forwarded by iptables and forward it to the corresponding target address» (<https://xtls.github.io/en/config/inbounds/tunnel.html>); `sockopt.tproxy: "tproxy"` — «transparent proxy in TProxy mode, supporting all IPv4 and IPv6 TCP and UDP connections», требует root или `CAP_NET_ADMIN` (<https://xtls.github.io/en/config/transports/sockopt.html>).
- `routeOnly: true` — «Use the sniffed domain only for routing; the proxy destination address remains the IP» (<https://xtls.github.io/en/config/inbound.html>). Значит, для TPROXY-трафика адрес назначения при маршрутизации всегда IP (доменные правила тоже работают благодаря sniffing).

iptables-часть (`shared/tproxy/tproxy.go:36-61`) заворачивает **весь** ingress с `awg0` — TCP и UDP, любые destination, включая приватные сети, сам сервер (`10.66.66.1`) и DNS (`1.1.1.1`, `awg.go:46`):

```
ip rule add fwmark 0x1/0x1 lookup 100
ip route replace local default dev lo table 100
iptables -t mangle -A PREROUTING -i awg0 -p tcp -j TPROXY --on-ip 127.0.0.1 --on-port 12345 --tproxy-mark 0x1/0x1
iptables -t mangle -A PREROUTING -i awg0 -p udp -j TPROXY --on-ip 127.0.0.1 --on-port 12345 --tproxy-mark 0x1/0x1
```

MASQUERADE при `RouteViaXray` **не** ставится (`tunnel/config.go:283-291`), т.е. вне Xray у AWG-клиентов выхода в интернет нет — все решения «куда» принимает Xray. IPv6-половина правил появляется только при `IPv6Enabled` (`tproxy.go:51-58`).

### 2.2 Что в шаблоне по умолчанию и почему это важно

`web/service/config.json:58-83`:

```json
"routing": {
  "domainStrategy": "AsIs",
  "rules": [
    {"type":"field","inboundTag":["api"],"outboundTag":"api"},
    {"type":"field","outboundTag":"blocked","ip":["geoip:private"]},
    {"type":"field","outboundTag":"blocked","protocol":["bittorrent"]}
  ]
}
```

Правила Xray применяются сверху вниз, срабатывает первое совпавшее; условия внутри правила — логическое И; если ничего не совпало — «traffic is sent via the first outbound by default» (<https://xtls.github.io/en/config/routing.html>). Первый outbound в шаблоне — `direct` (freedom, `config.json:29-37`).

Следствия для AWG:

1. Без дополнительных правил весь трафик `awg-tproxy-in` уходит в `direct` (с IP сервера), а не в `warp`.
2. Правило `geoip:private → blocked` перехватывает и трафик из туннеля к самому серверу (`10.66.66.1`), к соседним клиентам пула и к LAN — для сценария «private → direct» его надо **заменить**, а не дополнить (первое совпавшее правило побеждает).

### 2.3 Минимальный набор правил

Вариант A — **всё в WARP, private напрямую** (все инбаунды, включая VLESS/Reality и `awg-tproxy-in`):

```json
"routing": {
  "domainStrategy": "AsIs",
  "rules": [
    {"type": "field", "inboundTag": ["api"], "outboundTag": "api"},
    {"type": "field", "ip": ["geoip:private"], "outboundTag": "direct"},
    {"type": "field", "protocol": ["bittorrent"], "outboundTag": "blocked"},
    {"type": "field", "network": "tcp,udp", "outboundTag": "warp"}
  ]
}
```

- `geoip:private` — «includes all private addresses, such as 127.0.0.1» (routing docs); для TPROXY-трафика адрес уже IP, `AsIs` его не резолвит и не должен — совпадение точное. Для VLESS-клиентов, отправляющих домен, приватный адрес по имени не будет распознан при `AsIs`; если это нужно, ставьте `IPIfNonMatch` («resolve the domain to an IP and perform a second round of matching»).
- Последнее правило `network: "tcp,udp"` — явный catch-all. Альтернатива — переставить `warp` первым в `outbounds` (тогда он станет дефолтом без правила), но тогда фронт при удалении quick-правил считает outbound под индексом 0 «системным» (`xray.html:617-620`, `outboundIndex > 0`), поэтому явное правило надёжнее.
- Правило `bittorrent` опционально; без него торрент-трафик тоже уйдёт в WARP.

Вариант B — **только AWG в WARP**, остальные инбаунды как раньше:

```json
{"type": "field", "ip": ["geoip:private"], "outboundTag": "direct"},
{"type": "field", "inboundTag": ["awg-tproxy-in"], "outboundTag": "warp"}
```

(`inboundTag` — «The rule takes effect when an item matches the identifier of the inbound protocol», routing docs.) Правило на private должно стоять **выше**.

Порядок в итоговом конфиге: `injectMtprotoEgress` может добавить своё правило в самое начало (`xray.go:376`), `EnsureStatsRouting` поднимает `api` на первое место — на семантику «private → direct → всё остальное → warp» это не влияет.

### 2.4 Может ли UI панели это выразить

Да, но по частям — единого переключателя нет:

| Что нужно | Где в UI | Код |
|---|---|---|
| Создать outbound `warp` | Xray Settings → Outbounds → кнопка WARP → Create → Info → Add outbound | `warp_modal.html:5, 40, 88-89, 220-224` |
| Правило `inboundTag: awg-tproxy-in → warp` | Xray Settings → Routing → «Add rule»: Inbound tag выбирается из списка, куда бэкенд добавляет синтетические TPROXY-теги; Outbound — из списка outbounds шаблона (там есть `warp`) | `web/service/inbound.go:2335-2346` (`GetInboundTags`), `xray.html:564`, `xray_rule_modal.html:104-110, 202-205` |
| Catch-all `network: tcp,udp → warp` | Та же модалка (поле Network), либо Advanced → JSON | `xray_rule_modal.html`, `xray.html:1328-1336` |
| `geoip:private → direct` | Basics → «Direct IPs» добавляет правило с `outboundTag: "direct"`; «Blocked IPs» — с `"blocked"`. Нужно **убрать** `geoip:private` из Blocked и добавить в Direct | `xray.html:1558-1564` (blockedIPs), `:1603-1609` (directIPs), `templateRuleSetter :645-690` |
| Порядок правил | Routing → drag-and-drop таблицы (a-table-sortable → `replaceRule`) | `settings/xray/routing.html:5-15`, `xray.html:1200-1211` |

Что делает существующий quick-toggle «Правила WARP» (Basics): `warpDomains` (`xray.html:1651-1665`) пишет правило `{"type":"field","outboundTag":"warp","domain":[...]}` — только доменный сплит для отдельных сервисов. Подпись в i18n: «These options will route traffic based on a specific destination via WARP» (`web/translation/translate.en_US.toml:722`). Это не «всё через WARP».

Подсказка панели у тумблера AWG «Route traffic through Xray» прямо говорит, что цепочку надо достраивать в Xray → Routing (`translate.en_US.toml:229`).

### 2.5 DNS и IPv6 внутри туннеля

- AWG отдаёт клиентам DNS `1.1.1.1` (`awg.go:46`) → UDP/53 ловится TPROXY → под правилом catch-all уходит в `warp` (WireGuard outbound поддерживает UDP). Если DNS клиента указывает на сам сервер (`10.66.66.1`), его спасает правило `geoip:private → direct`.
- Если у AWG-сервера включён IPv6 (`awg.go:21`), появляется v6-половина TPROXY (`tproxy.go:51-58`), инбаунд слушает `::` — v6-трафик клиентов уйдёт в `warp` через v6-адрес WARP (`address[1]`), который панель добавляет всегда, когда API его вернул (`warp_modal.html:157-158`).

---

## 3. Проверка

### 3.1 Что показывает `cdn-cgi/trace`

Живой вывод с хоста (не через WARP):

```
$ curl -s https://www.cloudflare.com/cdn-cgi/trace
fl=151f56
h=www.cloudflare.com
ip=<публичный IP хоста>
ts=1789144423.000
visit_scheme=https
uag=curl/7.88.1
colo=RIX
sliver=none
http=http/2
loc=LV
tls=TLSv1.3
sni=plaintext
warp=off
gateway=off
rbi=off
kex=X25519
```

Значения поля `warp`: `off` — не через WARP; `on` — бесплатный WARP; `plus` — WARP+ (Cloudflare описывает `/cdn-cgi/trace` как диагностический endpoint — <https://developers.cloudflare.com/fundamentals/reference/cdn-cgi-endpoint/>; расшифровка полей в открытом описании <https://github.com/fawazahmed0/cloudflare-trace-api>: `warp` — «Whether client over Cloudflare's Wireguard VPN», `gateway`, `rbi`). Доказывают egress через WARP два поля вместе: **`warp=on|plus`** и **`ip=`, отличающийся от IP сервера** (адрес из сетей Cloudflare; `whois` покажет CLOUDFLARENET). `colo`/`loc` — PoP Cloudflare, через который вышли.

### 3.2 С самого сервера

1. Базовая линия: `curl -s https://www.cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp|colo)='` → `warp=off`, `ip=<IP сервера>`. Так и должно быть: правила routing действуют только на трафик через инбаунды Xray, а не на процессы хоста.
2. Через сам outbound `warp` (без клиента): временно добавить в Xray Settings → Inbounds не нужно — проще использовать встроенный тест outbound (Outbounds → Test): панель поднимает временный экземпляр Xray с SOCKS-инбаундом, маршрутизирует его в тестируемый outbound и делает GET на `outboundTestUrl` (`web/service/outbound.go:128-131, 244-255, 335-380`). Тест показывает только задержку и HTTP-код, содержимое `trace` не выводит, зато подтверждает, что рукопожатие WireGuard с `engage.cloudflareclient.com:2408` состоялось (при неверных `reserved`/ключах GET упадёт).
3. Ручной эквивалент с телом ответа: добавить в шаблон инбаунд `{"tag":"probe","listen":"127.0.0.1","port":1080,"protocol":"socks","settings":{"udp":false}}` и правило `{"type":"field","inboundTag":["probe"],"outboundTag":"warp"}` (или полагаться на catch-all), сохранить, затем
   `curl -s -x socks5h://127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp|colo)='` → ожидается `warp=on` (или `plus`) и IP Cloudflare. После проверки инбаунд удалить.
4. Состояние TPROXY-обвязки: `ss -lunp | grep 12345`, `ip rule show | grep 0x1`, `ip route show table 100`, `iptables -t mangle -L PREROUTING -v -n | grep TPROXY` — счётчики pkts/bytes должны расти, пока клиент AWG что-то делает (`tproxy.go:46-49`, `tunnel/kind.go:65-67`).

### 3.3 С клиента через туннель AWG

Подключиться клиентом AWG (AllowedIPs `0.0.0.0/0,::/0`, `awg.go:102`) и выполнить:

```
curl -4 -s https://www.cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp|colo|loc)='
curl -6 -s https://www.cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp)='   # если IPv6 включён на AWG
curl -s https://ifconfig.me ; echo
```

Ожидание: `warp=on` (или `plus`), `ip=` — адрес Cloudflare, **не** публичный IP сервера и не домашний IP клиента; `ifconfig.me` возвращает тот же адрес Cloudflare. Если `warp=off` и `ip=` равен IP сервера — трафик ушёл в `direct` (правило `warp` не сработало или стоит ниже другого). Если запрос вообще не проходит — трафик упёрся в `blocked` (например, сохранилось `geoip:private → blocked` при DNS на сервере) или TPROXY не активен.

### 3.4 Серверное доказательство, что именно AWG-трафик идёт в warp

1. **Access-log Xray.** В Xray Settings → Log переключить `access` с `none` на `./access.log` (`xray.html:331`; в шаблоне по умолчанию `"access":"none"`, `config.json:3`), сохранить, затем:
   `tail -f /usr/local/x-ui/bin/access.log | grep 'awg-tproxy-in -> warp'`
   Формат строки задаёт `AccessMessage.String()` в Xray-core (`common/log/access.go`): `from <src> accepted <dst> [<inbound> -> <outbound>]`, т.е. ожидается вид `from 10.66.66.2:51234 accepted tcp:104.16.x.x:443 [awg-tproxy-in -> warp]`. Появление `[awg-tproxy-in -> direct]` для непубличных адресов — норма. Путь `bin/` относителен папки панели (`config/config.go:75-81`, установка в `/usr/local/x-ui`, `install.sh:12`).
2. **Счётчики outbound.** В Basics включить «Stats outbound uplink/downlink» (`basics.html:69-83`; в шаблоне выключены, `config.json:54-55`), затем:
   `/usr/local/x-ui/bin/xray-linux-amd64 api statsquery --server=127.0.0.1:62789 -pattern 'outbound>>>warp'`
   Имена счётчиков `outbound>>>[tag]>>>traffic>>>uplink|downlink` документированы в <https://xtls.github.io/en/config/stats.html>; порт API — из инбаунда `api` шаблона (`config.json:21`, 62789; панель читает фактический порт из запущенного процесса, `web/service/xray.go:52-57`, `xray/process.go:174-176`). Значения должны расти синхронно с активностью клиента.
3. **tcpdump на внешнем интерфейсе.** `tcpdump -ni <ext-if> 'udp port 2408'` — во время работы клиента виден поток к `162.159.192.x`; одновременно `tcpdump -ni <ext-if> 'not udp port 2408 and not udp port 51820'` не должен показывать исходящих соединений к сайтам, которые открывает клиент (иначе есть утечка в `direct`). Порт 51820 — `ListenPort` AWG по умолчанию (`awg.go:9`).

---

## 4. Известные ограничения

### 4.1 MTU

- Панель прописывает `mtu: 1420` (`warp_modal.html:139`), что совпадает с дефолтом Xray, но **не** с практикой Cloudflare: wgcf — «To ensure maximum compatibility, the generated profile will have a MTU of 1280, just like the official Android app» (<https://github.com/ViRb3/wgcf>); руководство Xray по WARP тоже указывает 1280 (<https://xtls.github.io/en/document/level-2/warp.html>). Cloudflare One-клиент на macOS создаёт `utun` с MTU 1280, а для IPv6 требует минимум 1361 с учётом инкапсуляции и при меньшем MTU отключает IPv6 в туннеле (<https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/deployment/mdm-deployment/path-mtu-discovery/>).
- Цепочка «клиент → AWG (MTU 1420, `awg.go:10`) → Xray → WireGuard-outbound → WARP» — двойная инкапсуляция. TCP Xray терминирует на TPROXY-инбаунде и заново открывает поверх WARP-tun, так что MSS подстроится под MTU outbound; проблема — крупные UDP-датаграммы (QUIC, DNS-ответы с DNSSEC, игры): при MTU outbound 1420 и реальном path MTU к WARP меньше они теряются. Практическая рекомендация: выставить `mtu: 1280` у outbound `warp` (Outbounds → warp → MTU или Advanced JSON) и помнить, что кнопка «Reset» в WARP-модалке вернёт 1420 (`warp_modal.html:225-229`). Формула Xray: «encrypted data length minus headers (20/40 bytes IP + 8 UDP + 4 type + 4 key + 8 nonce + 16 auth tag)».

### 4.2 IPv6

- WARP выдаёт v6 `/128`; панель добавляет его в `address` (`warp_modal.html:157-158`), поэтому outbound способен ходить на v6-адреса даже с сервера без нативного IPv6 (внешний транспорт — UDP/IPv4 к `engage.cloudflareclient.com`).
- Клиенты AWG получат v6 внутри туннеля только при `IPv6Enabled` на сервере (`awg.go:21-24`; v6-часть TPROXY `tproxy.go:51-58`). Без этого `curl -6` с клиента просто не сработает — это не ошибка WARP.
- `domainStrategy: "ForceIP"` резолвит домены в любой семейство; если v6 через WARP ведёт себя нестабильно, можно переключить на `ForceIPv4` (список допустимых значений `outbound.js:67-73`, семантика в документации Xray). Для TPROXY-трафика это не влияет — назначение уже IP.
- Community-сообщение о некорректных данных `trace` при WARP поверх IPv6 (<https://community.cloudflare.com/t/cloudflare-warp-over-ipv6-showing-wrong-information-on-trace/305444>) — вторичный источник, не проверялся.

### 4.3 Бесплатный WARP, WARP+ и API

- Бесплатный WARP не имеет квоты трафика; WARP+ — «limited data plan», пополняемый рефералами, WARP+ Unlimited — платная подписка (материалы Cloudflare/1.1.1.1 в выдаче <https://developers.cloudflare.com/warp-client/>). В модалке панели видны `premium_data`, `quota`, `usage` из ответа API (`warp_modal.html:65-76`).
- Лицензия WARP+: максимум 5 привязанных устройств; «Only subscriptions purchased directly from the official 1.1.1.1 app are supported. Keys obtained by any other means, including referrals, will not work» (<https://github.com/ViRb3/wgcf>). Каждая регистрация панели — отдельное «устройство» с именем hostname (`warp.go:73-74`).
- По сообщениям сообщества Cloudflare (в выдаче поиска, страницы отдают 403 автоматическим клиентам, не проверено напрямую): при протоколе WireGuard `trace` может показывать `warp=on` вместо `warp=plus` даже с активной подпиской — <https://community.cloudflare.com/t/warp-does-not-work-with-wireguard/765494>, <https://community.cloudflare.com/t/1-1-1-1-with-wireguard-not-masque-is-not-picking-up-warp-unlimited-subscription/797012>. Панель использует именно WireGuard, MASQUE в Xray нет.
- API `api.cloudflareclient.com/v0a2158` неофициальный, панель подделывает `CF-Client-Version` (`warp.go:83`); Cloudflare может изменить/ограничить его без предупреждения.
- Сервисы с геолицензированием могут не работать, WebRTC WARP не проксирует (FAQ Cloudflare, <https://developers.cloudflare.com/warp-client/known-issues-and-faq/>); часть DNS-серверов провайдеров (Comcast, Cox) отвергает запросы с egress-IP WARP (<https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/troubleshooting/known-limitations/>). Egress-IP общий для многих пользователей — ожидаемы капчи и rate-limit.
- Один peer = один UDP-поток к одному PoP; балансировки/резерва нет. Kernel-TUN Xray занимает таблицу маршрутизации 10230 (+1 на каждый следующий WireGuard-outbound) — при нескольких таких outbound или конфликте с таблицами хоста включайте `noKernelTun: true` (документация Xray).
- Для PoP «Hong Kong / Los Angeles» без корректного `reserved` соединение не поднимается (руководство Xray по WARP); панель вычисляет `reserved` из `client_id` автоматически (`warp_modal.html:161-176`).

---

## Источники

Код репозитория (HEAD `b1e67026`):
- `web/service/warp.go`, `web/controller/xray_setting.go`, `web/service/setting.go`
- `web/html/modals/warp_modal.html`, `web/assets/js/model/outbound.js`, `web/html/xray.html`, `web/html/settings/xray/{basics,outbounds,routing}.html`, `web/html/modals/xray_rule_modal.html`
- `web/service/xray.go`, `web/service/xray_setting.go`, `web/service/inbound.go`, `web/service/outbound.go`, `web/service/config.json`, `xray/config.go`, `xray/process.go`
- `database/model/awg.go`, `tunnel/kind.go`, `tunnel/config.go`, `shared/tproxy/tproxy.go`, `config/config.go`
- i18n: `web/translation/translate.en_US.toml:228-229, 721-722`

Xray-core:
- WireGuard outbound — <https://xtls.github.io/en/config/outbounds/wireguard.html>
- Routing — <https://xtls.github.io/en/config/routing.html>
- Tunnel (dokodemo-door) inbound — <https://xtls.github.io/en/config/inbounds/tunnel.html>
- Sniffing — <https://xtls.github.io/en/config/inbound.html>
- Sockopt (`tproxy`, `mark`) — <https://xtls.github.io/en/config/transports/sockopt.html>
- Stats — <https://xtls.github.io/en/config/stats.html>
- Руководство «Enhancing Proxy Security via Cloudflare Warp» — <https://xtls.github.io/en/document/level-2/warp.html>
- Формат access-log — `common/log/access.go` в <https://github.com/XTLS/Xray-core>

Cloudflare:
- `/cdn-cgi/` endpoint — <https://developers.cloudflare.com/fundamentals/reference/cdn-cgi-endpoint/>
- WARP ingress IP/порты — <https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/warp/deployment/firewall/>
- PMTUD / MTU и IPv6 — <https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/deployment/mdm-deployment/path-mtu-discovery/>
- Known limitations — <https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/troubleshooting/known-limitations/>
- WARP client FAQ — <https://developers.cloudflare.com/warp-client/known-issues-and-faq/>
- Живой ответ `https://www.cloudflare.com/cdn-cgi/trace` (снят 2026-09-11)

Прочее (вторичные, помечены в тексте):
- wgcf — <https://github.com/ViRb3/wgcf> (MTU 1280, лимит устройств, реферальные ключи)
- Описание полей trace — <https://github.com/fawazahmed0/cloudflare-trace-api>
- Community-треды Cloudflare 765494, 797012, 305444 (недоступны для автоматического чтения)
