# Как mon-client гоняет пробу через xray-core и AmneziaWG в контейнерах

Исследование по тикету [#28](https://github.com/SBKubric/3ax-ui-proxy/issues/28) (часть карты [#20](https://github.com/SBKubric/3ax-ui-proxy/issues/20)).
Дата: 2026-09-12. Ссылки на код панели даны относительно `main` (HEAD `4d097721`). Термины — по [CONTEXT.md](../../CONTEXT.md): mon-client, target, path, probe account, tunnel probe, heartbeat.

Помимо документации и исходников часть утверждений **проверена опытом** на рабочей машине (Docker 29.8, `ghcr.io/xtls/xray-core:latest` = Xray 26.3.27, `amneziavpn/amneziawg-go:latest` = amneziawg-go 0.0.20250522 + amneziawg-tools v3.1.20260812, хост Debian 12 с модулем ядра `amneziawg` 3.1.20260812, AppArmor включён). Такие места помечены «(опыт)».

## TL;DR

- **xray-core.** Один процесс xray на все xray-targets: на каждый target — socks-inbound на `127.0.0.1:<порт>` без auth, outbound `vless` + `reality`, собранный из полей share-ссылки (`sni/pbk/sid/fp/spx`, `flow`, `encryption=none`), и правило routing `inboundTag → outboundTag`. Права не нужны. Xray отвечает на SOCKS CONNECT **сразу**, а TCP+Reality-handshake начинает в ту же миллисекунду (опыт) — поэтому время туннеля попадает в фазу TLS-handshake пробы, а не в connect. Провал Reality виден в логе даже при `loglevel: warning`: `[Error] transport/internet/reality: REALITY: received real certificate (potential MITM or redirection)`; при `loglevel: info` — ещё и финальная строка `failed to process outbound traffic > … REALITY: processed invalid connection > … all retry attempts failed` (опыт). Без `mux` каждая проба = свежий Reality-handshake, что нам и нужно.
- **AmneziaWG.** Рекомендуемый путь — **in-process userspace-стек**: в `amneziawg-go` (модуль `github.com/amnezia-vpn/amneziawg-go/v3`) есть `tun/netstack` (gVisor): `netstack.CreateNetTUN` + `device.NewDevice` + `dev.IpcSet(...)` + `http.Transport{DialContext: tnet.DialContext}`. Ни `/dev/net/tun`, ни NET_ADMIN, ни маршрутов, ни /32-граблины, полная изоляция N туннелей в одном процессе, `handshake time` — из `IpcGet()` (`last_handshake_time_nsec`). Если нужен «настоящий» TUN — только ручная сборка интерфейса, как в ранбуке (`/32` к endpoint **до** `0/1`+`128/1`), потому что `awg-quick` в контейнере не работает: он падает на `sysctl net.ipv4.conf.all.src_valid_mark=1` (в форке нет guard'а, `/proc/sys` read-only, а с `systempaths=unconfined` пишет AppArmor-профиль docker-default; заводится только в `--privileged` либо с `apparmor=unconfined`+`systempaths=unconfined`) (опыт).
- **Изоляция.** xray изолирует сам (тегами). Для AWG: netstack in-process (0 прав) > контейнер на туннель (`NET_ADMIN` + `/dev/net/tun`, ручные маршруты) > netns (`SYS_ADMIN` + `apparmor=unconfined`, иначе `ip netns add` падает на `mount --make-shared /var/run/netns`) (опыт) > policy routing (наименее доказуемая изоляция; SO_MARK требует NET_ADMIN).
- **Тайминги для минутного цикла.** Все targets — параллельно. Бюджет одной пробы **20 с** (внешний `context.WithTimeout`), connect 5 с, TLS-handshake 10 с, заголовки ответа 10 с, heartbeat 10 с, джиттер старта 0–5 с, `DisableKeepAlives`. Ориентиры: Prometheus `scrape_interval 1m` / `scrape_timeout 10s`, blackbox `timeout-offset 0.5`; Xray сам ретраит dial 5 раз по 16 с (до ~80 с!) — наш таймаут обязан быть короче; WireGuard ретраит handshake каждые 5 с (+джиттер ≤334 мс) до 90 с.
- **Измерение.** Через `net/http/httptrace`: для xray «латентность» = `TLSHandshakeStart→Done` (первая сквозная фаза; включает Reality-handshake + TCP до mon-server + TLS 1.3) и `ttfb` (`WroteRequest→GotFirstResponseByte` ≈ 1 RTT через туннель); handshake ≈ `tls − ttfb` (эвристика). Для AWG (netstack): `handshake = last_handshake_time − t(первый Dial)` точно, `connect` — вручную вокруг `tnet.DialContext` (Connect-хуки httptrace для кастомного dialer не срабатывают), `tls`, `ttfb`. В 5-минутный агрегат — `n_ok/n_fail`, `min/median/max`; перцентили на 5 точках бессмысленны.

---

## 1. Что mon-client получает от панели

mon-server тянет подписку probe account как обычный клиент (решение карты #20). Два формата:

| Формат | Где | Что внутри |
|---|---|---|
| base64-список ссылок | `GET /sub/<subId>` (`sub/subController.go:98-175`) | `vless://uuid@host:port?type=tcp&encryption=none&flow=xtls-rprx-vision&security=reality&sni=…&pbk=…&sid=…&fp=…&spx=…[&pqv=…]#remark`. Reality-параметры собирает `applyShareRealityParams` (`sub/subService.go:764-793`): `sni` и `sid` — **случайный** элемент из `serverNames`/`shortIds`, `spx` — `"/"+random.Seq(15)`, `pqv` из `mldsa65Verify`. |
| JSON-подписка | `GET /json/<subId>` (`subJsonEnable`, по умолчанию `false` — `web/service/setting.go:56`) | Массив полных клиентских конфигов, по одному на inbound × externalProxy (`sub/subJsonService.go:89-234`). Шаблон `sub/default.json`: inbound `mixed` на `10808` (socks+http, sniffing on), `http` на `10809`, outbound с тегом `proxy`, `realitySettings{show:false, publicKey, fingerprint, mldsa65Verify, spiderX, shortId, serverName}` (`subJsonService.go:284-309`), `policy.levels.8.handshake: 4`. |

Вывод для контракта: mon-server должен передавать mon-client **разобранные поля** target (адрес по path, порт, uuid, flow, sni, pbk, sid, fp, spx, pqv), а не сырую ссылку — mon-client собирает один общий конфиг xray на все targets (§2.1); JSON-подписка удобна только для ручного теста (в ранбуке, §5).

## 2. Проба через xray-core

### 2.1. Конфиг: share-ссылка → outbound, socks-inbound, routing

Стандарт ссылки — [XTLS/Xray-core#716](https://github.com/XTLS/Xray-core/discussions/716). Маппинг в JSON и проверки ядра (`infra/conf/vless.go`, `transport_internet.go`, `transport_security.go`):

| Параметр ссылки | JSON | Требования |
|---|---|---|
| `uuid@host:port` | `settings.vnext[0].{address,port,users[0].id}` | ровно один `vnext` и один user |
| `encryption` | `users[0].encryption` | обязателен; `"none"` (пустая строка → ошибка `please add/set "encryption":"none"`) |
| `flow` | `users[0].flow` | `xtls-rprx-vision` только с `tcp`(`raw`) + `tls`/`reality` |
| `type` | `streamSettings.network` | `"tcp"` и `"raw"` эквивалентны (`case "raw", "tcp": return "tcp"`); Reality — только RAW/XHTTP/gRPC |
| `security` | `streamSettings.security` | `"reality"` |
| `sni` | `realitySettings.serverName` | может быть пустым → берётся адрес |
| `fp` | `realitySettings.fingerprint` | **обязателен** для Reality; `unsafe`/`hellogolang` запрещены |
| `pbk` | `realitySettings.password` (старое имя `publicKey`, оба принимаются: `if c.Password != "" { c.PublicKey = c.Password }`) | обязателен |
| `sid` | `realitySettings.shortId` | hex ≤16, чётной длины; может быть `""` |
| `spx` | `realitySettings.spiderX` | по умолчанию `/` |
| `pqv` | `realitySettings.mldsa65Verify` | опционально |

Минимальный конфиг на один target (валидирован `xray run -test` для `"network":"tcp"` и `"raw"` — опыт):

```json
{
  "log": {"loglevel": "info", "access": "none"},
  "inbounds": [
    {"tag": "in-t1", "listen": "127.0.0.1", "port": 10801, "protocol": "socks",
     "settings": {"auth": "noauth", "udp": false}}
  ],
  "outbounds": [
    {"tag": "out-t1", "protocol": "vless",
     "settings": {"vnext": [{"address": "<proxy-or-real-ip>", "port": 443,
       "users": [{"id": "<uuid>", "encryption": "none", "flow": "xtls-rprx-vision"}]}]},
     "streamSettings": {"network": "tcp", "security": "reality",
       "realitySettings": {"serverName": "<sni>", "fingerprint": "chrome",
         "publicKey": "<pbk>", "shortId": "<sid>", "spiderX": "<spx>", "show": false}}},
    {"tag": "block", "protocol": "blackhole"}
  ],
  "routing": {"domainStrategy": "AsIs",
    "rules": [{"type": "field", "inboundTag": ["in-t1"], "outboundTag": "out-t1"}]}
}
```

N targets = N пар inbound/outbound и N правил в одном процессе; без правила трафик уходит в **первый** outbound (docs: «The first element in the list serves as the primary outbound»), поэтому последним outbound'ом стоит `blackhole`, а правило — на каждый inbound. Socks-inbound принимает и HTTP CONNECT (`proxy/socks/server.go`: «Not Socks request, try to parse as HTTP request»), отдельный http-inbound не нужен. `sniffing` для пробы не нужен (если ключ не задан — выключен).

### 2.2. Запуск в контейнере

Официальный образ (`.github/docker/Dockerfile`): `distroless/static:nonroot`, `ENTRYPOINT ["/usr/local/bin/xray"]`, `CMD ["-confdir", "/usr/local/etc/xray/"]`, без shell (опыт: `docker inspect`). В образе лежит `00_log.json` (`error → /var/log/xray/error.log`, `access: none`, `loglevel: warning`). Варианты подачи конфига:

- смонтировать свой каталог в `/usr/local/etc/xray/` (тогда `00_log.json` образа не применяется — берётся наш);
- `run -c stdin:` через `docker run -i` (`main/confloader/external`: `case arg == "stdin:"`); **но** если переопределить CMD, дефолтный confdir не читается;
- `run -test -c …` → `Configuration OK.` / exit 23 при ошибке; `-dump` печатает слитый конфиг.

Несколько файлов сливаются по правилам [multiple.md](https://xtls.github.io/en/config/features/multiple.html): одинаковые `tag` перезаписываются, inbounds дописываются в конец, outbounds — в начало (кроме файлов с `tail` в имени). Для mon-client проще генерировать один файл целиком и перезапускать xray при смене ревизии targets.

Для mon-client на Go есть и вариант «xray как библиотека» (`core.New(config)` + `core.Dial`) — тогда socks не нужен вовсе; здесь не проверялся (UNVERIFIED), контейнерный xray проще.

### 2.3. Поведение SOCKS CONNECT и dial (опыт)

`proxy/socks/protocol.go` `handshake5` пишет `writeSocks5Response(writer, statusSuccess, …)` **до** `DispatchLink`; dial к серверу делает `proxy/vless/outbound` в `Process` уже после ответа. Опыт с сырым SOCKS5-клиентом и outbound'ом на «чёрную дыру» `10.255.255.1:443`:

```
CONNECT reply at 15:45:57.744997 050000017f0000012a31   ← ответ мгновенно (0.000 с)
xray log:      15:45:57.745131 dialing TCP to tcp:10.255.255.1:443   ← dial в ту же мс
first write at 15:46:01.745223                          ← клиент ещё ничего не слал
xray log:      15:46:13.746682 dialing TCP …            ← повтор через 16 с
xray log:      15:46:29.955156 dialing TCP …            ← ещё через 16 с
```

Следствия:
1. Тайминг SOCKS CONNECT ничего не измеряет; первая **сквозная** фаза пробы — TLS-handshake с mon-server, и она включает Reality-handshake (если dial ещё не завершился к моменту ClientHello), TCP от real server до mon-server и сам TLS.
2. Один dial ограничен 16 с (`transport/internet/system_dialer.go`: `net.Dialer{Timeout: time.Second * 16}`), VLESS-outbound ретраит `retry.ExponentialBackoff(5, 200)` → до 5 попыток, т.е. **до ~82 с** на мёртвый адрес. Проба обязана иметь свой таймаут (§5); клиент видит закрытие соединения (`Recv failure: Connection reset by peer` в curl), а не SOCKS-код ошибки.
3. `curl -w '%{time_connect}'` через `--socks5-hostname` при провале печатает `0.000` (опыт, curl 7.88) — фазы через curl ненадёжны, лучше Go `httptrace` (§6).

### 2.4. Как распознать провал Reality-handshake

`transport/internet/reality/reality.go` `UClient`: сертификат проверяется HMAC-подписью по `AuthKey`; если пришёл настоящий сертификат (MITM/редирект/сервер не Reality) — `uConn.Verified` остаётся false, ядро логирует `REALITY: received real certificate (potential MITM or redirection)` уровнем **Error**, запускает «паука» (реальные HTTP-запросы к SNI-сайту через тот же conn как прикрытие) и возвращает `REALITY: processed invalid connection` (Warning). Опыт с outbound'ом на `www.cloudflare.com:443` и чужим `pbk`, `loglevel: debug`, `show: true`:

```
REALITY localAddr: 192.168.1.105:50954	uConn.Verified: false
2026/09/12 15:44:06.887083 [Error] [1645185437] transport/internet/reality: REALITY: received real certificate (potential MITM or redirection)
REALITY localAddr: 192.168.1.105:50954	req.Referer(): https://www.cloudflare.com/   … len(body): 1317944   ← паук
2026/09/12 15:44:07.487999 [Info] [1645185437] transport/internet/tcp: dialing TCP to tcp:www.cloudflare.com:443   ← ретрай
…
2026/09/12 15:44:08.397455 [Info] [1645185437] app/proxyman/outbound: app/proxyman/outbound: failed to process outbound traffic > proxy/vless/outbound: failed to find an available destination > common/retry: [transport/internet/reality: REALITY: processed invalid connection] > common/retry: all retry attempts failed
```

Итого ~2.6 с до сброса соединения клиента (5 попыток с паузами 0/200/400/600/800 мс). Что использовать:

| Сигнал | Уровень | Где | Годится для |
|---|---|---|---|
| `REALITY: received real certificate (potential MITM or redirection)` | Error (виден при дефолтном `warning`) | stderr/`log.error` | диагностика «подменили/заблокировали Reality» vs «сеть» |
| `failed to process outbound traffic > … > all retry attempts failed` | Info | то же, при `loglevel: info` | итог dial с цепочкой причин (`dial tcp … i/o timeout`, `connection refused`, `REALITY: processed invalid connection`) |
| `dialing TCP to tcp:<addr>:<port>` | Info | то же | связка сессии `[id]` с target'ом: адрес+порт из этой строки = адрес target по path |
| `show: true` (`uConn.Verified: false`) | stdout, **без timestamp**, мимо логгера | stdout | только отладка руками |
| закрытие socks-соединения без данных / ошибка TLS-handshake пробы | — | сам HTTP-клиент | факт DOWN |

Рекомендация: xray с `"log": {"loglevel": "info", "access": "none"}`, stderr читает mon-client и кладёт в heartbeat последнюю строку `failed to process outbound traffic` / `REALITY: received real certificate` за окно пробы, сопоставив по `dialing TCP to <addr>:<port>` с тем же `[session-id]`. Access-log бесполезен: пишется при dispatch, до dial, и не отличает успех от провала.

### 2.5. Observatory — альтернатива внешнему запросу

`observatory{subjectSelector, probeUrl, probeInterval}` делает `GET` через outbound с `Timeout: 5s`, `TLSHandshakeTimeout: 5s` (`app/observatory/observer.go`) и отдаёт `OutboundStatus{alive, delay(ms), last_error_reason, last_seen_time, last_try_time}` по gRPC `ObservatoryService.GetOutboundStatus`; у `xray api` субкоманды для него нет — нужен свой gRPC-клиент. Таймаут фиксирован, http.Transport с keep-alive (переиспользование соединения между пробами UNVERIFIED), фаз нет. Не подходит как основной механизм; можно оставить как «второе мнение» внутри xray.

### 2.6. Свежий handshake на каждую пробу

`mux.enabled` по умолчанию `false`; без mux `app/proxyman/outbound/handler.go` вызывает `h.proxy.Process` → `dialer.Dial` на **каждое** входящее соединение. Значит, если проба не переиспользует HTTP-соединение (`DisableKeepAlives: true`), каждая минута = новый TCP + Reality-handshake до real server — ровно то, что должен проверять tunnel probe. `sockopt.tcpKeepAliveInterval` (у outbound по умолчанию 45 с, как в Chrome) на это не влияет.

## 3. Проба через AmneziaWG

### 3.1. Что в образе и что нужно контейнеру

`amneziavpn/amneziawg-go` ([Dockerfile](https://github.com/amnezia-vpn/amneziawg-go/blob/master/Dockerfile)): Alpine + `amneziawg-go` (статический), `awg` (симлинк `wg`), `awg-quick` (`wg-quick`), `iproute2`, `iptables`, `bash`; **nft нет**, `resolvconf` нет, `CMD ["/bin/sh"]` (опыт: `docker inspect`, `ls`).

Права для TUN-варианта: `--cap-add NET_ADMIN --device /dev/net/tun` (`tun/tun_linux.go` открывает `/dev/net/tun`, `TUNSETIFF`, `SIOCSIFMTU`; `ip addr/route` — NET_ADMIN). `SYS_MODULE` нужен только чтобы грузить модуль ядра из контейнера — не наш случай. Для netstack-варианта (§3.4) не нужно **ничего**: только UDP-сокет процесса.

Ядро vs userspace — три граблины (опыт):
- Если на хосте есть модуль `amneziawg`, `ip link add awg0 type amneziawg` в контейнере **проходит через модуль хоста**; `awg-quick` переключается на `amneziawg-go` только когда `ip link add` падает (`add_if()` в `linux.bash`), переменная `WG_QUICK_USERSPACE_IMPLEMENTATION` лишь называет бинарь. Контейнер становится зависим от версии модуля хоста.
- `amneziawg-go awg0` печатает баннер «Running amneziawg-go is not required because this kernel has first class support…» и **всё равно поднимает userspace-интерфейс** (`/var/run/amneziawg/awg0.sock`); переменной `WG_I_PREFER_BUGGY_USERSPACE_TO_POLISHED_KMOD` в amneziawg-go нет (`main.go`). Комментарий в ранбуке «при модуле ядра на хосте интерфейс поднимет ядро» стоит уточнить: userspace-интерфейс создаётся всегда, а `awg setconf`/`awg show` работают с ним.
- Пир, чей `PublicKey` совпадает с публичным ключом самого интерфейса, **молча игнорируется** (`device/uapi.go`: `peer.dummy = device.staticIdentity.publicKey.Equals(publicKey)`) — при генерации тестовых конфигов легко получить «пустой» `awg show`.

Env `amneziawg-go`: `LOG_LEVEL=debug|error|silent` (по умолчанию error), `WG_PROCESS_FOREGROUND=1`, `WG_TUN_FD`, `WG_UAPI_FD`.

### 3.2. Почему `awg-quick` в контейнере не работает (опыт)

`awg-quick up` с `AllowedIPs 0.0.0.0/0` идёт по `add_default()`: `awg set fwmark 51820` → `ip rule add not fwmark 51820 table 51820` → `ip rule add table main suppress_prefixlength 0` → `ip route add 0.0.0.0/0 dev awg0 table 51820` → `sysctl -q net.ipv4.conf.all.src_valid_mark=1` → `iptables-restore` (CONNMARK save/restore для UDP + anti-spoof DROP в `raw`). Скрипт под `set -e`, любая ошибка — откат.

| Запуск контейнера | Результат `awg-quick up` |
|---|---|
| `--cap-add NET_ADMIN --device /dev/net/tun` | падает: `sysctl: error setting key 'net.ipv4.conf.all.src_valid_mark': Read-only file system` (`/proc/sys` в `ReadonlyPaths` moby) |
| + `--sysctl net.ipv4.conf.all.src_valid_mark=1` | значение уже `1`, но **всё равно падает** тем же EROFS — в `amneziawg-tools` (как и в релизе wireguard-tools 1.0.20250521) нет guard'а `[[ $(sysctl -n …) -ne 1 ]] &&`, который есть в git master wireguard-tools |
| + `--security-opt systempaths=unconfined` | `Permission denied` — пишет AppArmor-профиль `docker-default` |
| + `apparmor=unconfined` + `systempaths=unconfined` | **успех**: правила `32764: from all lookup main suppress_prefixlength 0`, `32765: not from all fwmark 0xca6c lookup 51820`, таблица `51820: default dev awg0`, iptables mangle CONNMARK + raw DROP |
| `--privileged` | успех |
| с `DNS = …` в конфиге | падает раньше: `resolvconf: command not found` |

Вывод: `awg-quick` в mon-client не использовать. Если нужен TUN — ручная сборка (§3.3) или `Table = off` + свои `PostUp`.

### 3.3. Ручная сборка и порядок маршрутов (граблина `/32`)

Рецепт ранбука (`docs/runbooks/proxy-front.md`, §5) проверен и работает:

```sh
amneziawg-go awg0                       # userspace TUN (баннер про модуль ядра — не ошибка)
awg setconf awg0 <(awg-quick strip awg0)  # strip убирает Address/MTU/DNS/Table/Pre*/Post*, Jc..H4 остаются
ip addr add 10.66.66.2/32 dev awg0; ip link set mtu 1420 up dev awg0
ip route add <endpoint-ip>/32 via $GW dev eth0   # ДО /1-маршрутов
ip route add 0.0.0.0/1 dev awg0; ip route add 128.0.0.0/1 dev awg0
```

Почему именно так — [wireguard.com/netns](https://www.wireguard.com/netns/), «Overriding The Default Route»: два `/1` в сумме равны `0/0`, но более специфичны и перекрывают default-маршрут, не трогая его; endpoint при этом тоже попадает под один из `/1`, и без явного `/32 via <gw> dev eth0` шифрованный UDP уходит в сам `awg0` — петля. Порядок важен только потому, что до появления `/32` первый же пакет (handshake) уже может уйти в туннель; добавление `/32` первым исключает окно. Ядро выбирает маршрут по длине префикса, так что после установки всех трёх порядок безразличен. Вариант wg-quick с fwmark решает ту же задачу без `/32`, но требует sysctl+iptables (§3.2). В контейнере с `--network none`/своим netns default идёт через `awg0` и `/32` не нужен только если сам UDP-сокет живёт в другом netns (§4).

### 3.4. In-process: `tun/netstack` в amneziawg-go (рекомендуется)

В `amneziawg-go` (модуль `github.com/amnezia-vpn/amneziawg-go/v3`, gVisor в зависимостях) есть `tun/netstack` с `CreateNetTUN(localAddresses, dnsServers []netip.Addr, mtu int) (tun.Device, *Net, error)` и `(*Net).DialContext/DialContextTCPAddrPort/DialUDP/LookupHost`. Пример из репозитория (`tun/netstack/examples/http_client.go`):

```go
tun, tnet, err := netstack.CreateNetTUN(
    []netip.Addr{netip.MustParseAddr("10.66.66.2")}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, 1420)
dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, ""))
err = dev.IpcSet(`private_key=<hex>
jc=4
jmin=40
jmax=70
s1=15
s2=45
h1=1234567
h2=2345678
h3=3456789
h4=4567890
public_key=<hex>
allowed_ip=0.0.0.0/0
endpoint=<proxy-or-real-ip>:51820
`)
err = dev.Up()
client := http.Client{Transport: &http.Transport{DialContext: tnet.DialContext, DisableKeepAlives: true}}
```

Перевод `.conf` → UAPI: ключи в нижнем регистре (`jc, jmin, jmax, s1..s4, h1..h4, i1..i5`, плюс `header_protection_key, content_padding_addition, rekey_after_time, rekey_timeout, reject_after_time, keepalive_timeout, max_handshake_attempts, random_trailers, disable_cookies`; peer: `public_key, preshared_key, endpoint, allowed_ip, persistent_keepalive_interval`), ключи **base64 → hex**. `awg-quick strip` сам ничего не переводит — это делает `awg` (`config.c`/`ipc-uapi.h`). Ограничения при merge: диапазоны `h1..h4` не должны пересекаться (`headers must not overlap`), header protection требует `S1..S4 ≥ 12`.

Что это даёт mon-client: один `device.Device` + один `*Net` на target, ни одного маршрута в ядре, никаких capabilities, `dev.IpcGet()` для `last_handshake_time_*`/`rx_bytes`/`tx_bytes`, свой `device.Logger` (struct с `Verbosef/Errorf`) для перехвата строк `Sending handshake initiation` / `Received handshake response` / `Handshake did not complete after 5 seconds, retrying (try 2)`. Практическая проверка netstack на стенде не делалась (UNVERIFIED), но TUN-вариант в userspace с императивным `awg set` проверен (опыт): пир применяется, инициации уходят каждые 5 с, `awg show awg1 latest-handshakes` = `0`, `transfer` растёт (`403 B sent` — junk + initiation).

### 3.5. Семантика handshake и детект мёртвого туннеля

Константы (`device/constants.go`, идентичны wireguard-go и ядру): `RekeyTimeout 5s`, `RekeyAttemptTime 90s` (`MaxTimerHandshakes = 18`), `RekeyTimeoutJitterMaxMs 334`, `RekeyAfterTime 120s`, `RejectAfterTime 180s`, `KeepaliveTimeout 10s`, `CookieRefreshTime 120s`. В AWG 3 их можно переопределить (`rekey_timeout`, `max_handshake_attempts`, …). Протокол ([wireguard.com/protocol](https://www.wireguard.com/protocol/)): инициация повторяется через `REKEY_TIMEOUT + jitter`, если ответа нет; попытки прекращаются через `REKEY_ATTEMPT_TIME`; сервер на неавторизованного клиента **не отвечает вообще** — ошибки handshake не существует как события.

Поэтому единственные сигналы «туннель мёртв»: `last_handshake_time` не обновился (UAPI `get=1`: `last_handshake_time_sec/nsec`, CLI `awg show <if> latest-handshakes`, `0` = никогда) и `rx_bytes` не растёт при растущем `tx_bytes`. Лог при `LOG_LEVEL=debug`: `Handshake did not complete after 5 seconds, retrying (try N)`, `Handshake did not complete after N attempts, giving up`. Cookie: под нагрузкой сервер отвечает cookie reply, клиент повторяет с mac2 автоматически — один лишний RTT; IP-rate-limiter при 1 handshake/мин на IP не мешает.

### 3.6. Keepalive и свежий handshake на каждую пробу

`PersistentKeepalive` нужен только для NAT-mapping, «most users will not need this» (wg(8)); клиент-инициатор, шлющий запрос раз в 60 с, обходится без него. Но с постоянным device сессия живёт до 180 с, rehandshake — раз в 120 с, т.е. handshake измеряется лишь на каждой второй-третьей пробе. Чтобы мерить его **каждую** минуту (симметрично xray, §2.6) — пересоздавать device на пробу (`dev.Close()` → `NewDevice`+`IpcSet`+`Up`; в netstack это дёшево) или `Down()/Up()`. Цена — одна инициация + `Jc` junk-пакетов в минуту на target.

### 3.7. Смысл AWG-параметров

`Jc` (4–12) junk-пакетов размером `Jmin..Jmax` перед каждым handshake (только на клиенте; `Jmax` меньше MTU системы, иначе фрагментация «выглядит подозрительно»); `S1/S2/S3/S4` — паддинг init/response/cookie/transport (`len(init)=148+S1`, `len(resp)=92+S2`); `H1..H4` — типы заголовков (число или диапазон `x-y`, не пересекаются; `1/2/3/4` = выключено). Старая проверка `148+S1 ≠ 92+S2` была в v0.2.12, в v3 её нет — тип пакета определяется по H-диапазонам.

## 4. Изоляция нескольких targets на одной коробке

| Вариант | Права | Что проверено (опыт) | Оценка |
|---|---|---|---|
| **xray: один процесс, N inbound/outbound, routing по `inboundTag`** | нет | конфиг валиден; detour логируется `taking detour [out-t1] for […]` | ✅ штатно, изоляция гарантирована тегами |
| **AWG: netstack in-process (N `device.Device`)** | нет | сам netstack — по коду/примеру репозитория | ✅ рекомендуется: нет маршрутов, нет TUN, нет `/32`-граблины |
| AWG: контейнер на туннель (TUN, ручные маршруты §3.3), проба внутри того же контейнера или через `--network container:<awg>` | `NET_ADMIN` + `/dev/net/tun` | работает (рецепт ранбука) | ⚠️ N контейнеров, зависимость от модуля ядра хоста |
| AWG: netns на туннель внутри одного контейнера (`ip netns add`, `ip link set awg0 netns`) | `NET_ADMIN`+`SYS_ADMIN` **и** `apparmor=unconfined` | с `SYS_ADMIN`: `unshare -n` ok, но `ip netns add` → `mount --make-shared /var/run/netns failed: Permission denied`; с `apparmor=unconfined` — ok | ⚠️ почти privileged; Go-процессу с потоками неудобно жить в нескольких netns |
| AWG: policy routing (fwmark на сокет пробы, `ip rule fwmark N table N`) | `NET_ADMIN` (SO_MARK, socket(7)) | не проверялось | ❌ общая таблица, ошибка в одном правиле тихо пускает пробу мимо туннеля → ложный UP |

Docker-факты: каждый контейнер получает свой netns (bridge по умолчанию), `--network container:<id>` делит его, `--network none` оставляет только `lo`; `--sysctl net.*` разрешён, но с `--network host` — нет; `/proc/sys` read-only, AppArmor `docker-default` дополнительно запрещает запись в `/proc/sys/net/*` (опыт §3.2).

Итоговая форма mon-client: **один контейнер, один Go-процесс** = встроенный amneziawg-go/netstack для AWG-targets + дочерний `xray` (бинарь из официального образа, конфиг в файл, stderr в pipe) для xray-targets. Контейнеру не нужны ни capabilities, ни `/dev/net/tun`, ни особый `--network`; нужен только исходящий UDP/TCP.

## 5. Тайминги для минутного цикла

Ориентиры из первоисточников: Prometheus `scrape_interval: 1m`, `scrape_timeout: 10s` (не больше интервала); blackbox_exporter берёт таймаут из scrape_timeout минус `--timeout-offset 0.5`; Kubernetes probe `timeoutSeconds 1`, `periodSeconds 10`, `failureThreshold 3`; Consul HTTP-check `timeout 10s`; Go `DefaultTransport` — dial 30 с, TLS-handshake 10 с. Внутренние таймеры туннелей: xray dial 16 с × 5 попыток (§2.3), Reality-провал ≈ 2.6 с (§2.4); WireGuard инициация каждые 5 с + джиттер, до 90 с (§3.5).

Рекомендация (наша, не цитата):

| Что | Значение | Почему |
|---|---|---|
| Цикл | 60 с, все targets параллельно, старт с джиттером 0–5 с | последовательно N×20 с в минуту не влезает; джиттер размазывает нагрузку на mon-server |
| Бюджет пробы (`context.WithTimeout`/`Client.Timeout`) | **20 с** (максимум 30) | ≥30 с остаётся на heartbeat и агрегат даже при полном таймауте всех targets |
| `net.Dialer.Timeout` / ручной connect через `tnet.DialContext` | 5 с | для xray — loopback, мгновенно; для AWG — ровно первая попытка handshake (`RekeyTimeout`) + TCP через туннель |
| `Transport.TLSHandshakeTimeout` | 10 с | первая сквозная фаза для xray: Reality-handshake + TCP до mon-server + TLS |
| `Transport.ResponseHeaderTimeout` | 10 с | тело ответа маленькое, режется общим контекстом |
| `DisableKeepAlives: true` (или Transport на пробу) | — | иначе со второй минуты `GotConn.Reused=true`, и фаз connect/TLS нет; blackbox тоже создаёт клиент на пробу |
| Heartbeat | отдельный Transport без прокси, 10 с | |
| DOWN | N пропусков подряд (порог на mon-server; по умолчанию 3, как `failureThreshold`) | одна проба ≠ инцидент |

Нюанс xray: после нашего таймаута xray может ещё ретраить dial в фоне (до ~80 с); отменяет ли он dial при закрытии socks-соединения — UNVERIFIED. На корректность пробы это не влияет, на нагрузку — минимально (одно соединение).

## 6. Как измерять latency и handshake

### 6.1. xray (проба через `Transport.Proxy = socks5://127.0.0.1:<port>`)

Хронология хуков `net/http/httptrace` при socks-прокси (`transport.go`: `cm.addr()` возвращает адрес прокси; SOCKS CONNECT делается `socksNewDialer(...).DialWithConn` без хуков; затем `addTLS` с `TLSHandshakeStart/Done`; `GotConn` — после TLS):

| Фаза | Хуки | Что внутри | Смысл |
|---|---|---|---|
| connect | `ConnectStart→ConnectDone` | TCP к 127.0.0.1 | ничего, ~0 |
| socks | `ConnectDone→TLSHandshakeStart` | SOCKS greeting+CONNECT | ~0 (ответ мгновенный, §2.3) |
| **tls** | `TLSHandshakeStart→TLSHandshakeDone` | Reality-handshake до real server (если ещё не завершён) + TCP real server→mon-server + TLS 1.3 с mon-server | **latency туннеля** — первый сквозной сигнал; при провале Reality здесь ошибка |
| **ttfb** | `WroteRequest→GotFirstResponseByte` | 1 RTT через туннель + время mon-server | RTT-метрика |
| total | старт→конец тела | | `probe_duration` |

`handshake ≈ tls − ttfb` — эвристика (TLS 1.3 handshake ≈ 1 RTT ≈ ttfb); точного timestamp завершения Reality-handshake xray не пишет (`show: true` без времени, §2.4). Если нужна точность — вариант «xray как библиотека» с собственным замером вокруг dial (UNVERIFIED).

### 6.2. AmneziaWG (netstack)

- **handshake**: `t0` перед первым `tnet.DialContext` (первый пакет ставит инициацию в очередь), затем `dev.IpcGet()` до изменения `last_handshake_time_sec/nsec` (наносекунды есть; опрос с шагом ~50 мс или один раз после успешного connect) — `handshake = t_hs − t0`. Точнее, чем у xray, потому что событие есть в UAPI. С TUN в ядре — `awg show <if> latest-handshakes` (секунды) или `dump`.
- **connect (RTT)**: `ConnectStart/Done` для кастомного `DialContext` **не срабатывают** (`net.Dialer` читает `nettrace` из контекста, gVisor — нет) — мерить вручную вокруг `tnet.DialContext`: TCP-connect через туннель = 1 RTT после handshake.
- **tls**, **ttfb** — как у xray, хуки работают (TLS делает сам Transport).
- Если device пересоздаётся на пробу (§3.6), handshake измеряется каждую минуту; иначе — только на первом/восстановительном.

### 6.3. curl (если проба — shell-out)

`--connect-timeout` покрывает DNS+TCP+TLS, `--max-time` — всё; `-w` даёт `time_namelookup/connect/appconnect/pretransfer/starttransfer/total`; через `socks5h://` `time_connect` — до прокси (по документации; в опыте при провале печатает `0.000`), сквозная фаза — `time_appconnect`. Для AWG/netstack curl неприменим вовсе. Рекомендуется Go `httptrace`.

### 6.4. Агрегат за 5 минут

На 5 сэмплах перцентили вырождаются (p95 = max); Prometheus прямо предупреждает, что усреднять квантили «statistically nonsensical». Отдавать `n_ok / n_fail`, `min / median / max` для `tls`(latency) и `ttfb`, `handshake` (min/median/max по тем пробам, где он измерен), а перцентили считать на mon-server по сырым значениям за длинные окна.

## 7. Итоговая рекомендация

1. mon-client — один Go-процесс в одном контейнере без capabilities: встроенный `amneziawg-go/v3` + `tun/netstack` для AWG-targets, дочерний `xray` (бинарь из `ghcr.io/xtls/xray-core`, конфиг-файл, `loglevel: info`, `access: none`, stderr в pipe) для xray-targets.
2. mon-server отдаёт mon-client разобранные поля target (не сырые ссылки); mon-client генерирует один xray-конфиг (socks-inbound на target, routing по тегу, `blackhole` последним) и один `IpcSet` на AWG-target (base64→hex, ключи AWG в нижнем регистре).
3. Проба — HTTPS `GET` на mon-server через `http.Transport` (socks5 proxy для xray / `tnet.DialContext` для AWG), `DisableKeepAlives`, `httptrace`; бюджеты 20/5/10/10 с; targets параллельно; DOWN после N пропусков.
4. Диагностика в heartbeat: для xray — последняя строка `failed to process outbound traffic…`/`REALITY: received real certificate…`, сопоставленная по `dialing TCP to <addr>:<port>`; для AWG — `last_handshake_time`, `tx/rx_bytes`, число попыток из логгера.
5. Handshake каждую минуту: xray — автоматически (mux off); AWG — пересоздание device на пробу.
6. `awg-quick`, netns и policy routing в контейнере не использовать; если когда-нибудь понадобится TUN — ручной рецепт ранбука с `/32` первым.

## 8. Открытые вопросы / UNVERIFIED

- Практический прогон `tun/netstack` из `amneziawg-go/v3` против стенда (пример и API есть в репозитории, но живой AWG-сервер в опыте не участвовал).
- Отменяет ли xray фоновые ретраи dial при закрытии socks-соединения клиентом.
- Переиспользует ли Observatory соединение между пробами; Observatory не рекомендован, вопрос теоретический.
- Вариант «xray как Go-библиотека» (`core.New`/`core.Dial`) для точного замера Reality-handshake.
- Уточнить комментарий в `docs/runbooks/proxy-front.md` §5 про «интерфейс поднимет ядро» (§3.1) — отдельная правка ранбука.

## Источники

- Xray share-link стандарт: https://github.com/XTLS/Xray-core/discussions/716
- Xray docs: [vless outbound](https://xtls.github.io/en/config/outbounds/vless.html), [reality](https://xtls.github.io/en/config/transports/reality.html), [socks inbound](https://xtls.github.io/en/config/inbounds/socks.html), [http inbound](https://xtls.github.io/en/config/inbounds/http.html), [routing](https://xtls.github.io/en/config/routing.html), [outbound/mux](https://xtls.github.io/en/config/outbound.html), [log](https://xtls.github.io/en/config/log.html), [observatory](https://xtls.github.io/en/config/observatory.html), [multiple configs](https://xtls.github.io/en/config/features/multiple.html), [command](https://xtls.github.io/en/document/command.html), [install/docker](https://xtls.github.io/en/document/install.html)
- Xray-core исходники (main): `infra/conf/vless.go`, `infra/conf/transport_internet.go`, `infra/conf/transport_security.go`, `proxy/socks/protocol.go`, `proxy/socks/server.go`, `proxy/http/server.go`, `proxy/vless/outbound/outbound.go`, `app/proxyman/outbound/handler.go`, `transport/internet/reality/reality.go`, `transport/internet/system_dialer.go`, `common/retry/retry.go`, `app/observatory/observer.go`, `main/confloader/external/external.go`, `.github/docker/Dockerfile`
- amneziawg-go: [README](https://github.com/amnezia-vpn/amneziawg-go/blob/master/README.md), [Dockerfile](https://github.com/amnezia-vpn/amneziawg-go/blob/master/Dockerfile), `main.go`, `device/uapi.go`, `device/constants.go`, `device/timers.go`, `tun/tun_linux.go`, `tun/netstack/tun.go`, `tun/netstack/examples/http_client.go`
- amneziawg-tools: `src/wg-quick/linux.bash` (`add_if`, `add_default`, `cmd_strip`), `src/config.c`, `src/ipc-uapi.h`
- WireGuard: [protocol](https://www.wireguard.com/protocol/), [netns](https://www.wireguard.com/netns/), [xplatform UAPI](https://www.wireguard.com/xplatform/), [wg(8)](https://git.zx2c4.com/wireguard-tools/about/src/man/wg.8), [wg-quick(8)](https://git.zx2c4.com/wireguard-tools/about/src/man/wg-quick.8), wireguard-tools `linux.bash` (git master, с guard'ом sysctl)
- Amnezia docs: https://docs.amnezia.org/documentation/amnezia-wg/
- Docker: [runtime privilege & capabilities](https://docs.docker.com/engine/containers/run/#runtime-privilege-and-linux-capabilities), [docker run --sysctl / --security-opt](https://docs.docker.com/reference/cli/docker/container/run/), [networking](https://docs.docker.com/engine/network/), [none driver](https://docs.docker.com/engine/network/drivers/none/), moby `oci/defaults.go` (`ReadonlyPaths`)
- Linux man: [capabilities(7)](https://man7.org/linux/man-pages/man7/capabilities.7.html), [unshare(2)](https://man7.org/linux/man-pages/man2/unshare.2.html), [socket(7) SO_MARK](https://man7.org/linux/man-pages/man7/socket.7.html), [ip-netns(8)](https://man7.org/linux/man-pages/man8/ip-netns.8.html), [ip-rule(8)](https://man7.org/linux/man-pages/man8/ip-rule.8.html)
- Тайминги: [Prometheus configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/), [blackbox_exporter README](https://github.com/prometheus/blackbox_exporter/blob/master/README.md), `prober/handler.go`, `prober/http.go`, [Kubernetes Probe API](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#Probe), [Consul checks](https://developer.hashicorp.com/consul/docs/services/configuration/checks-configuration-reference), [Prometheus histograms](https://prometheus.io/docs/practices/histograms/)
- Go: [net/http/httptrace](https://pkg.go.dev/net/http/httptrace), [net/http Transport/Client](https://pkg.go.dev/net/http), [net.Dialer](https://pkg.go.dev/net#Dialer), [x/net/proxy](https://pkg.go.dev/golang.org/x/net/proxy), `net/http/transport.go` (`addTLS`, socks5)
- curl: [write-out](https://curl.se/docs/manpage.html#-w), `connect-timeout`, `max-time`, `socks5-hostname`
- Панель: `sub/subService.go`, `sub/subJsonService.go`, `sub/default.json`, `web/service/setting.go`, `docs/runbooks/proxy-front.md`
