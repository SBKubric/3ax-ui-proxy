# Tunnel subscription: AWG/WG-конфиги в подписке

Спека и план реализации. Итог карты [Tunnel subscription: AWG/WG-конфиги в подписке без breaking changes к upstream](https://github.com/SBKubric/3ax-ui-proxy/issues/30); решения приняты в её тикетах, здесь они только собраны. Термины по [CONTEXT.md](../../CONTEXT.md): **tunnel subscription**, host override, proxy front, probe account. Принцип совместимости с upstream зафиксирован в [ADR 0002](../adr/0002-additive-upstream-compatibility.md).

## 1. Цель и границы

**Tunnel subscription** — публичный маршрут подписки, который по subId отдаёт клиентские конфиги AmneziaWG и WireGuard той же подписки. Он дополняет xray-подписку (`/sub`, `/json`, `/clash`), не меняя её. Потребители: страница подписки для людей (панель и proxy front) и mon-server карты мониторинга, которому нужен .conf probe account'а без входа в панель.

Рамка:

- **Совместимость с upstream = merge-ability.** Новые файлы и функции вместо правки существующих; без переименований; минимальные точки вставки в upstream-файлы (перечислены в §11). Поведение и заголовки `/sub`, `/json`, `/clash` не меняются. Схема БД только аддитивна: база форка открывается upstream-бинарником.
- AWG и WG вместе: одна модель `TunnelClient`, один generic `TunnelService[K]`, один маршрут.
- Вне scope: MTProto (его клиенты не живут в xray-подписке), изменение формата старых маршрутов, объединение трафика туннелей в их `Subscription-Userinfo`, отправка фичи PR'ом в upstream.

## 2. Модель данных

Новая таблица-связка `tunnel_client_subs`, новая модель `database/model/tunnel_subscription.go`. Модель `TunnelClient` не трогаем: тесты полноты полей legacy↔merged (`tunnel_migrate_test.go`, `awgwg_baseline_test.go`) упали бы от нового поля.

| поле | тип | назначение |
|---|---|---|
| `client_uuid` | string, PK | uuid туннельного клиента; один subId на клиента |
| `kind` | string (`awg` / `wg`) | денормализован: сервис сразу знает, какой `TunnelService[K]` звать |
| `sub_id` | string, index, not null | подписка |
| `created_at`, `updated_at` | time | служебные |

- FK `client_uuid → tunnel_clients.uuid` с `ON DELETE CASCADE`, объявлен **только на новой модели** (`tunnel_clients.uuid` уже имеет unique index; в `database/db.go` включён `foreign_keys=ON` и в DSN, и PRAGMA). Удаление клиента любым путём (`DeleteClient`, `DeleteClientByUUID`, `DeleteAllClients`, CLI) чистит связку без правки upstream-сервиса.
- Чтение — через join с `tunnel_clients`, чтобы сироты не просочились, если PRAGMA когда-нибудь выключат.
- Регистрация: `&model.TunnelClientSub{},` в конец среза `initModels` (`database/db.go`). AutoMigrate создаёт таблицу; миграций данных нет. `inboundDataTables`/`datagen` не трогаем.
- subId — любая непустая строка, как у xray-клиентов; совпадение с subId xray-клиента разрешено и желательно (одна подписка на пользователя).

## 3. Сервис

Новый файл `web/service/tunnel_subscription_service.go`, `TunnelSubscriptionService`:

- `Set(uuid, kind, subId)`, `Clear(uuid)`, `SubIdsByUUIDs([]string) map[string]string`.
- `ClientsBySubId(subId) []TunnelSubEntry{Kind, Client model.TunnelClient, Conf string}` — через публичные `GetClientByUUID` + `GetClientConfigByUUID` соответствующего `AwgService`/`WgService`. Форковый `renderClientConfig` уже применяет host override к `Endpoint` (`withProxyOverride`; порт не меняется, он extra port на proxy front).
- `Userinfo(entries) (header string, enable bool)` — по алгоритму `/sub` (`sub/subService.go:buildSubs`): `upload`/`download` — сумма; `total` — сумма `TotalGB`, но 0, если хоть у одного клиента безлимит; `expire` — общее значение `ExpiryTime`, иначе 0; `enable` = есть хоть один включённый.
- **Кэш**: свой TTL-кэш 10 с по subId (по образцу `sub/inbound_cache.go`, без `datagen`, который на `tunnel_clients` не реагирует). Сбрасывается из `Set`/`Clear` и из хендлеров add/update; правки клиента через upstream-пути (`toggle`, `resetTraffic`, `del`) кэш не видят — допустимая задержка до 10 с, как у `/sub`.
- **Отключённые / просроченные / исчерпавшие лимит** клиенты отдаются как у `/sub`: элемент присутствует с `enable` и `expiryTime`; пир у отключённого и так снят с интерфейса.

## 4. API панели

Решение владельца: subId — поле в существующих ручках `/panel/api/{awg,wg}/…`, а не отдельный endpoint. Это **4 малых hunk'а** в `web/controller/tunnel_controller.go` (файл спокойный: 2 коммита за полгода) — единственные правки контроллера.

- DTO-обёртка: `type tunnelClientReq struct { model.TunnelClient; SubId *string \`json:"subId"\` }`. `nil` — поле не прислано, связка не трогается; `""` — отвязать; непустая строка — привязать.
- `POST /client/add`: после успешного `AddClient` — `Set(uuid, kind, subId)`, если прислан непустой. Ответ содержит `subId`.
- `POST /client/update/:id`, `POST /client/updateByUuid/:uuid`: после успеха — `Set`/`Clear` по правилу выше.
- `GET /clients`: ответ оборачивается в тот же DTO с заполненным `subId` (через `SubIdsByUUIDs`). Старые потребители видят лишнее поле и не замечают.
- **Probe account для mon-server**: один вызов `POST /panel/api/{awg,wg}/client/add` с `subId` в теле, затем `/tun/<subId>` как обычный клиент.

Запасной вариант, если hunk'и в контроллере станут конфликтовать с upstream: отдельный аддитивный `POST /client/sub/:uuid {subId}` + `GET /client/subs` в новом файле (research §4.1). Он даёт то же поведение за два вызова.

## 5. Настройки

По образцу `subJson*`/`subClash*`:

| ключ | default | назначение |
|---|---|---|
| `subTunEnable` | `"true"` | включатель маршрута; без привязок маршрут ничего не отдаёт, поэтому включён |
| `subTunPath` | `"/tun/"` | префикс маршрута |
| `subTunURI` | `""` | ручной override публичного URL, как `subJsonURI` |

- `defaultValueMap` (`web/service/setting.go`) и `AllSetting` (`web/entity/entity.go`) — внутри уже форковых hunk'ов `proxyOverride*`. Тег `json` обязан совпадать с ключом `defaultValueMap`.
- Нормализация слэшей `subTunPath` — одна строка в `CheckValid` рядом с `SubClashPath`.
- Геттеры `GetSubTunEnable/GetSubTunPath/GetSubTunURI` — новый файл `web/service/setting_tunnel_sub.go`.
- `GetDefaultSettings`: одна строка `result["subTunEnable"]`, `result["subTunURI"] = subURI + subTunPath` для инфо-модалки. (Запасной вариант при конфликтах: обёртка `AddTunnelSubDefaults` в `web/controller/setting.go:getDefaultSettings`, research §4.2.)
- `web/assets/js/model/setting.js`: три дефолта.
- UI: переключатель в `settings/panel/subscription/general.html` (форковый hunk `proxyOverride`), путь и URI в `subscription/json.html` под `v-if="allSetting.subTunEnable"`; вкладка показывается при любом из трёх включателей.
- Миграций не нужно: ключ появляется при первом сохранении настроек. Применяется после «Restart panel», как остальные пути подписки.

## 6. Маршрут и ответ

- `sub/sub.go:initRouter` — один блок после `NewSUBController`: `if subTunEnable { s.tun = NewTunnelSubController(g.Group(subTunPath), …) }`. Контроллер — новый файл `sub/tunnelController.go`, `GET :subid`. Публичный, subId — секрет, как у `/sub`. `SUBController`/`NewSUBController` не расширяем.
- **Ответ**: всегда `200` и JSON-массив (`application/json; charset=utf-8`), в том числе `[]` для неизвестного subId и для subId без туннелей — перебор по коду ответа невозможен. `Accept` игнорируется: `/tun` всегда JSON.

```json
[
  {"kind":"awg","name":"ivan-phone","uuid":"…","filename":"ivan-phone","enable":true,"expiryTime":0,"conf":"[Interface]\n…"},
  {"kind":"wg","name":"ivan-laptop","uuid":"…","filename":"ivan-laptop","enable":false,"expiryTime":1767225600000,"conf":"[Interface]\n…"}
]
```

- `filename` санирует панель: из `name` оставить `[a-zA-Z0-9_=+.-]`, обрезать до 15 символов (лимит имени интерфейса), пусто → `<kind><порядковый номер>`; дубликаты в пределах ответа — суффикс `-2`, `-3`.
- `conf` — вывод `GetClientConfigByUUID`, с уже применённым host override к `Endpoint`.
- **Заголовки** при непустом ответе — та же экспортированная `ApplyCommonHeaders`: `Subscription-Userinfo` (§3), `Profile-Update-Interval`, `Profile-Title`, `Support-Url`, `Profile-Web-Page-Url` = страница `/sub/<subId>`, `Announce`, `Routing-*` — как есть, ради паритета. При `[]` заголовки профиля не ставятся.
- Кэша у контроллера нет: он читает `ClientsBySubId` с его TTL-кэшем.

## 7. Proxy front

`proxy/`:

- Поле `tunPath` в `Config` (default `"/tun/"`); старые `proxy.json` продолжают работать.
- `handleTun` по образцу `handleJson`: `fetchUpstream(cfg.TunPath, subid)`, ответ `application/json`, `copyHeaders` + замена `Profile-Web-Page-Url` на свой `/sub/<subId>`. Upstream `[]` проходит как есть, сбоем не считается.
- Страница proxy front (`proxy/subpage.html`) получает блок *Tunnels* серверным fetch'ем `/tun` внутри `renderPage` (у страницы нет JS-доступа к upstream); QR — PNG data-URI через уже используемый `qrcode`.
- Установщик и README: новый ключ конфига с дефолтом.

## 8. UI панели

Выбран вариант A прототипа ([артефакт](https://claude.ai/code/artifact/cd006a14-e843-4f2e-b4c3-919f08b389ef), [исходник](https://github.com/SBKubric/3ax-ui-proxy/blob/prototype/tunnel-subscription-ui/docs/prototypes/tunnel-subscription-ui.html)).

1. **Модалка AWG/WG-клиента** — то же поле *Subscription* с кнопкой генерации (16 символов) и подсказкой существующих subId, что у xray-клиента; пустое = не в подписке. Реализация: снять `inbound.protocol !== Protocols.AMNEZIAWG && inbound.protocol !== Protocols.NATIVEWG` из `v-if` в `web/html/form/client.html:159-160` (одна строка) и пробросить `subId` в payload `addAwgClient/updateAwgClient/addNativewgClient/updateNativewgClient` в `inbounds.html` (шаблон уже есть у MTProto: `subId: c.subId || ""`). Маппинг pseudo-inbound'а WG/AWG (`subId: ""` в `inbounds.html:1848`, `:1925`) → `subId: c.subId || ""`, поле приходит из обогащённого `GET /clients` (§4). Хинт — новая строка локализации.
2. **Таблица клиентов** — новая колонка *Subscription* с тегом subId (`—`, если нет) в `innerColumnsWg`/`innerMobileColumnsWg`; кнопка «ссылка подписки» в Operate только у привязанных. Инфо-модалка показывает `app.subSettings.subTunURI + subId` для AWG/WG (два малых hunk'а в `inbound_info_modal.html`); `this.subSettings` в `inbounds.html` дополняется `subTunEnable`, `subTunURI`.
3. **Страница подписки панели** (`settings/panel/subscription/subpage.html`) — аддитивная секция *Tunnels* под xray-конфигами. Карточка на конфиг: тег протокола, имя, `disabled` при `enable=false`, текст .conf, QR из текста .conf (уровень L, не меньше 4 px/модуль: ≥ 168 px для AWG 2.0, ≈ 232 px для AWG 3.x), кнопки *Download <filename>.conf* (главная; `Blob` + атрибут `download` с `filename` из ответа) и *Copy*, подпись «AmneziaVPN / AmneziaWG / WireGuard, автообновления нет». Данные — fetch `/tun/<subId>`; при `[]` секция показывает «No tunnel configs».
4. **Страница proxy front** — та же секция *Tunnels* после *Configs* (§7); в *Apps* добавить Amnezia и AmneziaWG. Страница остаётся английской.

### Локализация

13 локалей, ключи добавляются **в конец** секций (отсутствующий ключ падает в имя ключа, поэтому можно начать с `en_US`/`ru_RU`):

| ключ | en_US |
|---|---|
| `pages.inbounds.subscriptionTunnelDesc` | Leave empty to keep this peer out of any subscription. Use the same ID as an Xray client to share one subscription. |
| `pages.inbounds.tunnelSubscription` | Subscription |
| `pages.inbounds.notInSubscription` | Not in a subscription |
| `pages.settings.subTunEnable` | Enable tunnel subscription |
| `pages.settings.subTunPath` | Tunnel subscription path |
| `pages.settings.subTunPathDesc` | Serves AmneziaWG and WireGuard configs of a subscription as JSON |
| `pages.settings.subTunURI` | Tunnel subscription URI |
| `subscription.tunnels` | Tunnels |
| `subscription.downloadConf` | Download .conf |
| `subscription.copyConf` | Copy config |
| `subscription.tunnelHint` | Open with AmneziaVPN or the AmneziaWG / WireGuard app. Changes are not pushed: download again after the server changes. |
| `subscription.noTunnels` | No tunnel configs in this subscription |

## 9. Известные ограничения v1

- **Подписка только из туннелей** (без xray-клиента) страницы не имеет: `/sub` отвечает 400. Оператор выдаёт .conf из панели или ссылку `/tun`; mon-server страница не нужна. `sub/subService.go` не трогаем.
- **Автообновления у клиентов нет**: ни AmneziaVPN, ни нативные AmneziaWG/WireGuard не умеют подписок, ре-фетча или аналога `Profile-Web-Page-Url`; страница честно пишет «скачайте заново после изменений». Смена proxy front (host override) требует повторного импорта .conf.
- Официальный WireGuard отвергает AWG-ключи в .conf; для `kind=awg` страница рекомендует AmneziaVPN/AmneziaWG.
- Кэш 10 с не сбрасывается правками клиента через upstream-пути (`toggle`, `resetTraffic`, `del`).

## 10. Ручные проверки перед реализацией UI

Не блокируют спеку (из research по форматам импорта): читаемость QR AWG 3.x при 232–450 px на реальных телефонах; открытие скачанного .conf из Safari на iOS; при желании — `vpn://` с сырым .conf на Android (кнопка только для Android, опционально, не в v1).

## 11. Тронутые файлы

### Новые (нулевой риск конфликтов)

- `database/model/tunnel_subscription.go` — модель `TunnelClientSub` (§2).
- `web/service/tunnel_subscription_service.go` (+ `_test.go`) — сервис и TTL-кэш (§3).
- `web/service/setting_tunnel_sub.go` — геттеры настроек (§5).
- `sub/tunnelController.go` (+ `sub/tunnel_test.go`) — маршрут (§6).
- `docs/spec/tunnel-subscription.md`, `docs/adr/0002-additive-upstream-compatibility.md`.

### Вставки в upstream-файлы (по одному месту на файл, если не сказано иначе)

| файл : место | вставка |
|---|---|
| `database/db.go` : срез в `initModels` | `&model.TunnelClientSub{},` в конец |
| `web/service/setting.go` : `defaultValueMap`, форковый блок `proxyOverride*` | три ключа §5 |
| `web/service/setting.go` : `GetDefaultSettings` | одна строка `subTunEnable`/`subTunURI` |
| `web/entity/entity.go` : `AllSetting`, форковый блок; `CheckValid` | три поля; одна строка нормализации слэшей |
| `sub/sub.go` : `initRouter` после `NewSUBController`; поле `tun` в `Server` | блок §6 |
| `web/controller/tunnel_controller.go` : `addClient`, `updateClient`, `updateClientByUUID`, `getClients` | 4 hunk'а §4 |
| `web/assets/js/model/setting.js` : дефолты | три строки |
| `web/html/settings/panel/subscription/general.html` : форковый блок `proxyOverride` | switch `subTunEnable` |
| `web/html/settings/panel/subscription/json.html` | путь и URI под `v-if` |
| `web/html/form/client.html:159-160` | снять условие AWG/WG в `v-if` |
| `web/html/inbounds.html` | 4 hunk'а: маппинг `subId` WG/AWG, `this.subSettings`, payload add/update AWG/WG, колонка в `innerColumnsWg` |
| `web/html/modals/inbound_info_modal.html` | 2 hunk'а: ссылка `subTunURI + subId` для AWG/WG |
| `web/html/settings/panel/subscription/subpage.html` | аддитивная секция *Tunnels* в конце |
| `web/translation/translate.*.toml` | ключи в конец секций |
| `proxy/config.go`, `proxy/subserver.go`, `proxy/subpage.html`, `install.sh`, `README*.md` | форковые файлы, §7 |

### Не трогать

`sub/subService.go`, `sub/subController.go` (21-параметрный конструктор), `web/service/tunnel_service.go` (кроме уже форкового `renderClientConfig`), `database/model/tunnel.go`, `database/tunnel_migrate.go`, `web/html/awg.html` (страница сервера, churn 23), `inboundDataTables`/`datagen`.

## 12. План реализации

Каждый шаг — отдельный коммит, тесты через `docker run --rm -v $PWD:/src -w /src golang:1.26 go test ./...`.

1. Модель `TunnelClientSub` + строка в `initModels`; тест: AutoMigrate создаёт таблицу с FK, удаление `TunnelClient` каскадно удаляет связку.
2. `TunnelSubscriptionService`: Set/Clear/SubIdsByUUIDs/ClientsBySubId/Userinfo + TTL-кэш; тесты на join (сирота не возвращается), агрегацию Userinfo (безлимит → total 0, разные expire → 0), санацию `filename`, инвалидацию кэша.
3. Настройки: ключи, поля, геттеры, `CheckValid`, `GetDefaultSettings`, `setting.js`, UI Settings; тест `AllSetting` round-trip.
4. Маршрут `sub/tunnelController.go` + регистрация в `sub/sub.go`; тесты: `[]` для неизвестного subId, JSON-схема элемента, заголовки при непустом ответе и их отсутствие при пустом, `Accept: text/html` → JSON, override в `Endpoint`.
5. API: DTO и 4 hunk'а в `tunnel_controller.go`; тесты: add с subId создаёт связку, update с `""` удаляет, `nil` не трогает, `GET /clients` несёт `subId`.
6. UI панели: модалка, таблица, инфо-модалка, страница подписки, локализация `en_US`/`ru_RU`.
7. Proxy front: `tunPath`, `handleTun`, блок на странице, installer/README; тесты по образцу `subserver_test.go`.
8. Ручная проверка на стенде real/proxy по runbook: привязка AWG-клиента к subId xray-клиента, `/tun` через proxy front с адресом proxy в `Endpoint`, импорт .conf по QR в AmneziaVPN, mon-server-сценарий (add с subId → `/tun`).
9. Обновить `docs/runbooks/proxy-front.md` разделом о tunnel subscription.
