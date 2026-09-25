# Мониторинг inbound'ов: панельная часть

Спека и план реализации панельной стороны мониторинга. Итог карты [Healthcheck-мониторинг inbound'ов: mon-server, mon-clients и контракт с панелью](https://github.com/SBKubric/3ax-ui-proxy/issues/20); решения приняты в её тикетах, здесь они только собраны. Термины по [CONTEXT.md](../../CONTEXT.md): real server, proxy front, host override, mon-server, mon-client, target, path, probe account, tunnel probe, heartbeat, stale. Wire-форма — [Контракт API панели для mon-server](monitoring-contract.md); принципы — [ADR 0002](../adr/0002-additive-upstream-compatibility.md) (аддитивность к upstream) и [ADR 0004](../adr/0004-mon-server-single-source-panel-passive.md) (mon-server — единственный источник, панель — пассивный приёмник). Сторона mon-server и mon-client — [репо 3ax-ui-monitoring](https://github.com/SBKubric/3ax-ui-monitoring/tree/main/docs/spec).

Правки 2026-09-24 по карте [Мониторинг: исполнение](https://github.com/SBKubric/3ax-ui-monitoring/issues/49): поэлементный приём `/events` и `/stats`, грамматика `path`, `monStaleSince`, coverage дайджеста по `ONLINE`, без `503 xray_unavailable` — [#50](https://github.com/SBKubric/3ax-ui-monitoring/issues/50); `PAUSED` ниже `UP` в свёртке — [#51](https://github.com/SBKubric/3ax-ui-monitoring/issues/51); адрес probe-ссылок, `?hop=` до per-hop, guards `probe-*` в `AddInbound`/`UpdateInbound` — [#54](https://github.com/SBKubric/3ax-ui-monitoring/issues/54); ревизия по всему probe-материалу — [3ax-ui-proxy#117](https://github.com/SBKubric/3ax-ui-proxy/issues/117); AWG probe-пир на mon-client × path, контракт v2 — [3ax-ui-monitoring#80](https://github.com/SBKubric/3ax-ui-monitoring/issues/80), [3ax-ui-proxy#120](https://github.com/SBKubric/3ax-ui-proxy/issues/120). Wire-сторона тех же решений — в контракте.

Правки 2026-09-25, контракт 3 — решение [sane-3x-ui-monitoring#61](https://github.com/SBKubric/sane-3x-ui-monitoring/issues/61): `chain` (звенья `joined`/`legacy`) в `GET /state` и в ревизии, `?hop=` с адресом звена, AWG probe-пиры на mon-client × path каждого звена, потолок `monProbePeerLimit`; `proxy` — только пока пробируемых звеньев нет ([proxy-chain](proxy-chain.md) §6). Уточнения того же дня: снимок ensure несёт `paths` mon-clients и пиры заводятся только на реально пробируемые пары, `unallocated` — с `reason` (`pool_exhausted`/`limit`), при смене пробируемого набора path строки `mon_targets` выпавших path удаляются, `chain.hops` — inner'ы по `position`, затем edge по имени.

## 1. Цель и границы

Панель (real server) **принимает** мониторинг: открывает mon-server ручки контракта, заводит по его запросу probe accounts, хранит состояние targets, ленту событий и агрегаты, показывает страницу Monitoring и бейдж Health у inbound'ов, шлёт Telegram по переходам, ведёт единственное своё состояние — STALE. Панель **не** ходит наружу, не ведёт реестр mon-clients (только кэш снимка) и не считает UP/DOWN.

Рамка:

- **Аддитивность** (ADR 0002): новые файлы `monitoring*`, новые таблицы, ключи настроек в форковом блоке, по одной точке вставки в upstream-файл. Список — §10.
- Протоколы v1: xray-inbound'ы (vless/vmess/trojan/shadowsocks) и AmneziaWG-сервер (`inboundKind=awg`, `inboundId=0`). WireGuard и MTProto — после v1.
- Вне scope (Out of scope карты): групповая атрибуция алертов, автодействия по алертам, смена proxy front из Telegram, несколько mon-server на панель.

## 2. Модель данных

### 2.1 Таблицы (`database/model/monitoring.go`, gorm AutoMigrate через `initModels`, имена индексов с префиксом `idx_mon_` — индексы SQLite глобальны, регистрируются в `namedIndexes` в `database/db.go`)

| таблица | ключ | поля | индексы |
|---|---|---|---|
| `mon_targets` | unique `(mon_client_id, inbound_kind, inbound_id, path)` | `id`, `state` (`UP`/`DOWN`/`FLAPPING`/`UNKNOWN`/`PAUSED`), `since` ms, `reason`, `updated_at` ms | unique выше; `(inbound_kind, inbound_id)` |
| `mon_events` | `id` UUID v7 (PK, строка 36) | `ts`, `received_at`, `kind` (`target`/`mon_client`/`panel`), `mon_client_id`, `inbound_kind`, `inbound_id`, `path`, `from`, `to`, `reason`, `notified` | `(ts)`; `(inbound_kind, inbound_id, ts)` |
| `mon_stats_current` | unique `(mon_client_id, inbound_kind, inbound_id, path, bucket_start)` | `bucket_ms` (в v1 = 300000), `n_ok`, `n_fail`, `lat_min`, `lat_avg`, `lat_max`, `handshake_ms` (nullable) | unique выше; `(inbound_kind, inbound_id, bucket_start)` |
| `mon_stats_rollup` | unique `(mon_client_id, inbound_kind, inbound_id, path, step_ms, bucket_start)` | `step_ms`, `n_buckets`, `n_ok`, `n_fail`, `lat_min`, `lat_avg` (взвешен по `n_ok`), `lat_max`, `handshake_ms` (max) | unique выше; `(inbound_kind, inbound_id, step_ms, bucket_start)` |

- Имена **current / rollup**, а не 5m / 1h: размер окна живёт в строке (`bucket_ms`, `step_ms`) и в настройке, не в имени таблицы.
- `lat_*` — целые миллисекунды, `NULL` при `n_ok = 0`. Все времена — ms UTC.
- Строка `mon_targets` появляется из первого события или агрегата по ключу; удаляется, когда её mon-client пропал из снимка реестра (§4.4), удалён inbound (§3.6) или её path выпал из пробируемого набора ([proxy-chain](proxy-chain.md) §6.1).

### 2.2 Настройки (`defaultValueMap` в `web/service/setting.go`, `entity.AllSetting`, getter/setter — внутри форкового блока `proxyOverride*`; JS-дефолты в `web/assets/js/model/setting.js`)

| ключ | тип | default | назначение |
|---|---|---|---|
| `monEnable` | bool | `false` | включатель ручек `/mon/v1` |
| `monToken` | string | `""` | bearer-токен mon-server |
| `monStaleMinutes` | int | `15` | порог STALE |
| `monProbeSubId` | string | `""` | subId probe-набора |
| `monProbeLastEnsured` | int64 ms | `0` | последний `POST /probe/ensure` |
| `monProbeTtlHours` | int | `24` | TTL очистки probe-набора без ensure |
| `monProbePeerLimit` | int | `32` | потолок AWG probe-пиров, `0` — без лимита (контракт §4.3) |
| `monLastContact` | int64 ms | `0` | последний авторизованный запрос mon-server |
| `monStaleSince` | int64 ms | `0` | начало текущего STALE, `0` — не STALE (§5) |
| `monClientsSnapshot` | JSON string | `"[]"` | кэш реестра mon-clients для UI |
| `monRetentionDays` | int | `7` | ретеншн `mon_stats_current` и `mon_events`; окно дедупликации событий |
| `monRollupRetentionDays` | int | `30` | ретеншн `mon_stats_rollup` |
| `monRollupStepMinutes` | int | `60` | шаг rollup |

Миграций не нужно: ключ появляется при первом сохранении настроек. `monToken` хранится открытым текстом, как остальные секреты панели; сравнение constant-time.

## 3. Probe account

Решения тикета [Probe account: соглашение по имени, исключения, очистка по таймауту](https://github.com/SBKubric/3ax-ui-proxy/issues/24).

### 3.1 Один набор на панель

По одному probe-клиенту в каждом клиентском xray-inbound'е (vless/vmess/trojan/ss), **включая выключенные**, — общему для всех mon-clients и path (у VLESS нет состояния сессии на клиента), и в AWG-сервере по **probe-пиру на каждую пару mon-client × path** ([3ax-ui-monitoring#80](https://github.com/SBKubric/3ax-ui-monitoring/issues/80)): у WireGuard-пира один endpoint и одна сессия, общий пир параллельные пробы `direct` и `proxy` (и разных mon-clients) отбирали бы друг у друга. Всё под общим `monProbeSubId`. subId генерирует и хранит **панель** (формат обычного subId, 16 символов); mon-server его только получает из `GET /state` и `POST /probe/ensure`.

### 3.2 Имя и guard

- Email xray-клиента `probe-<inboundId>`, AWG-пира `probe-awg-<monClientId>-<path>` (`:` в path → `-`; `monClientId` — 1–32 символа `[A-Za-z0-9_-]`, контракт §3; WG позже — `probe-wg-…`). Общий `probe-awg` контракта v1 удаляет первый ensure v2. Префикс `probe-` — константа кода `web/service/monitoring_probe.go`: `const ProbePrefix = "probe-"`, `func IsProbeAccount(email string) bool` (без учёта регистра).
- **Все обычные пути** создания и правки клиента отказывают email с префиксом `probe-`: `InboundService.AddInboundClient`, `UpdateInboundClient`, `AddInbound` (любой `probe-*` в `settings.clients`), `UpdateInbound` (`probe-*`, которого в inbound не было; существующий probe-клиент проходит неизменным, его удаление разрешено), `POST /panel/api/{awg,wg}/client/add|update*`, tgbot, LDAP-синк. Один guard-вызов в начале каждого хендлера (точки вставки — §10). Создаёт только `MonitoringService.EnsureProbeSet` (§4.3).
- Правка и переименование probe account **запрещены** (снятый префикс превратил бы probe в пользователя); **удаление разрешено** — ensure воссоздаст клиента через минуту с новым uuid и тем же subId («сбросить probe»).
- AWG `ToggleClient` отдельного guard'а не требует — он уже отказывает через `UpdateClient`. AWG `ResetToDefaults` — без особого каскада: сброс поднимает ревизию мониторинга, mon-server перечитывает `/probe/configs`, AWG-target уходит в `PAUSED`, следующий ensure заводит пиры заново.

### 3.3 Атрибуты при создании

`enable=true`, `totalGB=0`, `expiryTime=0`, `limitIp=0`, `reset=0`, `tgId=0`, `subId=monProbeSubId`, `comment="monitoring probe"`; `flow` копируется у первого обычного клиента inbound'а (нет клиентов — пустой), чтобы проба шла тем же режимом, что и пользователи. xray-клиент добавляется через xray API без рестарта (как `AddInboundClient`); AWG-пир — `TunnelService[awg].addClient` (адрес из пула сервера, свои ключи и PSK) + связка subId через `TunnelSubscriptionService.Set` ([tunnel subscription](tunnel-subscription.md) §3).

### 3.4 «Не считается пользователем»

Лимиты, expiry, limitIp, отчёт «истощённых» и DM клиентам не срабатывают сами: у probe нулевые `totalGB/expiryTime/limitIp/tgId`. Явный фильтр `IsProbeAccount` нужен в:

- онлайн-множествах и счётчиках: `InboundService.GetOnlineClients`, `TunnelService.GetOnlineClients`;
- счётчике клиентов в таблице inbound'ов и бейдже online у inbound;
- отчёте бота (`prepareServerUsageInfo`, кнопки online, списки клиентов, поиск по email);
- **LDAP auto-delete** (`LdapSyncJob.deleteClientsNotInLDAP`) — иначе синк сотрёт probe в LDAP-inbound'е.

Трафик проб **не вычитаем** из суммы inbound'а, `addInboundTraffic` не трогаем; строка probe в `client_traffics` живёт как у всех.

### 3.5 UI

Строка probe account в таблицах xray- и AWG-клиентов с серым бейджем `probe`; редактирование скрыто, удаление оставлено; счётчики клиентов и online у inbound его не учитывают.

### 3.6 Удаление inbound

В транзакции удаления inbound панель каскадно удаляет все четыре таблицы §2.1 по `(inbound_kind, inbound_id)` (как upstream удаляет `client_traffics`). Telegram молчит. mon-server на следующем poll видит, что inbound исчез, и снимает targets с mon-clients; события и статистика по неизвестному `inboundId` принимаются с `200` и попадают в `ignored`, чтобы mon-server их не ретраил.

## 4. Контроллер и сервис контракта

### 4.1 Маршруты

Новый файл `web/controller/monitoring.go`, `MonitoringController`, регистрируется одним блоком в `web/web.go` рядом с `NewAPIController`: группа `<webBasePath>mon/v1`, middleware `checkMonAuth` (§4.2), затем ручки контракта:

| маршрут | метод сервиса |
|---|---|
| `GET /state` | `State()` |
| `POST /probe/ensure` | `EnsureProbeSet(snapshot)` |
| `GET /probe/configs[?host=\|?hop=\|?edge=]` | `ProbeConfigs(host, hop)` |
| `DELETE /probe` | `DeleteProbeSet()` |
| `POST /events` | `ApplyEvents(batch)` |
| `POST /stats` | `UpsertStats(batch)` |

Каждый успешный ответ несёт `X-Mon-Contract: 3` (`MonContractVersion`; путь `/mon/v1` исторический, контракт §1). Тела — JSON, лимит 1 МиБ (`http.MaxBytesReader`), неизвестные поля игнорируются (без `DisallowUnknownFields`), батчи валидируются поэлементно — невалидные элементы уходят в `rejected` ответа `200`, `400 invalid_body` только для нечитаемого тела (контракт §3); статусы и тело ошибки `{error, message}` — по контракту §3. Конверт `{success,msg,obj}` панельного API здесь **не** используется: mon-server — не браузер.

### 4.2 Аутентификация

`checkMonAuth` по образцу `APIController.checkAPIAuth` (`web/controller/api.go`): если `monEnable=false`, `monToken` пуст, заголовок `Authorization: Bearer <token>` отсутствует или не совпадает (`subtle.ConstantTimeCompare`) — `c.AbortWithStatus(404)` без тела. Успешная проверка пишет `monLastContact = now` (в память + в настройку не чаще раза в 10 с, чтобы не долбить SQLite).

### 4.3 `MonitoringService` (`web/service/monitoring_service.go`)

- **`State()`** — санированный список inbound'ов (`kind, inboundId, tag, remark, protocol, port, enable`; `port` = `publicPort` при nginx-фронте, иначе `port`), `override` из `proxyOverrideEnable/Host`, `chain` из реестра цепочки (звенья `joined`/`legacy`: inner'ы по `position`, затем edge по имени, [proxy-chain](proxy-chain.md) §6.1; нет поля при пустом реестре), `probe.subId/lastEnsured`, `revision`, `stale.thresholdMinutes`, `panelVersion`, `serverTime`. **Ревизия** — первые 16 hex SHA-256 канонического JSON всего probe-материала: `{hiddifyCompat, override, chain{activeEdge, hops}, probeSubId, inbounds[…] sorted by (kind, inboundId)}`, где у xray-inbound'а к полям target'а добавлены `listen`, `stream` (`streamSettings` без `externalProxy`) и `settings` с `clients`, сокращённым до probe-клиента, а у AWG — `peers[{name, conf}]` (`.conf` probe-пира с хостом `Endpoint` = `probe.invalid`) (контракт §4.2, [#117](https://github.com/SBKubric/3ax-ui-proxy/issues/117)). Тег и remark не входят; считается на каждый запрос, счётчика нет, от времени и порядка ключей не зависит.
- **`EnsureProbeSet(snapshot)`** — идемпотентно: `monClientId` вне `[A-Za-z0-9_-]{1,32}` → `400 invalid_body` до любых изменений; сгенерировать `monProbeSubId`, если пуст; дозавести недостающих xray probe-клиентов (§3.3); сверить AWG probe-пиры со снимком (`ensureTunnelProbes`): пир на каждый mon-client снимка (любое `state`) × пробируемые path из его `paths` (`direct`; `hops` — `edge:<name>`/`inner:<name>` каждого звена `joined`/`legacy`, без таких звеньев — `proxy`; явные `edge:`/`inner:` — если звено пробируется) в пределах `monProbePeerLimit` — по приоритету `direct` → active edge → standby edge по имени → inner по порядку цепочки, внутри уровня по `monClientId` (контракт §4.3); паре сверх лимита пир не заводится (`unallocated`, `reason: limit`); остальные probe-пиры — включая v1-шный `probe-awg` — удалить, сначала удаление, потом создание в порядке приоритета; исчерпанный пул (`ipam.ErrPoolExhausted`) — warning в лог, пара в `unallocated` ответа с `reason: pool_exhausted`, остальное идёт дальше; `monProbeLastEnsured = now`; заменить кэш снимка реестра (в памяти + `monClientsSnapshot`); удалить строки `mon_targets`, чей `mon_client_id` отсутствует в снимке (события и агрегаты остаются до ретеншна). Недоступный xray API — не ошибка: клиент записан в inbound, рестарт xray запланирован, `200`; `5xx` — только ошибка БД.
- **`ProbeConfigs(host, hop)`** — те же сервисы, что `/sub` и `/tun`: ссылки xray-клиентов probe-набора (без `monClientId`) и по `conf` на каждый mon-client снимка, у которого есть AWG-пир этого path, с `monClientId` и `filename` = имя пира. Ссылка рендерится на копии stream **без `externalProxy`** и всегда однострочная (`ProbeConfigs` страхуется первой строкой). Без `host` — с host override (path `proxy`; `409 override_disabled`, если он выключен); с `host` (path `direct`) — публичный `Listen` inbound'а, если задан, иначе адрес из параметра. С `?hop=`/`?edge=` — адрес звена из реестра (path `edge:<name>`/`inner:<name>`); `409 unknown_hop` — имени нет в реестре или два разных имени, `409 hop_not_joined` — звено не `joined`/`legacy` ([proxy-chain](proxy-chain.md) §6.1). Выключенные inbound'ы не попадают. `409 probe_not_ensured`, пока набор не создан.
- **`DeleteProbeSet()`** — удалить все probe account'ы по префиксу (xray + все AWG-пиры, вместе с `client_traffics`), очистить `monProbeSubId`, `monProbeLastEnsured`, снимок; `204`.
- **`ApplyEvents(batch)`** — поэлементная валидация (невалидные → `rejected: [{index, id, error}]`, остальные применяются; `path` по грамматике `direct|proxy|edge:<name>|inner:<name>`, пустой `from` допустим), затем в одной транзакции по порядку `ts`: вставка в `mon_events` (дубликат `id` → `duplicates`, неизвестный inbound → `ignored`), применение к `mon_targets` (событие старше текущего `since` пишется в ленту, но состояние не откатывает; `kind=mon_client` и `kind=panel` состояния target'ов не меняют), затем Telegram по §6 для событий с `notified=false`. Окно дедупликации — `monRetentionDays`.
- **`UpsertStats(batch)`** — поэлементная валидация как у `ApplyEvents` (`rejected: [{index, error}]`); upsert в `mon_stats_current` по ключу; в той же транзакции пересчёт затронутых rollup-бакетов (`GROUP BY` по строкам current в `[bucket_start − bucket_start % step_ms, +step_ms)`) и upsert в `mon_stats_rollup` с текущим `step_ms` (`n_buckets` показывает частичность незакрытого часа). Бакеты старше `monRetentionDays` → `ignored`.
- **`WorstLiveTargetState(inboundKind, inboundId)`** — свёртка для бейджа: худший target среди строк `mon_targets` с mon-client из текущего снимка по приоритету `DOWN > FLAPPING > UNKNOWN > UP > PAUSED`; `STALE` панели перекрывает всё. `PAUSED` ниже `UP` намеренно: это административное состояние (`config_disabled`, `override_disabled`, `path_removed`, `no_probe_link`, `config_error`, контракт §4.6), а не инцидент. Используется колонкой Health и фильтром `down` (§7.2).
- **`CascadeDeleteInbound(kind, id)`** — §3.6, вызывается из `InboundService.DelInbound` и удаления AWG-сервера.
- **Смена пробируемого набора path** ([proxy-chain](proxy-chain.md) §6.1) — в транзакции записи реестра цепочки удаляются строки `mon_targets`, чьего path в наборе больше нет; события и агрегаты стареют по ретеншну, Telegram молчит.

### 4.4 Снимок реестра

Тело `POST /probe/ensure` — полная замена, не патч; у каждого mon-client — его `paths` (контракт §4.3). Панель хранит его как кэш для UI (`monClientsSnapshot`), поле `state` в нём — то, что сказал mon-server, панель его не пересчитывает. mon-client вне снимка на странице Monitoring не показывается; его события в ленте помечены «выведен».

## 5. Фоновая job

`web/job/monitoring_job.go`, `MonitoringJob`, по образцу `CheckCpuJob`; регистрация в `web/web.go:startTask` рядом с остальными `addJob`:

- **`@every 1m`** — STALE: `now − monLastContact > monStaleMinutes` → все targets считаются `STALE` (строки `mon_targets` не трогаются), момент объявления пишется в `monStaleSince`, одно сообщение «monitoring silent since …»; флаг переживает рестарт панели, и «silent» после рестарта не повторяется. Первый авторизованный запрос сбрасывает `monStaleSince = 0` и шлёт «monitoring back (был недоступен N мин)». Пока `monLastContact = 0` (mon-server ни разу не приходил) или `monEnable=false` — STALE не объявляется и не шлётся.
- **`@hourly`** — (1) TTL probe-набора: `now − monProbeLastEnsured > monProbeTtlHours` → `DeleteProbeSet()`; та же ветка подчищает probe account'ы при пустом subId; (2) ретеншн батчами по 5000 `rowid`: `mon_stats_current` и `mon_events` старше `monRetentionDays`, `mon_stats_rollup` старше `monRollupRetentionDays`; без VACUUM (одно соединение к SQLite, busy 5 с); (3) при смене `monRollupStepMinutes` — пересборка rollup за окно `monRetentionDays` из current с новым `step_ms`, старые строки с прежним `step_ms` доживают свой ретеншн.

## 6. Telegram

Решения тикета [State machine target'а: DOWN/UP/STALE, атрибуция сбоев, Telegram-алерты](https://github.com/SBKubric/3ax-ui-proxy/issues/22).

- Через существующий `Tgbot.SendMsgToTgbotAdmins` и ключи `tgbot.messages.monitoring.*` (`I18nBot`). Событий панель не группирует: только `target DOWN/UP` (с длительностью простоя и диагностикой), `FLAPPING` вход/выход, `mon-client OFFLINE/ONLINE`, `STALE`/восстановление; переходы `PANEL_DOWN/UP` пишутся в ленту с пометкой «via mon-server», уведомление уже отправил mon-server.
- Формат: `⛔ DOWN · <inbound remark> via <path> · <mon-client name> (<region>) · reason: <diag> · since <HH:MM>`; `✅ UP · … · down for 7m`; `〰 FLAPPING · …`; `mon-client <name> (<region>) OFFLINE` / `ONLINE (был недоступен N мин)`; `monitoring silent since <HH:MM>` / `monitoring back (was silent N min)`.
- Дедупликация: событие с `notified=true` не уведомляется; повтор `id` — `200` без действий.
- **Ежедневный дайджест**: блок «Monitoring» в `Tgbot.SendReport` (runtime `StatsNotifyJob` из настроек), скользящие `[now − 24h, now)`: строка на inbound `<remark>: direct 99.8 % · proxy 97.1 % (cov 92 %) · 2 инцидента` (по path суммы `n_ok/n_fail` по всем mon-clients; инцидент = событие `kind=target, to=DOWN`), «худший target» — минимальный uptime среди target'ов с `coverage ≥ 50 %`, строка про mon-clients `OFFLINE` сейчас, отдельная строка «ожидают первого heartbeat» для mon-clients `NEVER` (они не считаются offline) и суммарное время STALE за окно. Знаменатель coverage — только mon-clients в `ONLINE` с этим path. Расчёт тот же, что у `GET summary` (§7.4).

## 7. UI панели

Решения тикета [UI панели: страница Monitoring и бейдж у inbound](https://github.com/SBKubric/3ax-ui-proxy/issues/26): вариант A прототипа ([артефакт](https://claude.ai/code/artifact/ef44c1c4-3d8b-4e87-a08f-ccf38503d557), [исходник](https://github.com/SBKubric/3ax-ui-proxy/blob/prototype/monitoring-ui/docs/prototypes/monitoring-ui.html)).

### 7.1 Страница Monitoring

`web/html/monitoring.html`, маршрут `panel/monitoring` в `web/controller/xui.go:initRouter` (одна строка), пункт меню после Inbounds в `web/html/component/aSidebar.html` (иконка `line-chart`, без счётчика).

- Шапка: заголовок + пилюля «monitoring live · last contact Ns ago»; при STALE — жёлтая штриховая «monitoring silent since …».
- Ряд пилюль mon-clients из снимка реестра: точка ONLINE/OFFLINE (пульсация как у online-клиентов), имя, регион, «hb Ns ago» / «OFFLINE Nm».
- `a-card` на каждый inbound из `GET targets` (включая PAUSED): заголовок remark + тег протокола + порт, справа адрес proxy front при включённом override и свёрнутый бейдж; внутри `a-table` target'ов — mon-client · path (тег `blue` для proxy, plain для direct) · state · «for 17m» · uptime 24h (+ «cov N%» при покрытии < 100) · latency avg · reason; под таблицей sparkline uptime по inbound (`GET stats?range=7d`, переключатель 24h/7d/30d) на существующем компоненте `sparkline` из `web/html/index.html` — чарт-библиотек не добавляем.
- Справа карточка Events — лента `GET events?limit=50` без группировки по дням: «● DOWN · Reality main via proxy · ams-1 · tls_timeout», для panel-событий приписка «via mon-server».

**Цвета** (панельные, light/dark): UP `#008771`/`#3ad3ba`, DOWN `#cf3c3c`/`#e04141`, FLAPPING `#f37b24`/`#ffa031`, UNKNOWN `#7a316f`/`#b06aa5`, PAUSED `#bcbcbc`/`#2c3950`, STALE янтарный со штриховой точкой; mon-client ONLINE/OFFLINE — зелёный/красный.

### 7.2 Таблица inbound'ов

Колонка **Health** после Clients в `web/html/inbounds.html` (десктоп; на мобильном — в `info`): один тег состояния по `WorstLiveTargetState`, тултип перечисляет target'ы не в UP, раскрытие строки не трогаем. Фильтр-чип **down** рядом с all/depleted/expiring/online. Данные приходят с `GET targets` одним запросом на страницу.

### 7.3 Настройки

Пятая вкладка **Monitoring** в `web/html/settings.html` (`settings/panel/monitoring.html`, иконка `line-chart`) из `a-setting-list-item`: включатель `monEnable`; `monToken` в input read-only с кнопками Copy и Regenerate (Regenerate — подтверждение; новый токен сразу инвалидирует старый); `monStaleMinutes`; `monRetentionDays`; `monRollupRetentionDays`; `monProbePeerLimit` (`0` — без лимита); информационная строка probe-набора (subId, last ensured, число клиентов) с кнопкой «Remove probe set» (`DeleteProbeSet` тем же сервисом). Общая кнопка Save. Дубль токена в CLI `x-ui` (показать / сбросить), по образцу `secret`.

### 7.4 Ручки для UI (`/panel/api/monitoring/`, сессия, конверт `{success,msg,obj}`; новый файл `web/controller/monitoring_ui.go`, регистрация одной строкой в `APIController.initRouter`)

- `GET targets` — живые строки `mon_targets` + снимок реестра + `stale` панели + свёртка по inbound'ам.
- `GET events?before=<ms>&limit=<n≤200>[&inboundKind&inboundId]` — лента по `ts desc`.
- `GET stats?inboundKind&inboundId&range=1h|24h|7d|30d` — `range ≤ 24h` из current (`stepMs=bucket_ms`), дальше из rollup (`stepMs=step_ms`); серии по `monClientId × path`, точки `{t, nOk, nFail, latMin, latAvg, latMax, handshakeMs}`, отсутствующие бакеты — `null`; времена UTC ms, UI рисует в поясе браузера.
- `GET summary?range=24h|7d` — uptime/coverage/инциденты по inbound и path (тот же расчёт, что у дайджеста): `uptime = Σ n_ok / Σ (n_ok + n_fail)`, `coverage = пришедших бакетов / ожидаемых`, ожидаемые — только от mon-clients в `ONLINE` с этим path; пропуски не считаются ни успехом, ни сбоем.

### 7.5 Локализация

13 локалей, ключи в конец секций (начать с `en_US`/`ru_RU`): `menu.monitoring`; `pages.monitoring.title/live/stale/lastContact/monClients/targets/events/path/state/for/uptime24/latency/reason/noData/coverage/retired`; `pages.inbounds.health/filterDown/healthTooltip/probeBadge`; `pages.settings.monitoringSettings/monEnable/monEnableDesc/monToken/monTokenDesc/monTokenCopy/monTokenRegenerate/monTokenRegenerateConfirm/monStaleMinutes/monStaleMinutesDesc/monRetentionDays/monRetentionDaysDesc/monRollupRetentionDays/monRollupRetentionDaysDesc/monProbePeerLimit/monProbePeerLimitDesc/monProbeSet/monProbeSetDesc/monProbeSetRemove`; `tgbot.messages.monitoring.down/up/flappingOn/flappingOff/clientOffline/clientOnline/stale/staleBack/digestTitle/digestLine`.

## 8. Известные ограничения v1

- Один mon-server на панель: один `monToken`, один снимок реестра.
- STALE — единственное состояние, которое считает панель; при STALE строки `mon_targets` хранят последнее известное состояние.
- Rollup-серии при смене шага могут ненадолго содержать два `step_ms`; ручка stats отдаёт `stepMs` явно.
- Первый time-series и первый ретеншн в панели — образца в коде нет, тесты §11 обязательны.

## 9. Ручные проверки перед реализацией

Не блокируют спеку: поведение xray API при добавлении клиента в выключенный inbound (probe account должен создаваться и в нём); что `GetOnlineClients` для AWG отдаёт после фильтра; читаемость sparkline с `null`-дырами на 7d.

## 10. Тронутые файлы

### Новые (нулевой риск конфликтов)

- `database/model/monitoring.go` — четыре модели §2.1.
- `web/service/monitoring_probe.go` — `ProbePrefix`, `IsProbeAccount`, атрибуты probe-клиента.
- `web/service/monitoring_service.go` (+ `_test.go`) — §4.3.
- `web/service/setting_monitoring.go` — геттеры/сеттеры 13 ключей §2.2.
- `web/controller/monitoring.go` (+ `_test.go`) — контракт §4.1–4.2.
- `web/controller/monitoring_ui.go` — ручки §7.4.
- `web/job/monitoring_job.go` — §5.
- `web/html/monitoring.html`, `web/html/settings/panel/monitoring.html`.
- `docs/spec/monitoring-contract.md`, `docs/spec/monitoring-panel.md`, `docs/adr/0004-mon-server-single-source-panel-passive.md`.

### Вставки в upstream-файлы (по одному месту на файл, если не сказано иначе)

| файл : место | вставка |
|---|---|
| `database/db.go` : `initModels` | четыре `&model.Mon*{},` в конец среза |
| `database/db.go` : `namedIndexes` | индексы `idx_mon_*` |
| `web/service/setting.go` : `defaultValueMap`, форковый блок `proxyOverride*` | 13 ключей §2.2 |
| `web/entity/entity.go` : `AllSetting`, форковый блок | 13 полей |
| `web/web.go` : регистрация контроллеров; `startTask` | `NewMonitoringController(...)`; `addJob("@every 1m", …)`, `addJob("@hourly", …)` |
| `web/controller/api.go` : `initRouter` | одна строка — группа `monitoring` UI-ручек |
| `web/controller/xui.go` : `initRouter` | `g.GET("/monitoring", a.monitoring)` |
| `web/service/inbound.go` : `AddInbound`, `UpdateInbound`, `AddInboundClient`, `UpdateInboundClient`, `DelInbound`, `GetOnlineClients` | guard `IsProbeAccount` (4), каскад (1), фильтр (1) |
| `web/service/tunnel_service.go` : `AddClient`, `UpdateClient*`, `GetOnlineClients` | guard (2), фильтр (1) |
| `web/service/tgbot.go` : `SendReport`; `prepareServerUsageInfo`, online-кнопки, поиск | блок Monitoring (1); фильтр `IsProbeAccount` |
| `web/job/ldap_sync_job.go` : `deleteClientsNotInLDAP` | пропуск probe |
| `shared/ipam/ipam.go` : `AllocateIPv4`, `AllocateIPv6` | sentinel `ErrPoolExhausted` в ошибке «нет свободных адресов» (текст прежний) — ensure отличает полный пул от прочих сбоев |
| `web/html/inbounds.html` | колонка Health, фильтр down, бейдж `probe`, скрытая правка probe |
| `web/html/settings.html` | пятая вкладка |
| `web/html/component/aSidebar.html` | пункт Monitoring |
| `web/assets/js/model/setting.js` | 13 дефолтов |
| `web/translation/translate.*.toml` | ключи §7.5 в конец секций |
| `main.go` / CLI `x-ui` | показать / сбросить `monToken` по образцу `secret` |

### Не трогать

`sub/subService.go`, `sub/subController.go`, `database/model/tunnel.go`, `database/tunnel_migrate.go`, `inboundDataTables`/`datagen`, `web/html/awg.html`.

## 11. План реализации

Каждый шаг — отдельный коммит; тесты через `docker run --rm -v $PWD:/src -w /src golang:1.26 go test ./...`.

1. Модели §2.1 + `initModels` + `namedIndexes`; тест: AutoMigrate создаёт таблицы и индексы, каскад по `(inbound_kind, inbound_id)` чистит все четыре.
2. Настройки §2.2: ключи, поля, геттеры, `setting.js`; тест `AllSetting` round-trip, генерация `monToken` 32 символа.
3. `monitoring_probe.go` + guard в add/update-путях xray и AWG, tgbot, LDAP; фильтр в `GetOnlineClients`; тесты: `probe-*` отказывается на всех путях (включая новый `probe-*` в `AddInbound`/`UpdateInbound`; существующий проходит), LDAP-синк не удаляет probe, online-множество без probe.
4. `MonitoringService.State/EnsureProbeSet/ProbeConfigs/DeleteProbeSet`; тесты: ревизия детерминирована и меняется от `enable`/override/subId и от probe-материала (Reality `serverNames`/`target`/ключи/`shortIds`, порт и ключи AWG), но не от remark, ensure идемпотентен и дозаводит удалённого клиента, `409` без override и без набора, `direct` подставляет `host` (или публичный `Listen`), inbound с `externalProxy` даёт разные однострочные ссылки для `direct` и `proxy`, `?hop=` рендерит с адресом звена, `409 unknown_hop`/`hop_not_joined`, выключенный inbound отсутствует в `items`; AWG-пиры сверяются со снимком (добавление/удаление mon-client, `probe-awg` v1 удаляется, плохой `monClientId` → `400`, исчерпанный пул → `unallocated` и нет AWG-элемента, пиры на каждое звено только по `paths` mon-client'а, потолок `monProbePeerLimit` режет по приоритету, пара сверх лимита без AWG-элемента, `unallocated` с `reason`), AWG-элементы по одному на mon-client с `monClientId`, ревизия двигается от набора пиров.
5. `ApplyEvents` + `UpsertStats` с rollup в транзакции; тесты: смешанный батч (валидные приняты, невалидный в `rejected`), неизвестное поле игнорируется, `path` `edge:x` принят, дубликат `id`, событие старше `since`, `ignored` по неизвестному inbound, upsert по ключу, rollup пересчитывается, `lat_avg` взвешен по `n_ok`, бакет старше ретеншна → `ignored`.
6. Контроллер контракта + `checkMonAuth`; тесты: голый `404` во всех неавторизованных случаях, `X-Mon-Contract`, `413` на батч сверх лимита, `monLastContact` обновляется.
7. `MonitoringJob`: STALE, TTL probe-набора, ретеншн, пересборка rollup; тесты на каждом.
8. Telegram §6 + блок дайджеста в `SendReport`; тесты форматирования и `notified=true`.
9. UI-ручки §7.4; тесты: серии с `null`-дырами, выбор current/rollup по `range`, summary = дайджест.
10. UI: страница Monitoring, колонка Health и фильтр, вкладка настроек, бейдж `probe`, локализация `en_US`/`ru_RU`.
11. CLI `x-ui` для `monToken`; README (раздел Monitoring со ссылкой на репо `3ax-ui-monitoring`).
12. Ручная проверка с mon-server на стенде real/proxy: `GET /state` → ensure → `/probe/configs` по обоим path → события DOWN/UP в ленте и Telegram → STALE через 15 мин тишины.
