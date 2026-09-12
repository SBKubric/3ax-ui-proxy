# Состояние upstream 3ax-ui вокруг подписок и туннельных клиентов; точки вставки для tunnel subscription

Исследование по тикету [#31](https://github.com/SBKubric/3ax-ui-proxy/issues/31) (часть карты [#30](https://github.com/SBKubric/3ax-ui-proxy/issues/30)).
Дата: 2026-09-12. Upstream — [coinman-dev/3ax-ui](https://github.com/coinman-dev/3ax-ui), подключён как remote `upstream` и прочитан через `git`; ссылки на строки даны для `upstream/main` = `38a02c1d` (v1.8.1) и для форка `origin/main` = `4d097721` (v1.8.1.5). Термины — по [CONTEXT.md](../../CONTEXT.md) и карте #30: tunnel subscription, host override, proxy front.

## TL;DR

- **Upstream не сдвинулся с момента расхождения.** `git merge-base origin/main upstream/main` = `38a02c1d` = `upstream/main` = тег `v1.8.1` (2026-09-09). Форк — 26 коммитов сверху. В `upstream/dev` один коммит вперёд (`7abd2aed`, 2026-09-09: `addColumn` в `database/db.go` перестаёт ругаться на «duplicate column» + тест). Он придёт в следующий мерж; наш дизайн его не задевает.
- **Ни subId у туннельных клиентов, ни туннельной подписки, ни планов у upstream нет.** Issues в репозитории выключены, PR один и закрыт (#1, май, TPROXY-цепочка, уже влит через `dev`), веток кроме `main`/`dev` нет. Напротив, upstream *осознанно* прячет поле Subscription для AWG/WG в форме клиента (`web/html/form/client.html:159-160`, с `9056fc9e`, апрель) и при маппинге туннельных клиентов в псевдо-inbound подставляет `subId: ""` (`web/html/inbounds.html:1848`, `:1925`).
- **Поправка к карте #30 (Out of scope):** клиенты MTProto имеют `SubId` в модели (`database/model/mtproto.go:33`), но в xray-подписке **не живут**: `getInboundsBySubId` фильтрует `protocol in ('vmess','vless','trojan','shadowsocks','hysteria','hysteria2')` (`sub/subService.go:157-163`), а инфо-модалка явно прячет для них ссылку подписки (`web/html/modals/inbound_info_modal.html:385-389`). MTProto остаётся вне tunnel subscription, но по другой причине: он вообще ни в какой подписке.
- **Образец «новый маршрут + настройки» у upstream** (§3): два ключа в `defaultValueMap` (`subXEnable`, `subXPath`) → геттеры `getBool`/`getString` → поля в `entity.AllSetting` (`json`-тег = ключ) → нормализация слэшей в `CheckValid` → чтение в `sub.Server.initRouter` и передача в `NewSUBController` → `g.Group(path).GET(":subid", handler)` под `if enabled` → JS-дефолты в `web/assets/js/model/setting.js` → переключатель в `settings/panel/subscription/general.html`, путь в `.../json.html` → выдача `subXURI` фронту через `GetDefaultSettings`. Миграций для новых ключей нет: строка в `settings` появляется при первом сохранении настроек.
- **Рекомендация по вставкам (§4):** почти всё — новыми файлами (модель-связка, сервис, контроллер подписки, геттеры настроек, API привязки). В upstream-файлы — шесть однострочных/малых вставок в Go: `database/db.go:initModels` (1 строка), `sub/sub.go:initRouter` (1 блок после `NewSUBController`), `web/service/setting.go:defaultValueMap` и `web/entity/entity.go:AllSetting` (внутри уже форковых hunk'ов `proxyOverride*`), `web/controller/api.go` (1 строка после `wgGroup`), `web/controller/setting.go:getDefaultSettings` (1 строка). UI-правки неизбежно попадают в `inbounds.html` и `form/client.html` — держим их в 4 точечных hunk'ах.
- **Чего избегать (§5):** `sub/subService.go` (`BuildURLs`/`getBaseSchemeAndHost` — уже форковые патчи поверх nginx-правок upstream), `web/service/setting.go:GetDefaultSettings`, `sub/subController.go`, `web/html/awg.html` (это страница *сервера* AWG/WG, не клиентов — 23 коммита за полгода), `database/model/tunnel.go`, `web/service/tunnel_service.go` (кроме уже форкового `renderClientConfig`); переводы правим только добавлением ключей в конец секций.

---

## 1. Где upstream относительно форка

| Что | Значение | Источник |
|---|---|---|
| merge-base `origin/main`…`upstream/main` | `38a02c1d` «Merge dev into main for v1.8.1: everything behind port 443», 2026-09-09 | `git merge-base` |
| Коммитов upstream после merge-base | **0** (`upstream/main` == merge-base) | `git rev-list --count 38a02c1d..upstream/main` |
| Коммитов форка после merge-base | 26 (v1.8.1.1 → v1.8.1.5; proxy front, host override, relay manifest) | `git rev-list --count 38a02c1d..origin/main` |
| `upstream/dev` | 1 коммит вперёд `main`: `7abd2aed` fix(db) — `addColumn` терпит «duplicate column» (`database/db.go`, `database/migration_test.go`); при этом `dev` отстаёт от `main` на 10 merge-коммитов (upstream мержит `dev`→`main` merge-коммитами, `dev` не перематывает) | `git log upstream/main..upstream/dev`, `gh api repos/coinman-dev/3ax-ui/branches` |
| Issues | выключены в репозитории | `gh issue list` → «has disabled issues» |
| PR | один: #1 «feat(wg/awg): route tunnel traffic through Xray (TPROXY chain)», закрыт 2026-05-13 без merge (влит вручную: `702b5075` «Merge feat/wg-awg-route-via-xray into dev») | `gh pr list --state all` |
| Релизы | v1.6.4/v1.6.5 (25 июня), v1.6.12 (23 авг), v1.7.0 (24 авг), v1.8.1 (9 сент); теги `-beta` между ними; 389 коммитов за 6 месяцев | `gh release list`, `git log --since='6 months'` |
| Второй upstream | 3ax-ui сам мержит [MHSanaei/3x-ui](https://github.com/MHSanaei/3x-ui): `01f50540` «Merge upstream 3x-ui v2.9.4 into 3AX-UI» (2026-06-14) — через такие мержи в `sub/` и `web/html/inbounds.html` прилетают пачки чужих правок | `git log --grep='Merge upstream 3x-ui'` |

### 1.1. Есть ли у upstream subId у туннельных клиентов или туннельная подписка — нет

- `database/model/tunnel.go` — `TunnelClient` без `SubId` (поля: `Id, ServerId, UUID, Name, Email, Enable, Comment`, ключи, адреса, `AllowedIPs, ClientAllowedIPs, ForwardedPorts, PersistentKeepalive, Upload/Download/TotalGB/AllTime, LastPeerUp/Down, ExpiryTime, Reset, LimitIp, TgId, LastOnline, LastIP, CreatedAt, UpdatedAt`).
- `web/html/form/client.html:159-160`: поле Subscription показывается с условием `inbound.protocol !== Protocols.AMNEZIAWG && inbound.protocol !== Protocols.NATIVEWG` — исключение внесено `9056fc9e` (2026-04-08, «feat: complete wg native panel parity») и с тех пор не менялось.
- `web/html/inbounds.html:1840-1855` (WG) и `:1916-1931` (AWG): при построении псевдо-inbound'а из `/panel/api/{wg,awg}/clients` клиенту пишется `subId: ""`; в payload'ах `addAwgClient`/`updateAwgClient`/`addNativewgClient`/`updateNativewgClient` (`:2755-2900`) `subId` не передаётся. Для MTProto в тех же местах `subId: c.subId || ""` (`:2029`, `:2914`, `:2943`) — то есть шаблон «пробросить subId в payload» в файле уже есть, копировать его легко.
- `sub/` не знает о туннелях вовсе: `git grep -i 'tunnel\|awg\|wireguard' upstream/main -- sub/` пусто.
- `git log -i --grep='subid\|subscription\|tunnel'` за 6 месяцев — только nginx-фронт (ссылки подписки на 443), Hiddify-режим, объединение AWG/WG, TPROXY; ничего про подписку туннелей.

### 1.2. Что upstream делал в интересующих зонах за последние 6 месяцев (важно для дизайна)

- **`84061ea7` (2026-08-22) «full-code audit fixes and one implementation for AWG/WG»**: четыре legacy-таблицы `awg_*/wg_*` слиты в `tunnel_servers`/`tunnel_clients`; `web/service/awg_service.go` и `wg_service.go` удалены, `AwgService`/`WgService` стали алиасами `TunnelService[awgKind]`/`TunnelService[wgKind]` (`web/service/tunnel_service.go:30-47`); `web/controller/awg_controller.go`/`wg_controller.go` заменены одним `tunnel_controller.go` (`NewAwgController`/`NewWgController` → `newTunnelController`, `:57-70`). Legacy-таблицы остались «на один релиз» (`database/tunnel_migrate.go`). Отсюда: «AWG и WG вместе, один generic-сервис» из карты — уже факт upstream, наш сервис просто параметризуется тем же `kindProvider`.
- **nginx-фронт (`ccc3cd38`, `6cd4a27c`, `165d852b`, 2026-08-24)**: правки в `sub/subService.go` (`BuildURLs`, `getBaseSchemeAndHost`, `resolveInboundAddress`) и `web/service/setting.go:GetDefaultSettings` (`PublicSubBase()`); форк затем накрыл ровно эти же функции host override'ом (`git diff 38a02c1d origin/main -- sub/subService.go`). Это уже «горячая» зона для мержей — новых вставок туда не добавлять.
- **`database/db.go:209-214`**: `inboundDataTables` (таблицы, запись в которые сбрасывает кэш подписки через `datagen.Bump()`) до сих пор перечисляет `awg_clients`/`wg_clients`, а не `tunnel_clients`. То есть на запись туннельных клиентов кэш `/sub` не сбрасывается — для xray-подписки это неважно, но для tunnel subscription на `datagen` полагаться нельзя: нужен либо собственный кэш с инвалидацией по записи в новую таблицу-связку и `tunnel_clients`, либо обходиться без кэша (выборка по индексированному `sub_id` в маленькой таблице дешёвая; нагрузка `/sub` объяснена в `sub/inbound_cache.go:11-19` тем, что там `JSON_EACH` по всем inbound'ам — у нас такого нет).
- **`upstream/dev` `7abd2aed`**: только `addColumn` в `db.go:540-550`; мы `addColumn` не трогаем.

## 2. Churn upstream за 6 месяцев (`git log --since='6 months' --name-only upstream/main`)

Всего 389 коммитов. Файлы, которые задевает дизайн (число коммитов, где файл менялся):

| Файл | Коммитов | Комментарий |
|---|---|---|
| `web/translation/translate.ru_RU.toml` / `en_US.toml` | 53 / 52 | остальные 11 локалей — по 30-32; правки в конец секций почти не конфликтуют |
| `web/html/inbounds.html` | **40** | здесь живёт маппинг AWG/WG-клиентов и payload'ы add/update — UI-вставки неизбежны |
| `web/service/inbound.go` | 30 | не трогаем |
| `web/html/awg.html` | **23** | страница настроек *сервера* AWG/WG (табы, obfuscation); клиентов там нет — **не трогаем** |
| `web/service/awg_service.go` | 22 | файл удалён в `84061ea7`, churn исторический |
| `sub/subService.go` | **20** | генерация ссылок; форк уже патчит `BuildURLs`/`getBaseSchemeAndHost`/`resolveInboundAddress` — не добавлять |
| `web/html/modals/inbound_info_modal.html` | 18 | блок Subscription URL (`:385-420`, `:768-771`) |
| `database/db.go` | 16 | `initModels`, pre-migrate, `inboundDataTables` |
| `web/service/setting.go` | 15 | `defaultValueMap`, `GetDefaultSettings` (nginx-правки авг.) |
| `web/html/form/client.html` | 14 | одна строка с условием AWG/WG |
| `web/html/component/aClientTable.html` | 9 | иконки действий; условие `record.isAmneziawg \|\| record.isNativewg` уже есть |
| `sub/subJsonService.go` | 8 | форк патчит `GetJson`/`getConfig` |
| `web/controller/api.go` | 7 | регистрация групп `/awg`, `/wg` (`:55-61`) |
| `sub/subClashService.go` | 6 | не трогаем |
| `web/html/settings.html` | 5 | табы настроек (`:80-88`); форк уже добавил `showRelayManifest` |
| `web/entity/entity.go` | 5 | `AllSetting`, `CheckValid` |
| `web/service/tunnel_service.go` | 5 | все 5 — авг. 2026 (создан в `84061ea7`); форк добавил `renderClientConfig`/`withProxyOverride` |
| `sub/sub.go` / `sub/subController.go` | 5 / 5 | последняя правка `84061ea7`; стабильные |
| `web/assets/js/model/setting.js` | 5 | JS-дефолты настроек |
| `web/html/settings/panel/subscription/general.html` / `json.html` | 3 / 1 | форк уже добавил в `general.html` блок `proxyOverride` |
| `database/model/tunnel.go` | 3 | все — авг. 2026 |
| `web/controller/tunnel_controller.go` | 2 | самый спокойный из контроллеров |
| `web/controller/setting.go` | 2 | `getDefaultSettings` — удобная точка для обёртки |

Вывод: самые «горячие» файлы (`inbounds.html`, `subService.go`, `awg.html`, `setting.go:GetDefaultSettings`) — либо не трогаем, либо ограничиваемся точечными hunk'ами в стабильных функциях.

## 3. Как upstream регистрирует маршрут подписки и его настройки (образец `subJsonPath`/`subClashPath`)

Пошагово, с точными местами:

1. **Дефолты** — `web/service/setting.go:27-…` `defaultValueMap`: `"subJsonEnable": "false"` (`:56`), `"subJsonPath": "/json/"` (`:74`), `"subJsonURI": ""` (`:75`), `"subClashEnable": "true"` (`:76`), `"subClashPath": "/clash/"` (`:77`), `"subClashURI": ""` (`:78`). Значения — строки; `getString` берёт из БД, при `IsNotFound` — из `defaultValueMap` (`:269-281`), иначе ошибка `key <…> not in defaultValueMap` (`:274`). **Миграций для новых ключей нет**: строка в `settings` появляется при первом `UpdateAllSetting`, который переписывает *все* поля `AllSetting` через `saveSetting` (`:785-803`).
2. **Геттеры** — `GetSubJsonEnable` (`:521`, `getBool`), `GetSubJsonPath` (`:568`, `getString`), `GetSubClashEnable`/`GetSubClashPath` (`:624-630`). Особый случай `GetSubPath` (`:557-566`): если значение совпало с дефолтом (случайный `/sub-xxxx/`), он сразу сохраняет его в БД, чтобы путь не менялся между рестартами.
3. **Сущность** — `web/entity/entity.go:58-87` `AllSetting`: `SubJsonEnable bool \`json:"subJsonEnable" form:"subJsonEnable"\``, `SubJsonPath string`, `SubClashEnable`, `SubClashPath`, `SubJsonURI`, `SubClashURI`. `GetAllSetting` (`setting.go:149-230`) и `UpdateAllSetting` заполняют/сохраняют поля рефлексией по `json`-тегу — **имя тега обязано совпадать с ключом `defaultValueMap`**, иначе поле молча не заполнится (`:180-183`). Нормализация слэшей — `CheckValid` (`entity.go:162-181`): `HasPrefix "/"`, `HasSuffix "/"` для `SubPath`, `SubJsonPath`, `SubClashPath`.
4. **Чтение в sub-сервере** — `sub/sub.go:68` `(*Server).initRouter`: `GetSubPath/GetSubJsonPath/GetSubClashPath/GetSubJsonEnable/GetSubClashEnable` (`:86-108`), затем `g := engine.Group("/")` (`:270`) и `s.sub = NewSUBController(g, LinksPath, JsonPath, ClashPath, subJsonEnable, subClashEnable, …)` (`:272-275`). Sub-сервер поднимается в `main.go:65-67` и пересоздаётся по `SIGHUP` (`main.go:80-110`) — так настройки применяются после «Restart panel»; отдельного hot-reload нет.
5. **Маршруты** — `sub/subController.go:85-96` `initRouter`: `gLink := g.Group(a.subPath); gLink.GET(":subid", a.subs)`; `if a.jsonEnabled { gJson := g.Group(a.subJsonPath); gJson.GET(":subid", a.subJsons) }`; аналогично Clash (`:92-95`). Хендлеры (`:99-215`) через `a.subService.ResolveRequest(c)` получают scheme/host, отдают `400 "Error!"` при пустом результате, ставят заголовки `ApplyCommonHeaders` (`:217-247`: `Subscription-Userinfo`, `Profile-Update-Interval`, `Profile-Title`, `Support-Url`, `Profile-Web-Page-Url`, `Announce`, `Routing-Enable`, `Routing`). Хендлер `subs` при `Accept: text/html` рендерит `subpage.html` (`:112-160`).
6. **JS-дефолты** — `web/assets/js/model/setting.js:29-58`: `this.subJsonEnable = false; this.subJsonPath = "/json/"; this.subClashEnable = true; this.subClashPath = "/clash/"; … this.subJsonURI = ""; this.subClashURI = ""`.
7. **UI настроек** — переключатели в `web/html/settings/panel/subscription/general.html` (`<a-switch v-model="allSetting.subJsonEnable">`, `…subClashEnable`, панель `key="1"`); пути и URI — в `web/html/settings/panel/subscription/json.html:4-38` с `v-if="allSetting.subJsonEnable"`, `@input` вырезает `[:*]`, `@blur` дописывает слэши (`:8-10`). Вкладка «JSON» показывается при `allSetting.subJsonEnable || allSetting.subClashEnable` (`web/html/settings.html:82`). Переводы — секция `[pages.settings]` (`translate.en_US.toml:568+`, ключи `subEnable`, `subJsonEnable`, `subPath`, `subPathDesc`, `subURI`…); для Clash upstream ключей не заводил, заголовки захардкожены по-английски (`general.html:22-24`, `json.html:23`).
8. **Ссылки для панели** — `setting.go:837-945` `GetDefaultSettings(host)`: карта `subEnable/subJsonEnable/subClashEnable/subURI/subJsonURI/subClashURI/…` и вычисление `subURI` (`PublicSubBase()` nginx → cert/port → `subDomain`/host запроса), затем `result["subJsonURI"] = subURI + subJsonPath` (`:932-937`). Фронт читает это в `inbounds.html:2096-2102` (`this.subSettings = {enable, subTitle, subURI, subJsonURI, subJsonEnable}`), а инфо-модалка строит ссылку `app.subSettings.subURI + subID` (`inbound_info_modal.html:802-807`).
9. **Страница подписки** — `web/html/settings/panel/subscription/subpage.html`: QR-блоки `qrcode`/`qrcode-subjson`/`qrcode-subclash` (`:103-145`) по `app.subJsonUrl`/`app.subClashUrl`; данные приходят через `BuildPageData` (`sub/subService.go:1515`) и `<template id="subscription-data" data-…>` (`subpage.html:277`).
10. **Proxy front форка** — `proxy/subserver.go`: `handleSub`/`handleJson` (`:115-147`) тянут `fetchUpstream(path, subid)` (`:163`) и копируют заголовки (`copyHeaders`, `:178`); пути из `proxy/config.go:61-62` (`SubPath`, `JsonPath`). Clash не проксируется. Новый маршрут добавляется по тому же образцу: поле `TunPath` в `Config`, `handleTun` и блок на `proxy/subpage.html` (`decodeConfigs`, `:235`).

## 4. Рекомендуемые точки вставки (файл + функция)

Принцип карты #30: новые файлы и функции вместо правок существующих; в upstream-файлы — минимальные вставки, желательно внутри уже форковых hunk'ов (git при мерже видит их как один блок форка, новый конфликт не появляется).

### 4.1. Новые файлы (нулевой риск конфликтов)

| Файл | Содержимое |
|---|---|
| `database/model/tunnel_subscription.go` | таблица-связка `TunnelClientSub{ ClientUUID string \`gorm:"primaryKey"\`; SubId string \`gorm:"index;not null"\`; CreatedAt/UpdatedAt }` — один subId на клиента (PK по UUID), поиск по `sub_id` индексом. Модель `TunnelClient` не трогаем. |
| `web/service/tunnel_subscription_service.go` | `TunnelSubscriptionService`: `SetSubId(uuid, subId)`, `ClearSubId(uuid)`, `SubIdsByUUIDs([]string) map[string]string`, `ClientsBySubId(subId) []TunnelSubEntry{kind, client, conf}` — обходит `AwgService{}` и `WgService{}` через `GetClientByUUID` + `GetClientConfigByUUID` (форковый `renderClientConfig` уже применяет host override к `Endpoint`, `tunnel_service.go:829-847` форка). Трафик/лимиты для `Subscription-Userinfo` — сумма `Upload/Download/TotalGB/ExpiryTime` по найденным клиентам. |
| `web/service/setting_tunnel_sub.go` | методы на `*SettingService` (Go разрешает методы в другом файле пакета): `GetSubTunEnable() bool` (`getBool`), `GetSubTunPath() string` (`getString` **с нормализацией слэшей здесь**, чтобы не трогать `entity.CheckValid`), `GetSubTunURI()`, и `AddTunnelSubDefaults(result map[string]any, host string)` — дописывает `subTunEnable`/`subTunURI` к ответу `GetDefaultSettings`, повторяя вычисление базы (`PublicSubBase()` → cert/port → `subDomain`/host) в своём файле. |
| `sub/tunnelController.go` | `TunnelSubController` с `NewTunnelSubController(g *gin.RouterGroup, path string, enabled bool, updateInterval, title, supportUrl, announce string)` и `initRouter`: `if enabled { g.Group(path).GET(":subid", a.subTunnels) }`; хендлер отдаёт JSON `[{kind, name, conf}]`, `400 "Error!"` при пустом списке, свои `Subscription-Userinfo`/`Profile-Update-Interval`/`Profile-Title` (можно переиспользовать `(*SUBController).ApplyCommonHeaders`, он экспортирован — `subController.go:217`). Отдельный контроллер, а не методы на `SUBController`, чтобы не расширять его конструктор с 21 параметром. |
| `web/controller/tunnel_sub_controller.go` | `NewTunnelSubController(awg, wg *gin.RouterGroup)` регистрирует `POST /client/sub/:uuid` `{subId}` и `GET /client/subs` (uuid→subId) на обеих группах. Это «опциональный subId в существующих add/update API» из карты в аддитивной форме: существующие маршруты `/client/add`, `/client/update/:id` (`tunnel_controller.go:88-96`) биндят `model.TunnelClient` через `ShouldBindJSON`, лишнее поле `subId` они молча игнорируют — без правки модели/контроллера принять его там нельзя. Для mon-server и скриптов — один дополнительный вызов после add. Альтернатива с правкой `TunnelController.addClient/updateClient/updateClientByUUID` (`:211-256`, churn 2) — 3 hunk'а в стабильном файле; допустимо, но новый endpoint чище. |
| `web/html/component/aTunnelSub.html` (и блок в форковом `proxy/subpage.html`) | блок AWG/WG с QR и скачиванием `.conf` для страницы подписки панели и proxy front. |
| `sub/tunnel_test.go`, `web/service/tunnel_subscription_service_test.go` | тесты (запуск через `docker run … golang:1.26 go test ./...`, как в карте). |

### 4.2. Вставки в upstream-файлы (по одному месту на файл)

| Файл : функция/место | Вставка | Почему риск низкий |
|---|---|---|
| `database/db.go` : `initModels`, срез моделей (`:39-57`) | `&model.TunnelClientSub{},` в конец, после `&model.StubSite{}` | AutoMigrate создаёт таблицу; аддитивно, БД форка открывается upstream-бинарником (лишняя таблица ему не мешает). Файл churn 16, но срез правится дописыванием в конец — конфликт «оба добавили строку» решается тривиально. `inboundDataTables` (`:209-214`) **не трогаем** (см. §1.2). |
| `web/service/setting.go` : `defaultValueMap`, внутри форкового блока `proxyOverride*` (`origin/main:92-96`) | `"subTunEnable": "false"`, `"subTunPath": "/tun/"`, `"subTunURI": ""` | Блок уже форковый — новый upstream-hunk не создаётся. Геттеры — в новом файле (§4.1), `GetDefaultSettings` не трогаем. |
| `web/entity/entity.go` : `AllSetting`, внутри форкового блока `ProxyOverride*` (`origin/main:89-92`) | `SubTunEnable bool \`json:"subTunEnable" form:"subTunEnable"\``, `SubTunPath string \`json:"subTunPath" …\``, `SubTunURI string` | Тег `json` = ключ `defaultValueMap` (обязательно, `setting.go:180-183`). `CheckValid` (`:162-181`) не трогаем — слэши нормализуем в геттере и в JS `@blur`, как upstream в `json.html:9`. |
| `sub/sub.go` : `(*Server).initRouter`, сразу после `s.sub = NewSUBController(…)` (`:272-275`) | `tunEnable, _ := s.settingService.GetSubTunEnable(); tunPath, _ := s.settingService.GetSubTunPath(); s.tun = NewTunnelSubController(g, tunPath, tunEnable, SubUpdates, SubTitle, SubSupportUrl, SubAnnounce)` + поле `tun *TunnelSubController` в `Server` (`:44-53`) | Конец функции, стабильный с `84061ea7`; переменные `SubUpdates/SubTitle/…` уже вычислены выше (`:130-160`). `subController.go` остаётся нетронутым. |
| `web/controller/api.go` : после `a.wgController = NewWgController(wgGroup)` (`:61`) | `NewTunnelSubController(awgGroup, wgGroup)` | одна строка между двумя стабильными блоками; churn 7, но правки там — новые группы, конфликт тривиален. |
| `web/controller/setting.go` : `getDefaultSettings` (`:60-67`), после `result, err := …GetDefaultSettings(...)` | `a.settingService.AddTunnelSubDefaults(result.(map[string]any), c.Request.Host)` | churn 2; `GetDefaultSettings` (`setting.go:837-945`, nginx-правки авг.) не трогаем. Фронт получает `subTunEnable`/`subTunURI` тем же ответом, что и `subURI`. |
| `web/assets/js/model/setting.js` : блок дефолтов (`:29-58`), после `this.subClashPath` | `this.subTunEnable = false; this.subTunPath = "/tun/"; this.subTunURI = "";` | churn 5; дописывание в блок. |
| `web/html/settings/panel/subscription/general.html` : внутри форкового блока `<a-divider>proxyOverride` (`origin/main:73-97`) | `<a-divider>Tunnel subscription</a-divider>` + switch `allSetting.subTunEnable` + input `allSetting.subTunPath` с тем же `@input/@blur`, что в `json.html:8-10` | блок уже форковый; вкладку `settings.html:82` и `json.html` не трогаем. |
| `web/html/form/client.html` : `:159-160` условие `v-if` поля Subscription | убрать `&& inbound.protocol !== Protocols.AMNEZIAWG && inbound.protocol !== Protocols.NATIVEWG` (или заменить на `&& (… \|\| app.subSettings?.subTunEnable)`) | одна строка, не менялась с апреля. |
| `web/html/inbounds.html` : (1) маппинг WG `:1848` и AWG `:1925` `subId: ""` → `subId: subs[c.uuid] \|\| ""` (subs — из `GET /panel/api/{wg,awg}/client/subs`, запрошенного рядом с `/clients`, `:1836`/`:1913`); (2) `this.subSettings = {…}` `:2096-2102` — добавить `subTunEnable`, `subTunURI`; (3) в `addAwgClient/updateAwgClient/addNativewgClient/updateNativewgClient` (`:2755-2900`) после успешного add/update — `POST /client/sub/:uuid {subId}` | 4 точечных hunk'а в функциях, которые upstream менял в июне (`a66265ed`) и августе (`ccc3cd38`) | файл churn 40 — конфликты возможны, но hunk'и маленькие и внутри awg/wg-специфичных функций; экспорт ссылок (`:3445`, `:3479`) заработает сам, как только `subId` перестанет быть пустым. |
| `web/html/modals/inbound_info_modal.html` : блок Subscription URL `:385-389` и `:768-771` | для `dbInbound.isAmneziawg \|\| isNativewg` показывать `app.subSettings.subTunURI + subId` вместо `subURI + subId` (в `genSubLink`, `:802`) | 2 маленьких hunk'а; альтернатива — отдельный блок в AWG/WG-секции `:442-480`. |
| `web/translation/translate.*.toml` (13 локалей) | новые ключи в конец `[pages.settings]` и `[pages.inbounds]` | как форк уже делал для `proxyOverride*` (`b1e67026`); отсутствующий ключ падает в имя ключа (`web/locale/locale.go:92`), так что можно начать с `en_US`/`ru_RU`. |

### 4.3. Чего не делать

- Не расширять `NewSUBController`/`SUBController` (`sub/subController.go`) — 21-параметрный конструктор, каждый новый параметр = конфликт со следующей правкой upstream.
- Не добавлять `subTunURI` в карту `GetDefaultSettings` (`setting.go:839-858`) и в вычисление `subURI` (`:884-937`) — обёртка в контроллере даёт фронту то же самое.
- Не трогать `inboundDataTables`/`datagen` — своя инвалидация (или без кэша).
- Не добавлять `SubId` в `TunnelClient` (`database/model/tunnel.go`) — решение карты; кроме того, `tunnel_migrate.go:copyByFieldName` и тесты `awgwg_baseline_test.go`/`tunnel_migrate_test.go` «assert full coverage» полей между legacy и merged моделями — новое поле в `TunnelClient` может уронить upstream-тесты после мержа.
- Не трогать `web/html/awg.html` — это страница сервера (obfuscation, интерфейсы), клиентов там нет; churn 23.

## 5. Upstream-файлы, которых избегать (кроме перечисленных вставок)

| Файл | Причина |
|---|---|
| `sub/subService.go`, `sub/subJsonService.go`, `sub/subClashService.go` | churn 20/8/6; форк уже несёт патчи host override ровно в тех функциях, что upstream правил для nginx (`BuildURLs`, `getBaseSchemeAndHost`, `resolveInboundAddress`, `GetJson`, `getConfig`); каждый новый hunk увеличивает конфликтную поверхность. Туннели живут в новом `sub/tunnelController.go`. |
| `sub/subController.go` | конструктор и `initRouter`; отдельный контроллер снимает необходимость. |
| `web/service/setting.go` вне `defaultValueMap` | `GetDefaultSettings`/`UpdateAllSetting`/`GetAllSetting` правились в авг. (nginx) и сент. (Hiddify). |
| `web/service/tunnel_service.go` | churn 5 за 3 недели после создания; форк уже вставил `renderClientConfig` — новый сервис подписки вызывает публичные `GetClientByUUID`/`GetClientConfigByUUID`, а не лезет внутрь. |
| `database/model/tunnel.go`, `database/tunnel_migrate.go`, `database/*_test.go` | схема и тесты полноты полей. |
| `web/html/awg.html` | страница сервера, churn 23. |
| `web/html/settings.html`, `web/html/settings/panel/subscription/json.html` | вкладки/поля — заменяем блоком в уже форковом hunk'е `general.html`. |
| `web/html/component/aClientTable.html`, `web/html/form/inbound.html`, `web/assets/js/model/inbound.js` | иконки/протоколы уже учитывают `isAmneziawg`/`isNativewg`; для бейджа ссылки достаточно инфо-модалки. |
| `install.sh`, `update.sh`, `x-ui.sh` | churn 48/24/19, форк и так их патчит; tunnel subscription в них не нуждается. |

## 6. Открытые вопросы для спеки (здесь не решались)

- Кэш: без `datagen` — либо `sync.Map` с TTL и инвалидацией из `TunnelSubscriptionService.Set/Clear` (правки клиентов в `TunnelService.UpdateClient/DeleteClient` кэш не увидят — можно ограничиться TTL 10 с, как `inboundsCacheTTL`), либо без кэша — выборка по индексу.
- `Subscription-Userinfo` для туннелей: `expire` = минимальный ненулевой `ExpiryTime`, `total` = сумма `TotalGB` (0, если хоть у одного лимита нет) — по образцу того, как `subJsonService.GetJson` сводит клиентов одного subId (`sub/subJsonService.go:123-140`).
- Перезапуск sub-сервера после включения `subTunEnable` — только через «Restart panel» (`SIGHUP`, `main.go:80-110`), как и для JSON/Clash.
- Отключённые/исчерпавшие клиенты: `GetClientByUUID` их вернёт; фильтровать по `Enable` в сервисе подписки (`/sub` фильтрует `enable = true` на уровне inbound'а, `subService.go:163`).

## Источники

- Upstream: `git fetch upstream` (`main` = `38a02c1d`, `dev` = `7abd2aed`), `git merge-base`, `git log --since='6 months' --name-only`, `git log -S`, `gh api repos/coinman-dev/3ax-ui`, `gh api …/branches`, `gh release list`, `gh pr view 1`, `gh issue list` (issues отключены).
- Код upstream: `sub/sub.go`, `sub/subController.go`, `sub/subService.go`, `sub/subJsonService.go`, `sub/inbound_cache.go`, `web/service/setting.go`, `web/entity/entity.go`, `web/controller/{api,setting,tunnel_controller,xui}.go`, `web/service/tunnel_service.go`, `tunnel/{kind,types,config}.go`, `database/{db,tunnel_migrate}.go`, `database/model/{tunnel,mtproto}.go`, `web/html/{inbounds,settings,awg}.html`, `web/html/form/client.html`, `web/html/modals/inbound_info_modal.html`, `web/html/settings/panel/subscription/{general,json,subpage}.html`, `web/assets/js/model/setting.js`, `web/locale/locale.go`, `main.go`.
- Код форка: `git diff --stat 38a02c1d origin/main`, `proxy/{subserver,config}.go`, `tunnel/config.go` (`ReplaceEndpointHost`), `web/service/tunnel_service.go` (`renderClientConfig`).
