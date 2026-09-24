# Контракт API панели для mon-server (v1)

Статус: **принят** — итог карты [Healthcheck-мониторинг inbound'ов: mon-server, mon-clients и контракт с панелью](https://github.com/SBKubric/3ax-ui-proxy/issues/20); черновик принят в тикете [Контракт API панели для mon-server](https://github.com/SBKubric/3ax-ui-proxy/issues/21), собран в [Собрать спеку](https://github.com/SBKubric/3ax-ui-proxy/issues/29). Термины — по [CONTEXT.md](../../CONTEXT.md) (real server, proxy front, host override, mon-server, mon-client, target, path, probe account, heartbeat, stale). Панельная сторона контракта — [monitoring-panel.md](monitoring-panel.md); сторона mon-server — [спека mon-server](https://github.com/SBKubric/3ax-ui-monitoring/blob/main/docs/spec/mon-server.md) в репо `3ax-ui-monitoring`. Принцип «mon-server — единственный источник, панель — пассивный приёмник» зафиксирован в [ADR 0004](../adr/0004-mon-server-single-source-panel-passive.md).

Правки 2026-09-24 по карте [Мониторинг: исполнение](https://github.com/SBKubric/3ax-ui-monitoring/issues/49), совместимые (версия контракта остаётся `1`): поэлементная валидация батчей, ответ с `rejected`, терпимость панели к неизвестным полям, грамматика `path`, пустой `from` у первого перехода, семантика `handshakeMs`, без `503 xray_unavailable` — [#50](https://github.com/SBKubric/3ax-ui-monitoring/issues/50); причины `PAUSED` — [#51](https://github.com/SBKubric/3ax-ui-monitoring/issues/51), [#53](https://github.com/SBKubric/3ax-ui-monitoring/issues/53); адрес probe-ссылок и `?hop=` до per-hop — [#54](https://github.com/SBKubric/3ax-ui-monitoring/issues/54); ревизия покрывает весь probe-материал (streamSettings, ключи, параметры AWG, набор probe-пиров), §4.2 — [3ax-ui-proxy#117](https://github.com/SBKubric/3ax-ui-proxy/issues/117).

Контракт описывает **только** ручки, которые панель (real server) открывает mon-server. Протокол mon-server ↔ mon-client — [mon-protocol.md](https://github.com/SBKubric/3ax-ui-monitoring/blob/main/docs/spec/mon-protocol.md) в репо `3ax-ui-monitoring`. Панель наружу не звонит: все запросы инициирует mon-server.

## 1. Версия и адрес

- Базовый путь: `<panelScheme>://<panelHost>:<panelPort><webBasePath>mon/v1/` — тот же листенер, TLS и `webBasePath`, что у панели. Версия контракта зашита в путь; несовместимое изменение = `/mon/v2/`.
- Каждый успешный ответ несёт заголовок `X-Mon-Contract: 1`; `GET /state` дублирует его полем `contract`.
- Совместимые изменения (новые необязательные поля, новые `reason`, новые `kind` событий) не меняют версию; обе стороны игнорируют неизвестные поля: mon-server — в ответах панели, панель — в телах запросов mon-server (строгий разбор с отказом на неизвестный ключ запрещён).

## 2. Аутентификация

- `Authorization: Bearer <monToken>`. Токен — настройка панели `monToken` (случайная строка 32 символа, как `secret`), включатель — `monEnable`. Генерация, повторная генерация и копирование — на вкладке Monitoring в настройках панели и в CLI `x-ui` (показать / сбросить). Новый токен немедленно инвалидирует старый; конфиг mon-server владелец обновляет сам.
- Сравнение токена constant-time. Хранение открытым текстом, как остальные секреты панели.
- **Без токена, с неверным токеном, при `monEnable=false` или пустом `monToken` — голый `404` без тела**, как у `checkAPIAuth` для `/panel/api`: панель не выдаёт своё присутствие. Все остальные статусы получает только авторизованный клиент. mon-server трактует `404` на `GET /state` как «неверный токен, путь или мониторинг выключен» и алертит в Telegram сам.
- Любой авторизованный запрос обновляет `monLastContact` — это и есть детектор STALE панели (порог `monStaleMinutes`, default 15).

## 3. Соглашения

- Тела — JSON, `Content-Type: application/json; charset=utf-8`. Все времена — `int64`, миллисекунды UTC epoch, как в моделях панели. Длительности и latency — `int64`, миллисекунды.
- Идентификаторы событий — **UUID v7** строкой (36 символов, lowercase); генерирует mon-server. `monClientId` — строка ≤ 64 символов `[A-Za-z0-9_.-]`, выдаёт mon-server.
- `inboundId` — числовой `id` xray-inbound'а панели. AWG-сервер адресуется `inboundId = 0` и `kind = "awg"`, чтобы target'ы xray и AWG жили в одном ключе `(monClientId, kind, inboundId, path)`.
- `path` — строка по грамматике `direct` | `proxy` | `edge:<name>` | `inner:<name>`, где `<name>` — имя звена цепочки (`[a-z0-9-]{1,32}`, [proxy-chain](proxy-chain.md) §6.1); по реестру цепочки имя не сверяется. Любое другое значение — ошибка элемента (`rejected`, ниже).
- Статусы: `200` успех с телом, `204` успех без тела, `400` схема/валидация, `404` см. §2 (и неизвестный маршрут), `409` конфликт (см. конкретные ручки), `413` батч больше лимита, `500` ошибка панели, `503` панель стартует / БД недоступна. Тело ошибки (кроме 404):

  ```json
  {"error": "invalid_body", "message": "events[3].kind: unknown value \"foo\""}
  ```

  `error` — стабильный snake_case код, `message` — для логов.
- Ретраи mon-server: только сеть, `5xx` и таймаут, с экспоненциальной задержкой; `4xx` — лог и дроп запроса.
- Батчи (`POST /events`, `POST /stats`) валидируются **поэлементно**: валидные элементы принимаются и пишутся, невалидные перечисляются в `rejected: [{index, id?, error}]` ответа `200` (`index` — позиция в массиве, `id` — только для событий, `error` — текст ошибки поля, как `message`). mon-server помечает отправленным всё, кроме `rejected`; отвергнутые логирует с `error` и помечает `dropped` без повторов. `400 invalid_body` на весь батч — только когда тело не читается (не JSON, нет массива) или версия неверна.
- Идемпотентность: `POST /events` по `id`, `POST /stats` — upsert по ключу, `POST /probe/ensure` — по построению. Повтор любого запроса безопасен.
- Лимиты: тело ≤ 1 МиБ; `events` ≤ 1000 элементов; `stats` ≤ 2000; `monClients` ≤ 200. Превышение — `413` `batch_too_large`.
- Неизвестный `inboundId` (inbound удалён) в событиях/статистике — запись пропускается, ответ `200`, id попадает в `ignored`. Панель не хранит ничего по неизвестным inbound'ам.

## 4. Ручки

| метод и путь | назначение | периодичность у mon-server |
|---|---|---|
| `GET /state` | снимок конфигурации и ревизия | раз в минуту (poll) |
| `POST /probe/ensure` | обеспечить probe-набор, обновить снимок реестра mon-clients | раз в минуту |
| `GET /probe/configs` | конфиги probe-набора для path | при смене `revision` |
| `DELETE /probe` | снять probe-набор (декомиссия mon-server) | вручную / uninstall |
| `POST /events` | переходы состояний | по факту, батчами |
| `POST /stats` | 5-минутные агрегаты | раз в 5 минут |

### 4.1 `GET /state`

Без побочных эффектов, кроме `monLastContact`.

```json
{
  "contract": 1,
  "panelVersion": "1.8.1-fork.3",
  "serverTime": 1757721600000,
  "revision": "9f2c1a7b3e5d4c60",
  "override": {"enabled": true, "host": "front.example.net"},
  "probe": {"subId": "k3j9d8s7f6g5h4j3", "lastEnsured": 1757721540000},
  "inbounds": [
    {"kind": "xray", "inboundId": 12, "tag": "inbound-443", "remark": "Reality main", "protocol": "vless", "port": 443, "enable": true},
    {"kind": "awg",  "inboundId": 0,  "tag": "awg",         "remark": "AmneziaWG",    "protocol": "awg",  "port": 51820, "enable": true}
  ],
  "stale": {"thresholdMinutes": 15}
}
```

- `inbounds` — **санированный** список: только поля выше, никаких `settings`/`streamSettings`/ключей. Попадают все клиентские xray-inbound'ы (vless/vmess/trojan/shadowsocks), включая выключенные, и AWG-сервер, если он создан. `port` — публичный порт (`publicPort` при nginx-фронте, иначе `port`).
- `probe.subId` — `null`, пока probe-набор ни разу не создан (первый `POST /probe/ensure` его заведёт).
- `override.host` — пустая строка при `enabled=false`.

### 4.2 Ревизия

`revision` — хэш **всего, что входит в probe-материал** (`/probe/configs`): первые 16 hex-символов SHA-256 от канонического JSON (ключи объектов отсортированы на всех уровнях, без пробелов) объекта:

```json
{"hiddifyCompat": false,
 "inbounds": [
   {"kind": "awg", "inboundId": 0, "protocol": "awg", "port": 51820, "enable": true,
    "peers": [{"name": "probe-awg", "conf": "[Interface]\nPrivateKey = …\n[Peer]\nEndpoint = probe.invalid:51820\n…"}]},
   {"kind": "xray", "inboundId": 12, "protocol": "vless", "port": 443, "enable": true, "listen": "",
    "stream": {"network": "tcp", "security": "reality", "realitySettings": {"serverNames": ["…"], "target": "…", "privateKey": "…", "shortIds": ["…"], "settings": {"publicKey": "…", "fingerprint": "chrome"}}},
    "settings": {"clients": [{"email": "probe-12", "id": "<uuid>", "flow": "xtls-rprx-vision", …}], "decryption": "none"}}
 ],
 "override": {"enabled": true, "host": "front.example.net"},
 "probeSubId": "k3j9d8s7f6g5h4j3"}
```

- `inbounds` — те же inbound'ы, что в `/state`, отсортированы по `(kind, inboundId)`; поля target'а `kind, inboundId, protocol, port, enable` плюс материал:
  - xray: `listen` (адрес path `direct`, если публичный); `stream` — `streamSettings` целиком, кроме `externalProxy` (в probe-ссылках он не участвует, §4.4): транспорт, TLS/Reality, `serverNames`/`target`/ключи/`shortIds`/SNI/fingerprint; `settings` — настройки протокола (метод shadowsocks, `decryption`, `fallbacks`…), где `clients` сокращён до probe-клиента этого inbound'а (`probe-<inboundId>` со всеми его полями). Добавление и правка пользователей ревизию не двигают. Сохранённые JSON-колонки разбираются и сериализуются заново, числа — в исходной записи.
  - AWG: `peers` — probe-пиры по имени (email), отсортированы по `name`; `conf` — текст `.conf`, как его отдаёт `/probe/configs`, но с хостом `Endpoint`, заменённым на `probe.invalid` (хост — не материал панели: для `proxy` это `override.host`, для `direct` — `host` из запроса). В `conf` входят публичные параметры AWG-сервера (публичный ключ, порт, MTU, DNS, обфускация) и ключи/адреса пира, так что ротация любого из них двигает ревизию; набор пиров — тоже.
- `hiddifyCompat` — настройка панели `xrayHiddifyCompat`, меняющая вид xhttp/grpc-ссылок.
- `remark`/`tag` в хэш не входят (переименование не меняет targets).

Ревизия детерминирована между рестартами панели: в хэше нет времени и нет зависимости от порядка map или порядка ключей в БД; счётчика в настройках нет. Для mon-server строка непрозрачна: он сравнивает её с последней виденной и при отличии перечитывает `/probe/configs` и пересобирает targets. Ревизия может сдвинуться и без видимой смены targets (ротация ключа, смена SNI) — это и есть сигнал перечитать материал.

### 4.3 `POST /probe/ensure`

Идемпотентно. Панель: (1) генерирует `monProbeSubId`, если пуст; (2) для каждого клиентского xray-inbound'а без клиента `probe-<inboundId>` создаёт его (`AddInboundClient`: xray API без рестарта), для AWG-сервера без клиента `probe-awg` — создаёт AWG-клиента с этим subId; атрибуты — по резолюции [Probe account](https://github.com/SBKubric/3ax-ui-proxy/issues/24); (3) ставит `monProbeLastEnsured = now`; (4) заменяет кэш реестра mon-clients содержимым тела.

Тело запроса — снимок реестра mon-clients (полная замена, не патч):

```json
{"monClients": [
  {"id": "ams-1", "name": "Amsterdam #1", "region": "NL", "state": "ONLINE", "lastHeartbeat": 1757721590000},
  {"id": "msk-1", "name": "Moscow #1",    "region": "RU", "state": "OFFLINE", "lastHeartbeat": 1757720000000}
]}
```

Ответ `200`:

```json
{"subId": "k3j9d8s7f6g5h4j3", "revision": "9f2c1a7b3e5d4c60", "lastEnsured": 1757721600000,
 "created": [{"kind":"xray","inboundId":12}], "present": 5}
```

`created` — что завёл этот вызов (обычно пусто), `present` — размер набора после вызова. Недоступный xray API — не ошибка: клиент записан в inbound, рестарт xray запланирован, ответ `200`. `5xx` — только при ошибке БД; набор тогда может быть создан частично, следующий ensure доделает.

Панель хранит снимок реестра как кэш для UI (в памяти + `monClientsSnapshot` в настройках, чтобы пережить рестарт), не как источник истины; поле `state` в нём — то, что сказал mon-server, панель его не пересчитывает. `state` ∈ `ONLINE` | `OFFLINE` | `NEVER` (`NEVER` — зарегистрирован, но heartbeat ещё не было, `lastHeartbeat = 0`); такие mon-clients не считаются offline и не входят в знаменатель coverage дайджеста ([monitoring-panel](monitoring-panel.md) §6).

### 4.4 `GET /probe/configs`

Отдаёт материал probe-набора для одного path. Панель рендерит теми же сервисами, что `/sub` и `/tun`, но по токену, без зависимости от sub-сервера.

- `GET /probe/configs` — как для пользователей: с host override, если он включён (path `proxy`). При `override.enabled=false` — `409` `override_disabled`.
- `GET /probe/configs?host=<realHost>` — без override (path `direct`): адрес — публичный `Listen` inbound'а, если он задан (единственный адрес, где inbound слушает), иначе из параметра. `host` — то, куда mon-server и так ходит за панелью; остальное (порт, SNI, serverName, ключи) панель не трогает.
- `GET /probe/configs?hop=<name>` (`?edge=<name>` — синоним) — path конкретного звена цепочки ([proxy-chain](proxy-chain.md) §6.1). До реализации per-hop любой непустой `hop`/`edge` → `409` `unknown_hop` (`unknown_edge` — синоним для старых клиентов); оба параметра с разными именами — тоже `409` `unknown_hop`. Панель не отдаёт вместо звена path `proxy`, и mon-server не принимает `proxy` за звено.
- `externalProxy` inbound'а в probe-ссылках не участвует: ссылка рендерится на копии stream без `externalProxy`, поэтому `direct` и `proxy` указывают туда, куда сказано выше, а не в `externalProxy.dest`. Отдельного path для `externalProxy` нет.

Ответ `200`:

```json
{"revision": "9f2c1a7b3e5d4c60", "path": "direct",
 "items": [
   {"kind": "xray", "inboundId": 12, "link": "vless://<uuid>@203.0.113.10:443?security=reality&…#probe-12"},
   {"kind": "awg",  "inboundId": 0,  "filename": "probe-awg", "conf": "[Interface]\nPrivateKey = …\n[Peer]\nEndpoint = 203.0.113.10:51820\n…"}
 ]}
```

- `link` — ссылка того же формата, что в `/sub` (ровно одна строка на inbound; multi-link inbound'ы отдают первую ссылку, т.к. probe один). `conf` — текст, идентичный элементу `/tun` ([tunnel subscription](tunnel-subscription.md) §6), с уже применённым host override к `Endpoint` для path `proxy` и с адресом из `host` для `direct`.
- Выключенные inbound'ы в `items` **не попадают** (как и в подписке) — так mon-server видит `PAUSED`. `revision` в ответе позволяет mon-server отбросить ответ, если ревизия уже устарела относительно `/state`.
- Если probe-набор ещё не создан — `409` `probe_not_ensured`.

### 4.5 `DELETE /probe`

Удаляет все probe account'ы (xray + AWG, вместе с `client_traffics`), забывает `monProbeSubId`, `monProbeLastEnsured`, снимок реестра. Ответ `204`. Для uninstall mon-server; то же делает hourly job панели после `monProbeTtlHours` (default 24) без ensure.

### 4.6 `POST /events`

Батч переходов. Панель применяет к состоянию target'ов/mon-clients, пишет в ленту событий и шлёт Telegram по правилам [State machine target'а](https://github.com/SBKubric/3ax-ui-proxy/issues/22).

```json
{"events": [
  {"id": "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", "ts": 1757721540000, "kind": "target",
   "monClientId": "ams-1", "inboundKind": "xray", "inboundId": 12, "path": "proxy",
   "from": "UP", "to": "DOWN", "reason": "tls_timeout", "notified": false},
  {"id": "019254a0-8a11-7e30-8c2d-2a3b4c5d6e7f", "ts": 1757721545000, "kind": "mon_client",
   "monClientId": "msk-1", "from": "ONLINE", "to": "OFFLINE", "reason": "heartbeat_missed", "notified": false},
  {"id": "019254a0-9b22-7f41-9d3e-3b4c5d6e7f80", "ts": 1757721000000, "kind": "panel",
   "from": "PANEL_UP", "to": "PANEL_DOWN", "reason": "http_timeout", "notified": true}
]}
```

- `kind`: `target` (обязательны `monClientId`, `inboundKind`, `inboundId`, `path`), `mon_client` (обязателен `monClientId`), `panel` (ни того, ни другого; всегда `notified=true`, панель только пишет в ленту).
- `from`/`to` для `target`: `UP` `DOWN` `FLAPPING` `UNKNOWN` `PAUSED`; для `mon_client`: `ONLINE` `OFFLINE`; для `panel`: `PANEL_UP` `PANEL_DOWN`. `from` пустой у первого перехода (панель принимает пустой `from` для любого `kind`); внутреннее состояние реестра mon-server `NEVER` наружу не уходит — первый переход mon-client приходит как `"" → ONLINE`.
- Переходы считает только mon-server, панель их не выводит и не проверяет: из `UNKNOWN` в `DOWN` — только после `downAfter` провалов подряд, в `UP` — с первого успеха; выход из `FLAPPING` — только по пришедшему результату, не по таймеру ([спека mon-server](https://github.com/SBKubric/3ax-ui-monitoring/blob/main/docs/spec/mon-server.md) §7.2).
- `reason` — snake_case из словаря диагностики (`tcp_refused` `tcp_timeout` `tls_timeout` `reality_real_cert` `awg_no_handshake` `http_error` `heartbeat_missed` `recovered` `flapping` `config_disabled` `config_enabled` `http_timeout`…); неизвестные значения панель принимает и показывает как есть (свободная строка ≤ 128 символов).
- `PAUSED` — административное состояние target'а, не инцидент. Кроме выключенного inbound'а (`config_disabled`), mon-server ставит его, когда target выпал из конфига по не-inbound причине, с `reason`: `override_disabled` (выключен host override), `path_removed` (у mon-client убран path), `no_probe_link` (у inbound'а нет probe-ссылки), `config_error` (mon-client отверг конфиг этой цели). Строка target'а не удаляется; при возврате в конфиг — `UNKNOWN`, затем первый результат.
- Отзыв токена mon-client приходит как `mon_client` `ONLINE → OFFLINE` с `reason` `token_revoked`, его targets — `→ UNKNOWN` с `reason` `mon_client_revoked`.
- `notified=true` → панель не шлёт Telegram за это событие (mon-server уже отправил «via mon-server»).
- Ответ `200`: `{"accepted": 2, "duplicates": 0, "ignored": [{"id": "…", "error": "unknown_inbound"}], "rejected": [{"index": 3, "id": "…", "error": "events[3].path: unknown value \"foo\""}]}`. `rejected` — невалидные элементы (§3), остальные элементы батча приняты. Дубликат по `id` — не ошибка. Окно дедупликации — та же скользящая неделя, что у ленты событий.
- Порядок применения — по `ts` внутри батча; событие старше текущего состояния target'а (пришло с опозданием после `PANEL_DOWN`) пишется в ленту, но состояние не откатывает.

### 4.7 `POST /stats`

Батч 5-минутных агрегатов; ключ `(monClientId, inboundKind, inboundId, path, bucketStart)`, `bucketStart` кратен 300 000 мс. Upsert: повтор с тем же ключом заменяет запись.

```json
{"stats": [
  {"monClientId": "ams-1", "inboundKind": "xray", "inboundId": 12, "path": "proxy", "bucketStart": 1757721300000,
   "nOk": 5, "nFail": 0, "latencyMinMs": 41, "latencyAvgMs": 47, "latencyMaxMs": 58, "handshakeMs": null}
]}
```

- `latency*` — фаза TLS/подключения по research [mon-client probes](https://github.com/SBKubric/3ax-ui-proxy/issues/28); `handshakeMs` — только AWG: латентность handshake в пробе, `last_handshake − t(dial)` (определение [протокола mon-server ↔ mon-client](https://github.com/SBKubric/3ax-ui-monitoring/blob/main/docs/spec/mon-protocol.md) §5.3), иначе `null`. При `nOk=0` все latency — `null`.
- Ответ `200`: `{"accepted": 1, "ignored": [], "rejected": [{"index": 4, "error": "stats[4].bucketStart: must be a positive multiple of 300000 ms"}]}`; `rejected` — по §3, без `id`.
- Хранение, ретеншн (скользящая неделя) и расчёт суточного uptime для дайджеста — тикет [Схема статистики](https://github.com/SBKubric/3ax-ui-proxy/issues/25); wire-форма выше — его вход.

## 5. Настройки панели, добавляемые контрактом

| ключ | тип | default | назначение |
|---|---|---|---|
| `monEnable` | bool | `false` | включатель ручек |
| `monToken` | string | `""` | bearer-токен |
| `monStaleMinutes` | int | `15` | порог STALE |
| `monProbeSubId` | string | `""` | subId probe-набора |
| `monProbeLastEnsured` | int64 ms | `0` | последний ensure |
| `monProbeTtlHours` | int | `24` | TTL очистки probe-набора |
| `monLastContact` | int64 ms | `0` | последний авторизованный запрос |
| `monStaleSince` | int64 ms | `0` | начало текущего STALE, `0` — не STALE; переживает рестарт |
| `monClientsSnapshot` | JSON string | `"[]"` | кэш реестра для UI |

Все — через `defaultValueMap` + `entity.AllSetting` + getter/setter, как остальные ключи форка.

## 6. Поведение панели, на которое опирается контракт

- Фоновая job `MonitoringJob` (`@every 1m`): STALE по `monLastContact`, момент объявления хранится в `monStaleSince` (рестарт панели не повторяет «monitoring silent»); при `monEnable=false` STALE не объявляется и не шлётся. Раз в час — очистка probe-набора по TTL.
- Удаление inbound каскадно удаляет его состояние, события и агрегаты; последующие события/статистика по этому `inboundId` → `ignored`.
- `GET /state` и `POST /probe/ensure` не требуют работающего xray: probe-клиент при недоступном xray API записывается в inbound, рестарт xray планируется (§4.3).

## 7. Пример цикла mon-server

1. Старт: `GET /state` → `POST /probe/ensure` (со снимком реестра) → `GET /probe/configs?host=<real>` и, при `override.enabled`, `GET /probe/configs` → раздать targets mon-clients.
2. Раз в минуту: `GET /state`; ревизия изменилась → шаг 1 без первого пункта. `POST /probe/ensure` раз в минуту (с актуальным снимком).
3. По переходам: `POST /events`. Раз в 5 минут: `POST /stats`.
4. Три неудачи подряд → `PANEL_DOWN`: события копятся (≤ 24 ч) и досылаются одним или несколькими батчами с `notified=true`; poll `GET /state` продолжается как детектор возврата.
