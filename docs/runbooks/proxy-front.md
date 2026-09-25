# Runbook: цепочка proxy front'ов (dokodemo relay → xray → WARP)

Как поднять и проверить **цепочку**: **real server** с панелью 3AX-UI (форк с proxy-фичей), у которого весь исходящий трафик уходит через встроенный WARP, и один или несколько одноразовых **proxy front**'ов — **звеньев** (hop), — которые клиенты видят вместо real server. Термины — по [CONTEXT.md](../../CONTEXT.md); спека — [docs/spec/proxy-chain.md](../spec/proxy-chain.md); решение о документе цепочки и join-токене — [ADR 0003](../adr/0003-chain-document-and-join-token.md). Историческая схема с relay manifest и setup page (она же — всё, что в этом runbook'е раньше называлось «вставить манифест») отменена: [ADR 0001](../adr/0001-relay-manifest-via-setup-page.md).

Стенд (§10 спеки) — `real ← bridge (inner) ← proxy (edge)`:

```
клиент ──VLESS-Reality / AmneziaWG──▶  proxy   ──dokodemo (L4)──▶  bridge  ──dokodemo (L4)──▶ real server ──warp──▶ интернет
        (в конфиге только IP edge)   edge front  (ключей нет)    inner front  (ключей нет)   (TLS/Reality, AWG, панель)
```

Каждое звено знает **только своего next hop**: `proxy` знает `bridge`, `bridge` знает `real`. Адреса глубже в документе цепочки звену не видны — в этом весь смысл усечения. Цепочка из одного звена — частный случай: единственное звено сразу `edge`, его next hop — панель.

## 1. Предусловия

- **VPS по числу звеньев + панель.** На стенде: `real` — Ubuntu 24.04, 870 MB RAM (панель + xray + awg ≈ 400 MB used); `bridge` — Debian 13, 1 vCPU, 380 MB RAM; `proxy` — Debian 13, 1 vCPU. IPv6 необязателен.
- **Порты наружу и между соседями:** 443 tcp+udp (VLESS-Reality), 51820 udp (AmneziaWG), 2096 tcp (sub-порт: подписки, `/chain/v1/*`, join page), порт панели (только админу). Провайдерский файрвол не должен их резать.
- **Порт 80 свободен на каждом боксе** — установщик берёт его под ACME-челлендж IP-сертификата (§3.3), и он же нужен каждые несколько дней для автопродления. Если цепочка релеит 80-й порт через бокс, IP-сертификат на нём не выпустится и не продлится: ставьте такой бокс с `PROXY_TLS=manual` или `none`.
- **Сервер подписок панели включён** (`subEnable`): ручки цепочки `/chain/v1/*` живут именно на нём, и без него ни одно звено не войдёт и не получит волну. Если в реестре есть звенья, а `subEnable` выключен, панель пишет WARN при старте.
- **Если на панели задан `subDomain`**, валидатор домена sub-сервера срабатывает **раньше** маршрутов цепочки. Тогда внутреннее звено обязано ходить на панель именно по этому домену: `PROXY_NEXT_HOP=<subDomain>`, а не по голому IP.
- **Доступ:** SSH-хосты `real`, `bridge`, `proxy` в `~/.ssh/config` рабочей машины.
- **Релиз форка.** Бинарники ставятся штатным `install.sh` **из форка** (`https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh`); `XUI_REPO` в нём по умолчанию `SBKubric/3ax-ui-proxy`. Схема тегов — `v<upstream>.<N>` без дефиса (`v1.8.1.5`): тег с дефисом GitHub считает pre-release, и `releases/latest` его не отдаёт.
- **На рабочей машине:** `docker` (для e2e-клиентов: `ghcr.io/xtls/xray-core`, `amneziavpn/amneziawg-go`, `golang:1.26` для тестов). Rootless podman без subuid/subgid образы не распаковывает.
- **Debian-бокс без curl:** `apt-get update && apt-get install -y curl` — без `update` установка тихо падает.

## 2. Real server

### 2.1 Установка панели

```bash
ssh real 'bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)'
x-ui settings          # порт, webBasePath, сертификат и Access URL
```

`x-ui settings` (он же `/usr/local/x-ui/x-ui setting -show true`) **паролей не печатает** — только порт, `webBasePath`, пути к сертификату и Access URL. Без TTY установщик генерирует случайные креды и печатает их **один раз, в конце установки**; если вывод потерян, их не «показывают», а задают заново: `/usr/local/x-ui/x-ui setting -username <u> -password <p>`.

Грабли:
- **ACME-клиент и на панели ходит по IPv4** — и для доменного сертификата, и для IP: зависает именно запрос к CA, а не выбор идентификатора. IPv6 включается явно, `XUI_TLS_IPV6=1` (в режиме прокси тот же переключатель — `PROXY_TLS_IPV6=1`).
- **Let's Encrypt для IP проигрывает гонку за порт 80** апстримному `install_nginx`: панель откатывается на self-signed (`/root/cert/self-signed/`). Для стенда достаточно; звено ходит к панели с `InsecureSkipVerify`.
- Свежая установка ставит `nginxMode=shared`, и панель отвергает inbound на 443 («Port already exists»). Стенд живёт **без** nginx-режима «всё за 443»:

```bash
ssh real 'sqlite3 /etc/x-ui/x-ui.db "update settings set value=\"off\" where key=\"nginxMode\";" && systemctl restart x-ui'
```

- Для AWG с «Route via Xray» нужен `iptables` (на Ubuntu 24.04 его может не быть): `apt-get install -y iptables`.
- Включите JSON-подписку: Settings → Subscription → `subJsonEnable` (без неё звено отдаёт 502 на `/json`).
- Проверьте `subEnable`: без сервера подписок цепочка не работает вообще (см. предусловия).

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

### 2.4 Реестр цепочки и relayed ports

Реестр живёт в **Settings → Subscription → Chain**: список звеньев с именем, ролью (`inner`/`edge`), хостом, порядком и признаком активного edge. Панель сама считает, какие порты звенья должны релеить, — ничего вставлять руками не нужно, файла манифеста на боксах больше нет.

Посмотреть тот самый список портов, который уедет в документ цепочки:

```bash
ssh real 'x-ui chain ports'                       # в stdout
ssh real 'x-ui chain ports -o /root/chain-ports.json'
```

> Обходной путь, пока не выехал фикс: команда читает базу по относительному пути, поэтому запускать её надо из каталога установки — `ssh real 'cd /usr/local/x-ui && ./x-ui chain ports'`.

Это debug-экспорт: `port`, `network`, `tag`, `source` (`xray` — из inbound'ов панели; `awg`/`wg`/`mtproto`/`extra` — то, что панель обслуживает вне xray). Ключей в нём нет: `grep -c 'privateKey\|password\|"id"' /root/chain-ports.json` → 0.

Порты вне xray (AmneziaWG, WireGuard, MTProto, что угодно ещё) добавляются в настройке реестра `chainExtraPorts` — не на боксе.

## 3. Введение звена в цепочку

Порядок всегда **изнутри наружу**: сначала панель, потом ближайшее к ней inner, потом следующее, и последним — edge. Звено не может войти раньше своего next hop: join пробрасывается внутрь до панели, и пока путь не собран, входить некуда.

### 3.1 Шаг на панели: завести звено и получить join-токен

Settings → Subscription → **Chain** → создать звено: `name`, `role` (`inner` или `edge`), `host`. Для первого inner next hop — сама панель; для edge панель вычисляет next hop сама (последний inner). В ответ панель **один раз** показывает **join-токен**: 32 символа, одноразовый, живёт 24 часа. Второй раз она его не покажет — просрочился или потерялся, жмите «перевыпустить токен».

### 3.2 Шаг на боксе: установка

Два пути, оба поддержаны.

**С токеном (неинтерактивно, рекомендуется):**

```bash
ssh bridge 'XUI_PROXY_MODE=1 \
PROXY_NEXT_HOP=<ip real или subDomain панели> PROXY_NEXT_HOP_SUB_PORT=2096 \
PROXY_JOIN_TOKEN=<токен из п.3.1> \
PROXY_SUB_PORT=2096 \
bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)'
```

Установщик пишет `/etc/x-ui/proxy.json` v2, выпускает TLS (§3.3), **входит в цепочку до старта сервиса** (`x-ui chain rejoin` под капотом) и только потом поднимает `x-ui`. Join page при этом не поднимается вообще. Футер печатает `Joined the chain:` и вывод `x-ui chain status`.

**Через join page (если токена под рукой нет):** запустите то же самое без `PROXY_JOIN_TOKEN`. Бокс стартует в bootstrap-режиме: релея нет, слушает только sub-порт и отдаёт одноразовую страницу входа. Футер печатает ссылку; повторно её покажет `x-ui chain join-url` (файл `/etc/x-ui/chain-join.url`). На странице — next hop (предзаполнен) и поле join-токена. После принятого входа страница отвечает 404, файл ссылки исчезает, релей и полный sub-сервер стартуют в том же процессе.

Переменные установщика — в [README](../../README.md#11-proxy-chain-anti-blocking). Коротко: `PROXY_NEXT_HOP` (обязательна), `PROXY_NEXT_HOP_SUB_PORT` (2096), `PROXY_NEXT_HOP_SCHEME` (https), `PROXY_JOIN_TOKEN`, `PROXY_TLS` (`letsencrypt-ip`), `PROXY_TLS_IPV6` (выкл.), `PROXY_DOMAIN`, `PROXY_SUB_PORT` (2096), `PROXY_SUB_LISTEN`, `PROXY_RELAY_LISTEN` (`::`), `PROXY_CERT`/`PROXY_KEY` (только при `PROXY_TLS=manual`).

Старые переменные (`PROXY_UPSTREAM_HOST`, `PROXY_UPSTREAM_BASE`, `PROXY_EXTRA_PORTS`, `PROXY_RELAY_MANIFEST`, `PROXY_SUB_PATH`, `PROXY_JSON_PATH`, `PROXY_XRAY_CONFIG`) установщик **отвергает с ошибкой**, а не игнорирует: молча проглоченный `PROXY_EXTRA_PORTS` дал бы фронт без половины портов, и выяснилось бы это только на клиенте.

**Переустановка поверх существующего бокса без TTY:** ответьте `2` на вопрос «already installed»: `printf '2\n' | XUI_PROXY_MODE=1 … bash <(curl …)`. Иначе установщик уходит в путь обновления.

### 3.3 TLS на звене: IP-сертификат Let's Encrypt

По умолчанию (`PROXY_TLS=letsencrypt-ip`) установщик выпускает сертификат Let's Encrypt **на IP-адрес бокса** — домен для этого не нужен, а у свежего одноразового фронта его обычно и нет. Выпуск идёт через acme.sh (тот же клиент, что и на панели), standalone-челлендж на порту 80, профиль `shortlived`. Такой сертификат живёт **≈ 6 дней**; acme.sh продлевает его из своего cron с интервалом 3 дня и перезапускает `x-ui` хуком. Короткий срок здесь — плюс: сертификат живёт не дольше самого бокса.

Пути ложатся в `cert`/`key` `proxy.json`: `/root/cert/ip/fullchain.pem`, `/root/cert/ip/privkey.pem`.

Грабли:
- **Порт 80 должен быть свободен и снаружи достижим** — и при выпуске, и при каждом продлении. Установщик проверяет занятость и при неудаче печатает WARN. Если цепочка релеит через этот бокс 80-й порт, IP-сертификат обречён: ставьте `PROXY_TLS=manual` с собственными `PROXY_CERT`/`PROXY_KEY` либо `none`.
- **Бокс за NAT** IP-сертификат не получит.
- **ACME-клиент ходит по IPv4** (`--listen-v4 --request-v4`): на боксе без глобального IPv6 холодный dual-stack-коннект к CA съедает весь таймаут curl, и acme.sh сдаётся с `Cannot init API`. IPv6 включается явно: `PROXY_TLS_IPV6=1`.
- **Повторить выпуск, не переустанавливая бокс** (установщик печатает эту же строку в WARN):
  ```bash
  ~/.acme.sh/acme.sh --issue -d <ip бокса> --standalone --server letsencrypt \
    --listen-v4 --request-v4 --certificate-profile shortlived --days 3 --httpport 80 --force
  ```
  затем прописать получившиеся пути в `cert`/`key` файла `/etc/x-ui/proxy.json` и `systemctl restart x-ui`.
- **Выпуск не удался — установка не падает.** Бокс поднимается без TLS, join page отдаётся по **HTTP** с баннером «токен уйдёт открытым текстом». Это не блокировка: во время установки одноразового фронта эта страница часто и есть единственный работающий канал. Но если TLS нет, лучше переустановить бокс с `PROXY_JOIN_TOKEN` через ssh, чем вводить токен в HTTP-страницу.
- `PROXY_TLS=none` — осознанный отказ от TLS (бокс за внешним терминатором, стенд, отладка). `manual` — прежнее поведение: сертификат ваш, продление ваше; без `PROXY_CERT`/`PROXY_KEY` или с нечитаемым/не тем файлом установщик останавливается, ничего не тронув.
- **Переустановка бокса с живым IP-сертификатом** его не перевыпускает: если в `/root/cert/ip` лежит сертификат на IP бокса, действующий ещё больше суток, и acme.sh его продлевает, установщик берёт его как есть — лимит дубликатов Let's Encrypt (5 за 168 ч) не тратится.

### 3.4 Проверка звена

```bash
ssh bridge 'x-ui chain status'
# name:      bridge (inner)
# next hop:  <ip real>:2096 (reachable: true)
# revision:  42
# relay:     running=true ports=[443 51820]
# last wave: …
ssh bridge 'systemctl is-active x-ui; ss -ltnup | grep -E ":443 |:51820 |:2096 "; journalctl -u x-ui -n 5 --no-pager'
ssh bridge 'grep -RIl "privateKey\|PrivateKey" /etc/x-ui /usr/local/x-ui; ls -la /etc/x-ui'   # ключей нет; proxy.json 0600, chain/ 0700
```

Ожидаем в логе: `joined chain as "bridge" (inner), next hop <ip real>:2096, revision 42` и `relaying ports [443 51820] -> <ip real> via dokodemo-door (L4 passthrough)`. Порт 12345 (TPROXY) на звене открываться **не должен**.

На панели: звено в состоянии `joined`, свежий `last_seen_at`, `lastRevision` совпадает с текущей ревизией реестра.

Следующее звено снаружи ставится тем же порядком, с `PROXY_NEXT_HOP` = адрес только что введённого звена.

## 4. Host override: активное edge

Если на панели уже был включён старый host override, миграция реестра заводит одно звено **с именем `legacy`** (роль `edge`, хост — прежний адрес прокси) и делает его активным: подписки продолжают указывать туда же, куда и до обновления, а цепочки за ним ещё нет. Это имя, а не роль: звено так и называется `legacy`, пока владелец его не переименует или не удалит. По §10 оно удаляется после того, как настоящее edge вошло в цепочку и стало активным.

Активный edge выбирается **в реестре цепочки**, а не отдельным переключателем: Settings → Subscription → *Chain* → отметить звено активным, либо `/proxy <name>` в Telegram-боте (`/proxy` — список звеньев с ролями и состоянием, `/proxy off` — выключить override). После этого подписки, JSON и AWG-конфиги (`Endpoint = <edge-ip>:51820`) указывают на активное edge; заголовок `Profile-Web-Page-Url` звено переписывает на себя.

Переключение между двумя уже вошедшими edge не требует ни входа, ни выхода: обе коробки уже релеят и качают подписки, меняется только то, какой хост панель подставляет в новые конфиги. Уже выданные конфиги продолжают ходить через прежнее edge, пока оно живо.

Грабли: сохранение настроек в панели — **полная замена** всех полей состоянием открытой страницы. После изменений через API или бота перезагрузите страницу настроек, прежде чем что-то сохранять, иначе реестр откатится.

## 5. Переустановка бокса

Порядок для стенда: панель остаётся real server, новый VPS `bridge` ставится с нуля как inner, а нынешний `proxy` переустанавливается как edge. Тот же порядок годится и для цепочки из одного звена.

1. **На панели:** Settings → Subscription → *Chain* → завести звено (`name`, `role`, `host`), получить **join-токен** (24 ч, одноразовый).
2. **На боксе:** `systemctl stop x-ui` — релей останавливается, клиенты на время переустановки идут мимо. Если есть второе edge, заранее переключите на него активность.
3. Сохранить старый конфиг: `cp /etc/x-ui/proxy.json /root/proxy.json.v1`.
4. Удалить legacy-артефакты: `rm -f /etc/x-ui/proxy.json /etc/x-ui/relay-manifest.json /etc/x-ui/proxy-setup.url`.
5. Переустановить в режиме звена — команда из §3.2, с `PROXY_NEXT_HOP` = адрес next hop (`real`/`subDomain` для inner, адрес последнего inner для edge). Без `PROXY_JOIN_TOKEN` установщик напечатает ссылку join page.
6. Проверить по §3.4: `x-ui chain status` → ревизия совпадает с панелью; `ss -ltnup | grep -E ":(443|51820|2096)"`; в реестре звено `joined` со свежим `last_seen_at`.
7. Убедиться, что ключей на боксе нет: `grep -RI "privateKey\|password" /etc/x-ui /usr/local/x-ui/bin || echo clean`. Флаг `-I` обязателен: без него grep находит обе строки внутри `geosite.dat`/`geoip.dat` и отчитывается о «ключах», которых нет.
8. Если бокс — edge: отметить его активным (§4) и проверить подписку клиента.
9. Следующее звено снаружи переустанавливается тем же порядком.

## 6. Удаление звена

Плановый вывод звена из цепочки (§4.5 спеки) делается **только в панели**: Settings → Subscription → *Chain* → удалить звено. Панель в одной транзакции перецепляет внешнего соседа удаляемого на его next hop, сжимает порядок оставшихся inner'ов и бампает ревизию. Активное edge удаляется только через `force` и только последним.

Волна доносит новый `nextHop` до внешнего соседа за ≤ `chainPollSeconds` (по умолчанию 30 с), он перезапускает relay и начинает ходить мимо удалённого.

`x-ui chain rejoin` при этом **не нужен**: перецепку делает волна, секрет и имя соседа не менялись.

**Заведение звена ревизию не двигает** (§2.6.3): пока звено `pending`, документа оно не меняет — его в цепочке ещё нет, менять нечего. Ревизия бампается ровно один раз, в момент успешного join. Поэтому после «создать звено» не ждите роста `chainRevision` — ждите его после того, как бокс вошёл.

**Когда гасить сам бокс.** Ответ на удаление несёт `safeToPowerOffWhen: {hop, revision}`, и в реестре у внешнего соседа видно `lastRevision`. Дождитесь, пока сосед покажет ревизию ≥ указанной, — до этого момента удалённый бокс ещё держит старые соединения, и выключать его рано. Отдельного состояния «draining» нет: панель боксы не гасит, гасит их человек.

Проверка на соседе: `x-ui chain status` → `next hop` стал следующим внутрь, `relay: running=true`.

## 7. Смерть inner'а

Симптом: внешний сосед мёртвого inner'а перестал получать документ, помечает себя `stale`, но **продолжает релеить** на мёртвый адрес — трафик уже не идёт, а починить конфигурацию некому. Сам он выправиться не может принципиально: он не знает ничего глубже своего next hop, а next hop мёртв. Процедура ручная (§4.6 спеки).

1. **В панели:** удалить мёртвое звено из реестра. Реестр перецепляет внешнего соседа на следующее внутрь звено и бампает ревизию.
2. **В панели:** перевыпустить join-токен внешнему соседу. Он перейдёт в `pending`; новый вход и вернёт ему связь. Без этого шага шаг 3 не пройдёт: старый токен одноразовый и сгорел при первом входе.
3. **На боксе внешнего соседа** (ssh):

   ```bash
   x-ui chain rejoin --next-hop <host нового next hop> [--sub-port 2096] [--scheme https] --token <перевыпущенный токен>
   systemctl restart x-ui
   ```

   Команда переписывает `nextHop` в `proxy.json`, стирает `hopSecret`, выполняет join на указанный адрес, записывает полученные секрет и документ. Флаги обязательны: угадывать новый next hop боксу неоткуда. Подтверждения не спрашивает — токен и так одноразовый и выпущен минуту назад именно для этого бокса.
4. Проверить на боксе: `x-ui chain status` → `stale` нет, `relay: running=true`, `next hop … (reachable: true)`; в реестре у звена свежий `lastRevision`.
5. Всё, что снаружи от вылеченного звена, чинится волной само — никуда больше ходить не нужно.

Голый `404` в ответ на join означает одно из: токен неизвестен, просрочен, уже израсходован или звено уже `joined`. Различать эти случаи вошедшему не положено — перевыпустите токен и повторите.

## 8. E2E-чеклист (шесть критериев приёмки)

Клиенты запускаются на рабочей машине в docker; `<edge-ip>` — адрес активного edge (`proxy`).

**VLESS через цепочку (критерии 1, 2, 4):**

```bash
curl -s -H 'User-Agent: v2rayNG/1.9' https://<edge-ip>:2096/json/<subId> -k -o sub.json
grep -c '"address": "<edge-ip>"' sub.json; grep -cE '<real-ip>|<bridge-ip>' sub.json     # 1 / 0
docker run --rm -v $PWD/sub.json:/etc/xray/sub.json:ro ghcr.io/xtls/xray-core:latest run -test -c /etc/xray/sub.json   # Configuration OK
docker run -d --name xray-e2e --network host -v $PWD/sub.json:/etc/xray/sub.json:ro ghcr.io/xtls/xray-core:latest run -c /etc/xray/sub.json
curl -s https://cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp)='                          # с машины: warp=off
curl -s --socks5-hostname 127.0.0.1:10808 https://cloudflare.com/cdn-cgi/trace | grep -E '^(ip|warp|colo)='   # warp=on, ip Cloudflare
docker rm -f xray-e2e
```

Ни адрес `real`, ни адрес `bridge` в подписке появляться не должны — клиент видит только edge (критерий 2). Пока трафик идёт, на `real` `ss -tn | grep ':443 '` показывает только `<bridge-ip>`, а на `bridge` — только `<edge-ip>` (критерий 4). Access-log на панели по умолчанию выключен.

**AmneziaWG через цепочку, UDP (критерии 4, 5):** конфиг клиента — из панели (`GET /panel/api/awg/client/<id>/config`, поле `obj`), в нём `Endpoint = <edge-ip>:51820`.

```bash
docker run -d --name awg-e2e --cap-add NET_ADMIN --device /dev/net/tun \
  -v $PWD/awg0.conf:/etc/amnezia/amneziawg/awg0.conf:ro --entrypoint sleep amneziavpn/amneziawg-go infinity
docker exec awg-e2e sh -c '
GW=$(ip route | awk "/default/ {print \$3; exit}")
awg-quick strip /etc/amnezia/amneziawg/awg0.conf > /tmp/awg0.conf
amneziawg-go awg0 2>/dev/null || true          # при модуле ядра на хосте интерфейс поднимет ядро
awg setconf awg0 /tmp/awg0.conf
ip addr add 10.66.66.2/32 dev awg0; ip link set mtu $(awk '/^MTU/ {print $3}' /etc/amnezia/amneziawg/awg0.conf) up dev awg0   # MTU из конфига: 1420 − S4
ip route add <edge-ip>/32 via $GW dev eth0     # ОБЯЗАТЕЛЬНО до /1-маршрутов, иначе туннель заворачивается сам в себя
ip route add 0.0.0.0/1 dev awg0; ip route add 128.0.0.0/1 dev awg0
echo "nameserver 1.1.1.1" > /etc/resolv.conf; sleep 3
awg show awg0 latest-handshakes
wget -qO- https://cloudflare.com/cdn-cgi/trace | grep -E "^(ip|warp|colo)="   # warp=on
nslookup example.com 1.1.1.1 | head -3                                          # UDP-DNS через туннель'
docker rm -f awg-e2e; shred -u awg0.conf
```

На `real` `awg show awg0 endpoints` показывает `<bridge-ip>:<port>` (а не адрес клиента и не адрес edge); в xray-логе панели при `loglevel=info` — `[awg-tproxy-in -> warp]`. `awg-quick` в контейнере падает на sysctl (`/proc/sys` read-only) — поэтому интерфейс собирается вручную.

**Состав портов (волна):** добавьте на панели inbound на новом порту → за ≤ 2 × `chainPollSeconds` порт открыт и на `bridge`, и на `proxy` (`ss -ltnup`); выключите или удалите inbound — порт закрылся на обоих.

**Звено без ключей (критерий 3):** §3.4.

**Telegram (критерий 6):** `/proxy` в боте показывает список звеньев с ролями и состоянием; проверяет владелец.

## 9. Обновления

- **real:** `bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/update.sh) </dev/null` — проверено 1.8.1.1 → 1.8.1.3 → 1.8.1.4: xray держит 443, `nginxMode=off` и реестр сохраняются.
- **звено:** тот же `update.sh`; бокс определяется по `/etc/x-ui/proxy.json`. `proxy.json`, `/etc/x-ui/chain/`, сертификат и unit не трогаются.
- **v1-конфиг на боксе останавливает обновление.** Если в `proxy.json` остался ключ манифестной эпохи (`upstreamHost`, `relayManifestPath`, `extraPorts`, `upstreamBase`, `subPath`, `jsonPath`) или нет маркера `"version": 2`, `update.sh` печатает сообщение и выходит **до подмены бинаря**: бокс продолжает релеить на той версии, которая на нём уже стоит. Лечится переустановкой по §5, а не правкой файла руками — секрет и порты цепочка выдаёт только через join.
- Бокс с v2-конфигом, но без `hopSecret`, обновляется штатно; в конце напоминание про `x-ui chain join-url`.
- Панельная «проверка обновлений» и кнопка обновления смотрят на форк (`SBKubric/sane-3x-ui`) в канале текущей версии: сборка с суффиксом (`v1.9.0-chain.N`) видит и pre-release'ы, стабильная — только стабильные; кнопка ставит ровно показанный тег через `update.sh` форка и не откатывает на более старую версию.

## 10. Известные ограничения и открытые вопросы

Вне рамок стенда: PROXY protocol / реальные client-IP за цепочкой (IP-лимит и IP-лог видят IP соседнего звена); nginx-режим «всё за 443» на real; WARP+; Hysteria2.

Не решено (см. карту #2 и #68): гонка LE за порт 80 в `install.sh` на панели; продление IP-сертификата на боксе, который релеит 80-й порт; дефолт `noKernelTun` в WARP-модалке; дефолтный Reality `dest` в GUI; прятать ли веб-UI панели с публичного IP; общие разделы README всё ещё ссылаются на апстримный `install.sh`.

История: relay manifest и setup page (ADR 0001) отменены [ADR 0003](../adr/0003-chain-document-and-join-token.md) — вставлять манифест больше некуда и незачем.

## 11. API-шпаргалка панели

```bash
P=https://<real-ip>:<port>/<webBasePath>
curl -sk -c cookies -X POST $P/login --data-urlencode username=… --data-urlencode password=…
curl -sk -b cookies $P/panel/api/chain/list                        # реестр цепочки
curl -sk -b cookies -X POST $P/panel/api/chain/add                 # новое звено; join-токен в ответе — один раз
curl -sk -b cookies -X POST $P/panel/api/chain/reissueToken/<id>   # перевыпустить токен (звено → pending)
curl -sk -b cookies -X POST $P/panel/api/chain/update/<id>         # сменить host/имя/порядок
curl -sk -b cookies -X POST $P/panel/api/chain/setActive/<id>      # активное edge
curl -sk -b cookies -X POST $P/panel/api/chain/del/<id>            # удалить звено (safeToPowerOffWhen в ответе)
curl -sk -b cookies $P/panel/api/awg/clients                       # AWG-клиенты
curl -sk -b cookies $P/panel/api/awg/client/<id>/config            # клиентский .conf в obj
curl -sk -b cookies -X POST $P/panel/api/server/restartXrayService # перезапуск Xray после правок конфига
curl -sk -b cookies -X POST $P/panel/setting/all                   # все настройки (subEnable, subJsonEnable, nginxMode, chainExtraPorts)
```

`POST /panel/setting/update` принимает **полный** объект настроек — перед отправкой возьмите актуальный из `/panel/setting/all`.
