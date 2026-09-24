# Proxy chain: цепочка proxy front'ов с реестром в панели

Статус: черновик по карте [Proxy chain](https://github.com/SBKubric/3ax-ui-proxy/issues/68), собран тикетом [#75](https://github.com/SBKubric/3ax-ui-proxy/issues/75) из решений тикетов #69–#74. Термины — по `CONTEXT.md` (real server, proxy front, relay, relayed port, host override; chain, hop, next hop, edge front, inner front, active edge, standby edge, chain registry, chain document, wave, join token, hop secret). Решение об отмене части ADR 0001 — [ADR 0003](../adr/0003-chain-document-and-join-token.md).

## 1. Цель и границы

Режим proxy front получает **цепочку**: real server ← inner front(ы) ← edge front, с запасными edge на последнем inner. Панель ведёт **реестр цепочки** и только описывает её; каждое звено само poll'ит свой next hop (первое inner — sub-сервер панели) и применяет свою часть **документа цепочки**, усечённого наружу. Новое звено входит по одноразовому **join-токену**, после входа живёт под собственным **hop secret**. Ручная вставка relay manifest уходит: relayed ports (с `network`) панель считает сама и везёт в документе; setup page становится страницей входа.

Цели карты: живучесть и ротация edge (запасные входы, переключение host override по имени), скрытие адреса real server от edge, мониторинг через каждое edge с подсказкой о здоровом запасном.

Вне карты (см. Out of scope в #68): шифрованный хоп между звеньями, несколько активных edge, автоматический failover, IP-allowlist на внутренних звеньях, оркестратор, PROXY protocol, nginx «всё за 443», TLS/домен для подписок на боксах, WARP+, Hysteria2.

Термины карты уточнены: *chain secret* (общий секрет цепочки) из списка карты в спеке заменён на **hop secret** — персональный секрет звена (§3.7, ADR 0003); в CONTEXT.md *chain secret* помечен как избегаемый синоним. *Setup page* стала *join page*.

Правила реализации: аддитивность к upstream ([ADR 0002](../adr/0002-additive-upstream-compatibility.md)), тесты по `docs/agents/testing.md` (unit + Playwright e2e), ветки `<номер>-<название>`, Conventional Commits.

### 1.1 Сквозные решения

| # | решение | где раскрыто |
|---|---|---|
| 1 | Реестр — таблица `chain_hops` + настройки `chainRevision`, `chainExtraPorts`, `chainPollSeconds`, `chainJoinTokenHours`, `chainStaleMinutes` | §2 |
| 2 | Host override становится производным от active edge; `GetProxyOverride()` читает реестр, потребители не меняются; legacy-host импортируется как звено `legacy` | §2 |
| 3 | Relayed ports панель считает сама (xray по `skipReason`, AWG/WG, MTProto) плюс `chainExtraPorts`; пакет `relaymanifest` становится сборщиком портов документа | §3 |
| 4 | Секрет — **per-hop** (hop secret), хэши в документе; изъятое edge раскрывает только свой секрет и адрес next hop; отзыв = удаление звена | §3, §4 |
| 5 | Join пробрасывается внутрь звено → звено до панели; ответ — id, секрет и документ | §4 |
| 6 | Волна: `GET /chain/v1/document` по bearer hop secret, `If-None-Match` = ревизия, 404 при неверном секрете, poll каждые `chainPollSeconds` | §3 |
| 7 | Документ v1: `revision`, `self`, `nextHop` (с sub-путями), `activeEdge`, `hops` (только наружу), `ports` с `network` | §3 |
| 8 | `proxy.json` v2: `nextHop`, `hopSecret`, sub-порт/TLS, `stateDir`; старые ключи читаются с предупреждением и ведут в режим входа | §5 |
| 9 | Мониторинг: совместимое расширение контракта v1 — `chain` с полным списком звеньев в `/state`, `?hop=` в `/probe/configs` (`?edge=` — синоним), path `edge:<name>` и `inner:<name>` | §6 |
| 10 | Бот: `/proxy` — список звеньев, `/proxy <имя>` — переключение active edge, `/proxy off` остаётся | §2, §7 |



## 2. Реестр цепочки

### 2.1 Что панель хранит и чем это заменяет host override

Реестр цепочки — единственное место, где описана цепочка. Панель **описывает** цепочку и никогда не звонит на боксы (принцип карты #20): любое изменение реестра — это всего лишь новая ревизия, которую звенья заберут сами волной (§3).

Сущность звена:

| свойство | значение |
|---|---|
| имя | `[a-z0-9-]{1,32}`, уникально в пределах панели; им звено адресуется в боте, в UI и в `path` мониторинга |
| хост | адрес, по которому на звено ходит его **внешний** сосед (клиенты — для edge; соседнее звено — для inner) |
| роль | `inner` \| `edge` |
| next hop | ссылка на звено-сосед изнутри; `NULL` = панель (real server) |
| порядок | `position` — место среди inner'ов, `0` у ближайшего к real server |
| состояние | `pending` (заведено, join-токен выдан, бокс ещё не вошёл) → `joined` → `draining` (удалено, но ещё обслуживает бывших внешних соседей, §4.5); плюс `legacy` для мигрированного host override (2.3) |
| активность | `is_active` — ровно у одного edge или ни у одного |

Одна цепочка на панель: inner'ы образуют один путь `real server ← inner[0] ← inner[1] … ← inner[n]`, все edge подвешены к **последнему** inner'у (`inner[n]`), а если inner'ов нет — прямо к панели. Запасные edge отличаются от активного только флагом `is_active`; они уже вошли в цепочку, относят трафик туда же и ждут переключения.

Host override не исчезает как понятие — он перестаёт быть ручной настройкой и становится **производным** от реестра: адрес, который панель подставляет в конфиги и ссылки, — это хост активного edge.

### 2.2 Хранение

Новая таблица `chain_hops`, новая модель `database/model/chain.go` (ADR 0002: upstream-модели не трогаем, JSON в settings не годится — нужны уникальность имени, выборка по `next_hop_id` и индексы).

```go
// ChainHop — одно звено реестра цепочки. Времена — int64 мс UTC, как везде в панели.
// Имена индексов глобальны в SQLite, поэтому префикс idx_chain_ и регистрация в namedIndexes.
type ChainHop struct {
    Id   int    `json:"id"   gorm:"primaryKey;autoIncrement"`
    Name string `json:"name" gorm:"size:32;not null;uniqueIndex:idx_chain_hops_name"`
    Host string `json:"host" gorm:"size:255;not null"`
    Role string `json:"role" gorm:"size:8;not null;index:idx_chain_hops_role,priority:1"`  // inner | edge

    NextHopId *int `json:"nextHopId" gorm:"index:idx_chain_hops_next"` // nil = панель (real server)
    Position  int  `json:"position"  gorm:"not null;default:0"`        // порядок среди inner'ов
    SubPort   int  `json:"subPort"   gorm:"not null;default:2096"`     // порт волны и подписок звена
    SubScheme string `json:"subScheme" gorm:"size:8;not null;default:https"`

    State    string `json:"state"    gorm:"size:8;not null;index:idx_chain_hops_role,priority:2"` // pending|joined|legacy|draining
    IsActive bool   `json:"isActive" gorm:"not null;default:false"`

    // Уход звена (§4.5). Заполнены только у state="draining" и только
    // на время draining: строка вместе с ними исчезает при завершении.
    DrainRevision int64  `json:"drainRevision"` // ревизия, в которой начался уход
    DrainUntil    int64  `json:"drainUntil"`    // дедлайн, мс UTC: drain_revision_at + chainDrainMinutes
    DrainOuter    string `json:"-" gorm:"size:512"` // JSON-массив имён бывших внешних соседей: ["edge-a","edge-b"]

    JoinTokenHash    string `json:"-" gorm:"size:64"` // sha256 hex, пусто после использования
    JoinTokenExpires int64  `json:"joinTokenExpires"`
    SecretHash       string `json:"-" gorm:"size:64"` // sha256 hex от hop secret

    ObservedAddr string `json:"observedAddr" gorm:"size:64"` // адрес, с которого пришёл join (подсказка)
    JoinedAt     int64  `json:"joinedAt"`
    LastSeenAt   int64  `json:"lastSeenAt"`   // последний подтверждённый опрос (свой или через outer[])
    LastRevision int64  `json:"lastRevision"` // последняя ревизия, которую звено подтвердило
    CreatedAt    int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
    UpdatedAt    int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}
```

- Регистрация: `&model.ChainHop{},` в конец среза `initModels` (`database/db.go:40`); `idx_chain_hops_name`, `idx_chain_hops_next`, `idx_chain_hops_role` — в `namedIndexes` (`database/db.go:101`).
- FK на `next_hop_id` **не объявляем**: удаление звена обрабатывает сервис (перецепка соседей, §4.5), каскад тут был бы вреден.
- Секреты хранятся только хэшами (`sha256` hex): открытый hop secret живёт на боксе, открытый join-токен показывается владельцу один раз. Это отличие от `monToken`, который лежит открытым: там панель сама предъявляет токен в UI, здесь — нет нужды.

Настройки реестра (в форковом блоке `defaultValueMap`, `web/service/setting.go:92`, рядом с `proxyOverride*`; геттеры — новый файл `web/service/setting_chain.go`):

| ключ | тип | default | назначение |
|---|---|---|---|
| `chainRevision` | int64 | `0` | монотонная ревизия реестра; растёт на каждой записи, меняющей хоть один документ |
| `chainExtraPorts` | JSON string | `"[]"` | relayed ports, которые панель не может вычислить сама (§3.7) |
| `chainPollSeconds` | int | `30` | интервал опроса next hop |
| `chainStaleMinutes` | int | `60` | после скольких минут без свежего документа звено считает себя `stale` |
| `chainJoinTokenHours` | int | `24` | TTL join-токена |
| `chainDrainMinutes` | int | `10` | сколько удалённое звено обслуживает бывших внешних соседей, пока они не перецепятся (§4.5) |

`chainRevision` — состояние, а не предпочтение: в `entity.AllSetting` его **нет**, чтобы сохранение формы настроек не откатило ревизию (та же логика, что у `monLastContact`, `web/service/setting.go:96-99`). `chainExtraPorts`, `chainPollSeconds`, `chainStaleMinutes`, `chainJoinTokenHours`, `chainDrainMinutes` — предпочтения, они в `AllSetting` и в форме.

### 2.3 Миграция `proxyOverrideEnable` / `proxyOverrideHost`

`GetProxyOverride()` (`web/service/setting.go:388`) становится производной от реестра, **сигнатура не меняется** — потребители (`sub/subService.go:72`, `sub/subJsonService.go:90`, `sub/probe_links.go:39`, `web/service/tunnel_service.go:858`, `web/service/server.go:473`, `web/service/tgbot.go`) не правятся вообще:

```go
// GetProxyOverride: активный edge реестра, иначе legacy-настройки.
func (s *SettingService) GetProxyOverride() (string, bool) {
    if host, ok := chainActiveEdgeHost(); ok { // ChainService: is_active — в любом состоянии, см. ниже
        return host, true
    }
    return s.legacyProxyOverride() // прежнее тело: proxyOverrideEnable + proxyOverrideHost
}
```

> **Решение о месте правки.** Чтобы остаться в рамках ADR 0002 (одна вставка на upstream-файл), прежнее тело переезжает в новый форковый файл `web/service/setting_chain.go` как `legacyProxyOverride()`, а в `setting.go` остаётся один короткий hunk-обёртка. `GetProxyOverrideEnable/Host/Set*` не трогаем — они остаются доступом к legacy-ключам.

**Активное звено держит override в любом состоянии.** `ActiveEdgeHost()` возвращает хост звена с `is_active`, даже если оно `pending`: перевыпуск токена активному edge (§4.4) переводит его в `pending`, не снимая активности, и фильтр по состоянию на это окно опубликовал бы адрес real server через legacy-ключи. Сделать активным `pending`-звено по-прежнему нельзя (инвариант 1); речь только о звене, которое уже было активным. Хост введён владельцем и остаётся верным.

Разовая миграция при первом старте после апгрейда (`ChainService.MigrateLegacyOverride()`, вызов из `database/db.go` рядом с прочими миграциями, `database/db.go:350-367`):

1. Если `chain_hops` пуста **и** `proxyOverrideHost` непуст — создать звено `name="edge"`, `host=proxyOverrideHost`, `role="edge"`, `next_hop_id=NULL`, `state="legacy"`, `is_active=proxyOverrideEnable`, `secret_hash=""`, `sub_port=2096`.
2. `chainRevision = 1`.
3. Legacy-ключи **не стираются**: откат на прежний бинарник должен продолжать работать.

Состояние `legacy` значит «звено есть в реестре, но не входило по join-токену»: у него нет hop secret, оно не получает документ, волна его не касается, оно не умеет relay'ить новые порты. Оно годится ровно на одно — быть источником host override, как раньше. В UI помечается как «legacy, требует переустановки»; `POST …/reissueToken/:id` превращает его в обычное `pending`-звено.

**Решено: срок жизни `legacy`-звена не ограничивается.** Ни отсчёта дней, ни отказа заводить новые звенья, пока в реестре есть `legacy`: панель не гасит боксы и не диктует владельцу расписание переустановки, а «протухший» legacy-хост виден и так — он не получает документ и не relay'ит новые порты. Единственное напоминание — постоянный баннер в редакторе цепочки у такого звена: «переустановите бокс как звено».

Секция Settings → Subscription → *Proxy front* становится read-only: два поля показываются с отметкой «управляется реестром цепочки» и ссылкой на редактор цепочки. Единственный случай, когда они редактируемы, — пустой реестр (чистый апгрейд без цепочки).

### 2.4 API реестра

Новый контроллер `web/controller/chain_controller.go`, группа `api.Group("/chain")` — одна строка в `web/controller/api.go:72` рядом с `NewMonitoringUIController`. Авторизация — сессия панели через `checkAPIAuth` (`web/controller/api.go:33`): неавторизованному **голый `404`**, как всему `/panel/api`. Конверт — `{success, msg, obj}` (`jsonObj`/`jsonMsg`).

| метод и путь | назначение |
|---|---|
| `GET /panel/api/chain/list` | реестр целиком: `{revision, activeEdge, hops:[…]}` |
| `POST /panel/api/chain/add` | завести звено; ответ несёт `joinToken` — **единственный раз** |
| `POST /panel/api/chain/update/:id` | сменить `name`, `host`, `subPort`, `subScheme` |
| `POST /panel/api/chain/del/:id` | удалить звено: немедленно, если снаружи от него никого нет, иначе через `draining` (§4.5) |
| `POST /panel/api/chain/setActive/:id` | сделать edge активным |
| `POST /panel/api/chain/reissueToken/:id` | перевыпустить join-токен (старый мёртв) |
| `GET /panel/api/chain/hops/health` | здоровье звеньев (и edge, и inner) по данным мониторинга (§6.4) |

`GET /list` (`obj`):

```json
{"revision": 42, "activeEdge": "edge-a", "pollSeconds": 30,
 "hops": [
   {"id":1,"name":"inner-1","role":"inner","host":"10.0.0.7","subPort":2096,"position":0,
    "nextHopId":null,"state":"joined","isActive":false,
    "joinedAt":1758300000000,"lastSeenAt":1758380000000,"lastRevision":42,"observedAddr":"198.51.100.7"},
   {"id":2,"name":"inner-2","role":"inner","host":"203.0.113.9","subPort":2096,"position":1,
    "nextHopId":1,"state":"joined","lastRevision":42},
   {"id":3,"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,
    "nextHopId":2,"state":"joined","isActive":true,"lastRevision":42},
   {"id":4,"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,
    "nextHopId":2,"state":"pending","joinTokenExpires":1758386400000}]}
```

`POST /add` — тело `{"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,"position":null}`; ответ `obj`: `{"hop":{…},"joinToken":"…32 символа…","joinTokenExpires":1758386400000}`. Для `role="edge"` `next_hop_id` вычисляет панель (последний inner или `NULL`); для `role="inner"` — по `position` (см. 2.5.3). Ошибки валидации — `success:false` с текстом (конверт панели не различает коды; HTTP всегда `200`, кроме `404` неавторизованному).

**Шов для mon-server и оркестратора.** Наружу реестр открывается только на чтение и только по контракту мониторинга v1 (совместимое расширение, `docs/spec/monitoring-contract.md` §4.1): `GET /mon/v1/state` получает поле `chain`:

```json
"chain": {"revision": 42, "activeEdge": "edge-a",
          "hops": [{"name":"inner-1","role":"inner","host":"10.0.0.7","state":"joined"},
                   {"name":"edge-a","role":"edge","host":"a.example.net","state":"joined"},
                   {"name":"edge-b","role":"edge","host":"b.example.net","state":"pending"}]}
```

Плюс `GET /mon/v1/probe/configs?hop=<name>` (`?edge=` — сохранённый синоним) и ключ `path` вида `edge:<name>` / `inner:<name>`. Запись в реестр снаружи в v1 не открывается: весь ввод — через `ChainService` (единственный путь записи, он же держит инварианты и `chainRevision`). Будущий оркестратор получит `POST /mon/v1/chain/*` поверх того же сервиса — это и есть шов, отдельного слоя под него не строим.

### 2.5 Telegram

Команда `/proxy` уже зарегистрирована (`web/service/tgbot.go:294`, `case "proxy"` — `tgbot.go:733`); меняется только её тело:

- `/proxy` без аргументов — список звеньев: роль, имя, хост, состояние (`joined`/`pending`/`legacy`), свежесть (`lastRevision` против `chainRevision`), здоровье от мониторинга (UP/DOWN/UNKNOWN по `path = edge:<name>` для edge и `path = inner:<name>` для inner, §6.3), маркер активного. Ключ `tgbot.commands.chainList` + `tgbot.commands.chainHop`.
- `/proxy <имя>` — переключить активный edge: запись в реестр, `chainRevision++`, ответ `tgbot.commands.chainSwitched` (`Name==`, `Host==`) плюс **одна строка предупреждения** `tgbot.commands.chainSwitchNote`: уже выданные ссылки и конфиги продолжают ходить через прежнее edge, пока клиент не перечитает подписку (§4.7). Неизвестное имя, `pending`-звено или inner → тот же список плюс `tgbot.commands.chainUnknownName`.
- `/proxy off` сохраняется как есть: гасит host override (`is_active=false` у всех edge) — ровно то, что делал `SetProxyOverrideEnable(false)`.

Регистрация в `SetMyCommands` не меняется, меняется только `tgbot.commands.proxyDesc` (`translate.en_US.toml:1155`) — он **переписывается** под цепочку: «Show and switch the proxy chain edges (admin)». Старые ключи одиночного override — `tgbot.commands.proxyStatus`, `proxyEnabled`, `proxyDisabled`, `proxyUsage` — этим же тикетом бота **удаляются** (не остаются «на всякий случай»): команда `/proxy` целиком переезжает на цепочку, и ни один из этих текстов больше не достижим. Новые ключи добавляются **в конец** секции `tgbot.commands` во всех 13 локалях; начинаем с `en_US`/`ru_RU` (отсутствующий ключ падает в имя ключа). Защита от пустого описания (`tgbot.go:282-287`) остаётся.

### 2.6 Четыре сценария

**2.6.1 Единственный front (цепочка длины 1).** Реестр: одно звено `role=edge`, `next_hop_id=NULL`, `is_active=true`. Его next hop — панель: оно опрашивает sub-сервер панели и relay'ит на real server. Ровно нынешняя схема прокси-фронта, но манифест приезжает документом. Inner'ов нет — `hops[]` в его документе содержит только его самого.

**2.6.2 Два запасных edge.** Три edge подвешены к `inner[n]` (или к панели), у всех `next_hop_id` одинаков, `is_active` — у одного. `inner[n]` держит в своём документе `secretHash` всех трёх и впускает всех: standby-edge тоже качает подписки и relay'ит — иначе переключение было бы не мгновенным. Переключение (`setActive` или `/proxy <имя>`) — одна запись в реестре плюс `chainRevision++`; на боксах не меняется **ничего** (§3.5). Мониторинг зондирует все edge, активный и запасные (`path = edge:<name>`), и все inner (`path = inner:<name>`, §6.3).

**2.6.3 Вставка нового inner между существующими.** Было `real ← A(pos 0) ← B(pos 1) ← edges`; вставляем `M` между `A` и `B`.

Порядок действий владельца:
1. `POST /add {"name":"m","role":"inner","host":"…","position":1}` → в одной транзакции: `M.next_hop_id = A.id`, `B.next_hop_id = M.id`, `B.position = 2`, `M.state = pending`. Ревизия **не** бампается: пока `M` в `pending`, ни один документ не меняется (см. ниже); бамп случится в транзакции join (§4.3). Ответ несёт join-токен.
2. Поставить бокс `M` с этим токеном (`PROXY_JOIN_TOKEN`, §4.1). `M` входит в цепочку через `A` → `панель`, получает hop secret и свой документ, поднимает relay на `A` и sub-сервер.
3. Волна сама доводит `B` до нового next hop: на ближайшем опросе `B` увидит ревизию, в которой `nextHop = M`, перепишет outbound relay и upstream подписок и перезапустит relay.

Что меняется в `next_hop` соседей: у внутреннего соседа (`A`) — ничего; у внешнего (`B`) — с `A` на `M`. Волна доносит это до `B` за ≤ `chainPollSeconds`.

Важное ограничение порядка: между шагом 1 и завершением шага 2 `B` уже может узнать про `M` и перецепиться на мёртвый хост. Поэтому **пока `M` в состоянии `pending`, панель строит документы так, будто `M` в цепочке нет** (`B.nextHop = A`); реальная перецепка `B` попадает в документ только вместе с переходом `M` в `joined` — той же ревизией, что и join. Это делает вставку безопасной при любой задержке между заведением звена и установкой бокса.

**2.6.4 Удаление активного edge.** `POST /del/:id` активного edge **отказывает**: `{"success":false,"msg":"…active_edge_in_use…"}`. Владелец сначала делает активным другой вошедший edge (`setActive`), и только потом удаляет.

Обход — тело `{"force": true}`, разрешённое **только когда других `joined`-edge в реестре нет**: это декоммиссия цепочки. Тогда звено удаляется, `is_active` не переходит никуда, `GetProxyOverride()` возвращает `false`, и панель начинает публиковать **адрес real server**. Именно поэтому обход не по умолчанию и сопровождается явным предупреждением в UI и записью в лог («chain: override disabled, the real server address is now published»).

Почему так, а не «удалить и выключить override молча»: автоматический переход на запасной edge был бы автофейловером, а он вне scope (переключение всегда ручное); молчаливое выключение override — это утечка адреса real server, единственной вещи, которую вся конструкция и прячет.

### 2.7 Инварианты

Их держит `ChainService` в транзакции на каждой записи; нарушение — отказ с сообщением, а не молчаливая починка.

1. Активных edge не больше одного, и он в состоянии `joined` или `legacy` (`pending`- и `draining`-звено активным сделать нельзя).
2. `next_hop_id` любого **живого** edge = последний живой inner в состоянии `joined`/`legacy`, либо `NULL`, если таких inner'ов нет.
3. Живые inner'ы образуют **один путь**: ровно один inner с `next_hop_id = NULL`, у каждого следующего `next_hop_id` — предыдущий, `position` подряд от 0 без дыр, ветвлений нет.
4. Имена уникальны (уникальный индекс `idx_chain_hops_name`) и отвечают формату `[a-z0-9-]{1,32}`.
5. Звено не может быть собственным предком: обход по `next_hop_id` от любого звена завершается на `NULL` не более чем за `len(hops)` шагов.
6. `chainRevision` монотонна и растёт на каждой записи, меняющей хотя бы один документ (§3.4).
7. **Живым** звено называется, если его состояние не `draining`. Draining-звено не входит в живой путь (инварианты 2 и 3 его не видят), ни для кого не является `nextHop` и не считается «последним inner'ом» для edge'ей; свои `next_hop_id`, `host`, `position` и `secret_hash` оно сохраняет неизменными до завершения (§4.5).
8. `draining` — терминальное состояние: из него нет переходов, кроме удаления строки. `update`, `setActive`, `reissueToken` и повторный join по draining-звену отказывают (`hop_is_draining`); `del` по нему идемпотентен и ревизию не бампает.
9. У звена в состоянии `draining` заполнены `drain_revision`, `drain_until` и `drain_outer`; ни у одного другого состояния они не заполнены.

### Тронутые файлы

Новые:
- `database/model/chain.go` — модель `ChainHop` (2.2).
- `web/service/chain_service.go` (+ `_test.go`) — реестр, инварианты, ревизия, миграция legacy override.
- `web/service/setting_chain.go` — геттеры настроек `chain*`, `legacyProxyOverride()`, `chainActiveEdgeHost()`.
- `web/controller/chain_controller.go` (+ `_test.go`) — `/panel/api/chain/...` (2.4).
- `web/html/settings/panel/subscription/chain.html`, `web/assets/js/model/chain.js` — редактор цепочки.

Вставки (по одному месту на файл):

| файл : место | вставка |
|---|---|
| `database/db.go:40` (`initModels`) | `&model.ChainHop{},` в конец среза |
| `database/db.go:101` (`namedIndexes`) | три индекса `idx_chain_hops_*` |
| `database/db.go:350-367` (блок миграций) | вызов `service.MigrateLegacyOverride()` |

Три вставки в `database/db.go` и две в `web/service/setting.go` — сознательное исключение из «одна вставка на файл» ADR 0002: регистрация модели, индексов и миграции — стандартная триада, которую форк уже делает для мониторинга в тех же местах, а `defaultValueMap` + `GetProxyOverride` лежат внутри форкового блока `proxyOverride*`. Хуки `ChainService.BumpRevision()` в `inbound.go`, `tunnel_service.go`, `mtproto_service.go` (§3) — принимаемая цена аддитивности: любое изменение состава портов должно поднять ревизию, а общего события «порты изменились» в панели нет.
| `web/service/setting.go:92` (`defaultValueMap`, блок `proxyOverride*`) | пять ключей 2.2 |
| `web/service/setting.go:388` (`GetProxyOverride`) | обёртка над реестром, тело → `setting_chain.go` |
| `web/entity/entity.go:91` (`AllSetting`, форковый блок) | четыре поля (без `chainRevision`) |
| `web/controller/api.go:72` | `NewChainController(api.Group("/chain"))` |
| `web/service/tgbot.go:733` (`case "proxy"`) | новое тело 2.5 |
| `web/service/monitoring_service.go` (`/state`) | поле `chain` |
| `web/translation/translate.*.toml` | ключи в конец секций |
| `web/html/settings/panel/subscription/general.html` | блок `proxyOverride` → read-only + ссылка |

### Открытые вопросы владельцу

Открытых вопросов нет.

---

## 3. Документ цепочки и волна

### 3.1 Схема

```
{"version":1,"revision":42,"generatedAt":1758380000000,
 "self":{"name":"inner-2","role":"inner","host":"203.0.113.9","state":"joined"},
 "nextHop":{"host":"10.0.0.7","subPort":2096,"subScheme":"https",
            "subPath":"/sub/","jsonPath":"/json/","tunPath":"/tun/"},
 "activeEdge":"edge-a",
 "hops":[{"name":…,"role":…,"host":…,"subPort":…,"secretHash":…,"state":…}],
 "ports":[{"port":443,"network":"tcp,udp","tag":"inbound-443","source":"xray"}]}
```

- `self` — как звено называется в реестре. Имя нужно ему для `/chain/v1/status` и логов, `host` — чтобы заметить расхождение с тем, что владелец ввёл (§4.4), `state` — чтобы знать, что оно **уходит**: `joined` | `legacy` | `pending` | `draining`. Отсутствие `self.state` читается как `joined` (бокс старее панели). Единственное следствие `draining` на боксе — правило §4.5.3: отдавая документ внешнему соседу, такое звено подставляет в его `nextHop` свой собственный `nextHop`, а не себя.
- `nextHop` — единственное, что звено знает про «глубже»: хост, порт и пути подписок. Пути едут в документе, поэтому `proxy.json` больше не хранит `subPath`/`jsonPath`/`tunPath` (§4 рамки).
- `hops` — **сам звено и всё, что снаружи от него**; `secretHash` = `sha256(hop secret)` hex; `state` — `joined` | `legacy` | `pending` | `draining`. `pending`-звенья в списке не появляются, кроме перевходящих (§4.5.7); `draining`-звено появляется ровно в одном списке — в `hops[]` своего next hop'а, и это единственное место, где его `secretHash` ещё жив (§4.5.2). Звено использует из этого списка только хэши своих **прямых** внешних соседей (тех, кто к нему подключается); остальные записи — материал для UI и для сверки `outer[]`.
- `ports` — полный список relayed ports (§3.7). Он одинаков для всех звеньев цепочки: порты real server пробрасываются один-в-один до самого края.

### 3.2 Полный пример: `real ← inner-1 ← inner-2 ← {edge-a (active), edge-b}`

**Правило усечения.** Документ для звена `H` = `self(H)` + `nextHop(H)` + `ports` + `hops`, где `hops` содержит `H` и всё, что от `H` наружу (для inner — внешние inner'ы и все edge на последнем inner'е; для edge — только его самого, потому что снаружи edge только клиенты). Соседний edge — не «снаружи», а «сбоку»: `edge-b` в документе `edge-a` **не появляется**. Ни в одном документе нет ничего изнутри от `H`, кроме одного поля `nextHop.host`.

Правило не знает исключений и для уходящих звеньев: `draining`-звено остаётся на своём месте в упорядоченном списке, поэтому в суффикс `hops[H:]` любого звена снаружи от него оно не попадает, а в документе его next hop'а оно стоит там же, где стояло. Именно поэтому завершение ухода не задевает документы снаружи (§4.5.6).

Документ **inner-1** (ближайший к real server; его `nextHop` — панель):

```json
{"version":1,"revision":42,"generatedAt":1758380000000,
 "self":{"name":"inner-1","role":"inner","host":"10.0.0.7","state":"joined"},
 "nextHop":{"host":"198.51.100.1","subPort":2096,"subScheme":"https",
            "subPath":"/sub/","jsonPath":"/json/","tunPath":"/tun/"},
 "activeEdge":"edge-a",
 "hops":[
   {"name":"inner-1","role":"inner","host":"10.0.0.7","subPort":2096,"secretHash":"3b1f…","state":"joined"},
   {"name":"inner-2","role":"inner","host":"203.0.113.9","subPort":2096,"secretHash":"9c4a…","state":"joined"},
   {"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,"secretHash":"77de…","state":"joined"},
   {"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,"secretHash":"01bc…","state":"joined"}],
 "ports":[
   {"port":443,"network":"tcp,udp","tag":"inbound-443","source":"xray"},
   {"port":8443,"network":"tcp,udp","tag":"inbound-trojan","source":"xray"},
   {"port":51820,"network":"udp","tag":"awg","source":"awg"},
   {"port":51821,"network":"udp","tag":"wg","source":"wg"},
   {"port":9443,"network":"tcp","tag":"mtproto-17","source":"mtproto"},
   {"port":8080,"network":"tcp","tag":"extra-8080","source":"extra"}]}
```

Документ **inner-2**: то же `ports`, `activeEdge`, но `nextHop.host = "10.0.0.7"` (inner-1), `self.name = "inner-2"`, а `hops` — без `inner-1`:

```json
 "hops":[
   {"name":"inner-2","role":"inner","host":"203.0.113.9","subPort":2096,"secretHash":"9c4a…","state":"joined"},
   {"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,"secretHash":"77de…","state":"joined"},
   {"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,"secretHash":"01bc…","state":"joined"}]
```

Документ **edge-a** — всё, что знает изъятый край:

```json
{"version":1,"revision":42,"generatedAt":1758380000000,
 "self":{"name":"edge-a","role":"edge","host":"a.example.net","state":"joined"},
 "nextHop":{"host":"203.0.113.9","subPort":2096,"subScheme":"https",
            "subPath":"/sub/","jsonPath":"/json/","tunPath":"/tun/"},
 "activeEdge":"edge-a",
 "hops":[{"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,"secretHash":"77de…","state":"joined"}],
 "ports":[ … тот же список … ]}
```

> **Решение:** поле `activeEdge` попадает в документ edge-звена **только если активно оно само** (иначе отсутствует): иначе изъятый standby-edge узнавал бы имя активного соседа, а это ровно та подсказка, которой мы его лишаем усечением `hops`. Для inner'ов поле есть всегда — им оно нужно для `/chain/v1/status` и диагностики.

### 3.3 Транспорт

Три ручки, одинаковые на **sub-порту каждого звена** (`proxy/chain.go`, новый файл) и на **sub-сервере панели** (`sub/chainController.go`, новый файл; регистрация — один блок в `sub/sub.go:initRouter` после `NewSUBController`, по образцу `tunnelController`). Префикс `/chain/v1/` фиксирован, не настраивается: звенья должны находить его без документа.

| метод и путь | кто зовёт | авторизация |
|---|---|---|
| `GET /chain/v1/document` | звено → свой next hop | `Authorization: Bearer <hop secret>` звонящего |
| `POST /chain/v1/join` | входящий бокс → next hop, далее внутрь | нет (токен в теле), §4 |
| `GET /chain/v1/status` | владелец локально / внешний сосед | bearer собственного секрета звена или секрета прямого внешнего соседа |

`GET /chain/v1/document`:

- `200` + документ (усечённый под звонящего), `ETag: "42"`, `Content-Type: application/json; charset=utf-8`.
- `If-None-Match: "42"` и ревизия не изменилась → `304` без тела.
- Неизвестный/неверный секрет, отсутствующий заголовок, звонящий не числится прямым внешним соседом — **голый `404`** без тела, как `checkAPIAuth` (`web/controller/api.go:33`) и как §2 контракта мониторинга. Сравнение — constant-time по хэшу.
- **Кого впускает панель.** `AuthenticateHop` сверяет предъявленный секрет с `secret_hash` звеньев первого яруса (`next_hop_id IS NULL`) в состояниях `joined`, `legacy` **и `draining`**: уходящее звено первого яруса обязано получить хотя бы один документ после удаления — тот, в котором его `self.state` стал `draining`, — иначе оно продолжит называть соседям себя (§4.5.3).
- **Кого впускает звено.** Ничего нового: `outerNeighbour` сверяет секрет с `secretHash` из собственного документа. Уходящее звено впускает своих бывших внешних соседей, потому что их записи остались в его `hops[]`; его собственный next hop впускает его, потому что его запись (`state:"draining"`) остаётся в документе этого next hop'а до завершения ухода.
- Интервал опроса — `chainPollSeconds` (default 30), с джиттером ±20 %, чтобы цепочка не опрашивалась синхронной волной. **Решено: «пинка» нет** — временного ускорения до 5 с после смены ревизии не вводим. Интервал ровно один, всегда 30 с с джиттером: прокатывание до края за ≤ 30 с × длину цепочки (три звена ≈ 1,5 мин) приемлемо, а ускорение стоило бы второго режима опроса, заражающего соседей, и отдельного класса багов «звено застряло в быстром режиме». Единственное исключение уже есть и остаётся локальным: только что вошедшее звено ретраит первый опрос раз в 5 с первые 3 минуты (§4.3).
- **Переключение активного edge волны не касается вообще**: оно не требует, чтобы новая ревизия доехала до звеньев (на боксах от него не меняется ничего, §3.5/§4.7), и срабатывает в момент записи в реестр. Задержка волны — это только про состав портов и перецепку next hop.
- **Решено: sub-порт звена обслуживает и подписки, и волну** — одного порта достаточно, разносить `/chain/v1/*` на отдельный порт не будем. Звенья цепочки и так открыты снаружи без allowlist (решение карты), ручки волны защищены bearer'ом hop secret и отвечают голым `404` без него, а второй порт добавил бы ещё одно поле в реестр, в документ и в установщик ради файрвол-правила, которое нечего защищать.
- Звено шлёт `X-Chain-Seen: <revision>` — ревизию, которую оно уже применило, и заголовок `X-Chain-Outer: <base64 json>` с подтверждениями своих внешних соседей: `[{"name":"edge-a","lastRevision":42,"lastSeen":1758379990000}, …]`. Принявшее звено кладёт своё подтверждение и всё, что пришло снаружи, в тело **своего** следующего опроса внутрь. Так реестр получает свежесть по каждому звену, ни разу не позвонив наружу: панель напрямую слышит только первый inner, всё остальное приезжает с его подтверждениями.

> **Решение:** подтверждения `outer[]` не едут в теле опроса: `GET` с телом ненадёжен на прокси, поэтому подтверждения едут заголовком `X-Chain-Outer` (base64 JSON, ≤ 8 КиБ — при переполнении звено режет список и логирует). Семантика та же.

Панель на каждом авторизованном `GET /chain/v1/document` обновляет `last_seen_at` и `last_revision` звена-звонящего; на `X-Chain-Outer` — те же поля у перечисленных звеньев, но `last_seen_at` не может быть новее полученного значения.

### 3.4 Ревизия

`chainRevision` — монотонный `int64` в настройках панели, инкремент в той же транзакции, что и запись в реестр, **если запись меняет хоть один документ**: заведение/удаление/переименование звена, смена хоста, `subPort`, `subScheme`, порядка, смена активного edge, успешный join, ротация hop secret, изменение `chainExtraPorts`, а также изменение состава relayed ports (добавление/удаление/включение inbound'а, смена порта AWG/WG/MTProto — хук в конце уже существующих обработчиков, см. «Тронутые файлы»).

Не инкрементят: обновление `last_seen_at`/`last_revision`, заведение звена в `pending` и выдача или перевыпуск его join-токена (документы строятся без `pending`-звеньев, §2.6.3), правка `chainPollSeconds`, повторный `del` по уже уходящему звену.

Отдельный случай — уход звена (§4.5): `del` инкрементит один раз (начало ухода), а завершение — только если next hop уходящего был звеном, а не панелью; тогда из его `hops[]` пропадает одна запись, и без новой ревизии он продолжал бы впускать умерший секрет. Разбор — §4.5.6.

ETag — строка `"<revision>"`. Ревизия глобальна на цепочку: звено, до которого изменение не дошло по смыслу, всё равно увидит новое число и просто не найдёт диффа (§3.5) — это дешевле, чем ревизия на документ.

### 3.5 Применение на звене

Звено сравнивает принятый документ с предыдущим (`<stateDir>/document.json`) и делает ровно то, что изменилось:

| изменилось | что делает звено |
|---|---|
| `ports` | пересобирает конфиг relay-xray и **перезапускает** relay |
| `nextHop.host` / `subPort` / `subScheme` | переписывает `address` во всех dokodemo-door outbound'ах и upstream подписок, **перезапускает** relay; sub-сервер подхватывает новый upstream без рестарта |
| `nextHop.subPath` / `jsonPath` / `tunPath` | только sub-сервер, без рестарта relay |
| `hops[].secretHash` (состав или значения) | перезагружает таблицу авторизации в памяти, **без рестарта** |
| `activeEdge` | **ничего** |
| `self.name`, `self.host` | лог; `host` расходится с наблюдаемым — предупреждение в `/chain/v1/status` |
| `self.state` → `draining` | на собственный relay **ничего**; меняется только то, что звено отдаёт наружу: в `nextHop` каждого внешнего соседа подставляется собственный `nextHop` документа (§4.5.3). В лог — `chain: this hop is draining, handing my next hop to my neighbours`; в `/chain/v1/status` — `draining: true` |
| ничего (только `revision`) | записывает новый `document.json`, отвечает новым `X-Chain-Seen` |

Перезапуск relay рвёт принятые соединения: dokodemo-door не умеет мягкой перезагрузки, xray-процесс поднимается заново. Это **принимаемое неудобство**: клиенты переподключаются за секунды, а событие редкое (смена состава портов или перецепка). Переключение активного edge — самая частая операция — специально не требует на звеньях ничего вообще: активный edge меняет только то, какой адрес панель подставляет в свои конфиги и ссылки. Подтверждаем: inner'ы и edge про это переключение узнают, но не реагируют.

Порядок: сначала атомарно записать документ, потом применять. Если применение упало — звено оставляет новый `document.json` (иначе зациклится), логирует и повторит применение на следующем опросе.

### 3.6 Состояние на диске и недоступный next hop

- `<stateDir>/document.json` (`stateDir` default `/etc/x-ui/chain`). Запись атомарная: `document.json.tmp` → `fsync` → `rename`. Файл — `0600`.
- На старте звено поднимается из `document.json` **до** первого успешного опроса: перезагрузка бокса не ждёт волну.
- Next hop недоступен (сеть, `404`, `5xx`, таймаут): звено **держит последний документ и продолжает relay'ить**. Ретраи — обычным интервалом опроса с экспоненциальным замедлением до 5 минут. Самовыключения нет ни при каких условиях: живой relay со слегка устаревшей конфигурацией всегда лучше мёртвого.
- После `chainStaleMinutes` (default 60) без успешного `200`/`304` звено пишет в лог `chain: document is stale (last ok <ts>, next hop <host>)` (раз в час, не спамом) и выставляет `stale: true`.
- **Решено: верхнего предела «протухания» нет.** Звено relay'ит последнюю известную ревизию **бесконечно** и само в bootstrap не уходит — ни через сутки, ни через неделю. Уход в bootstrap означал бы, что звено само себя выключает как relay и поднимает join page именно в тот момент, когда цепочка и так сломана: трафик, который ещё шёл, встал бы, а починку всё равно делает человек (§4.6). `stale: true` в `/chain/v1/status`, строка в логе и DOWN в мониторинге — достаточный сигнал.

`GET /chain/v1/status` (`200`):

```json
{"version":1,"name":"inner-2","role":"inner","revision":42,
 "lastPoll":1758379990000,"lastOk":1758379990000,"stale":false,"draining":false,
 "relay":{"running":true,"ports":[443,8443,51820,51821,9443,8080],"restartedAt":1758300000000},
 "nextHop":{"host":"10.0.0.7","subPort":2096,"reachable":true},
 "observedHostMismatch":false}
```

Секретов и списка `hops` в статусе нет. Неверный bearer — `404`. `draining: true` значит, что панель удалила это звено и оно доживает как транзит для бывших соседей (§4.5): это сигнал «бокс можно гасить, как только соседи перецепятся», а не поломка.

### 3.7 Доверие

Что узнаёт **изъятый edge** (бокс в руках противника): свой hop secret; хост и sub-порт своего next hop; полный список relayed ports; своё имя; факт, что он сам активен (или отсутствие этого факта). Чего не узнаёт: адрес real server, имена и адреса остальных звеньев, включая соседние edge, число звеньев в цепочке, ключи и клиентов панели.

Что может подделать: ничего внутрь. Его секрет авторизует **единственное** действие — чтение собственного усечённого документа у его next hop. Он не может ни прочитать чужой документ (сравнение идёт по конкретному `secretHash` из `hops`), ни записать что-либо в реестр (ручек записи нет вообще, кроме `join`, которая требует действительного join-токена, выданного владельцем в панели), ни выдать себя за другое звено (документы усечены персонально). Он может ретранслировать чужой трафик и читать подписки, которые через него и так идут, — это свойство L4-relay, а не протокола цепочки. Отзыв — удаление звена из реестра: следующая ревизия убирает его `secretHash` из документа next hop, и опрос начинает получать `404`; relay при этом надо гасить у провайдера (бокс сам себя не выключает).

Для изъятого **edge** отзыв мгновенен: снаружи от edge только клиенты, поэтому `del` удаляет строку сразу (§4.5.1, п. 3) и секрет умирает в той же транзакции. Отсрочка бывает только у inner'а с внешними соседями — он уходит через `draining` и до завершения сохраняет ровно два права: читать **свой собственный** усечённый документ и отдавать соседям их. Ничего сверх прежнего он не получает: усечение работает как работало, внутрь он видит один `nextHop.host`, записать в реестр не может ничего. Если владельцу нужен мгновенный отзыв и у inner'а (бокс в чужих руках) — `del` с `{"skipDrain": true}`, ценой ручной починки внешнего соседа по §4.6.

Почему персональный секрет лучше общего: при общем секрете изъятый edge получал бы право читать документ у **любого** звена, а значит — идти вглубь цепочки, опрашивая каждый следующий next hop; усечение перестало бы работать. Персональный секрет делает компрометацию строго локальной, а отзыв — точечным, без перевыпуска чего-либо у остальных.

Почему подпись документа в v1 не нужна: единственный, кто отдаёт звену документ, — его next hop, и канал к нему уже аутентифицирован (bearer через TLS sub-порта). Подпись защищала бы от **скомпрометированного next hop'а**, но такой next hop и так владеет трафиком звена целиком — подделка документа ничего к его возможностям не добавляет. Будущая опция (не v1): панель подписывает канонический JSON документа Ed25519-ключом, публичный ключ едет в `proxy.json` при join; это даст обнаружение вредящего посредника, но потребует ротации ключа и обработки расхождений — отдельный тикет.

### 3.8 Состав relayed ports

Порты считает панель; пакет `relaymanifest` переименовывается в `chainports` и становится сборщиком этого списка (§5.9). Список:

1. **xray inbounds** — из таблицы `inbounds`: строки с `enable = true` и протоколом, который обслуживает xray (все, кроме `amneziawg`, `nativewg` и `mtproto` — ровно те три, что пропускает `XrayService.GetXrayConfig`). Правила пропуска те же пять, что были у `proxy/relay.go skipReason` (теперь `chainports.SkipReason`):
   - `port <= 0` — «no port»;
   - `tag == "api"` — внутренний gRPC-inbound;
   - `listen` ∈ `127.0.0.1`, `::1`, `localhost` — loopback;
   - `listen` начинается с `@` — unix-socket fallback;
   - transparent-proxy inbound (`proxy/relay.go:31`): `dokodemo-door` с `streamSettings.sockopt.tproxy` ∈ `tproxy`/`redirect`, либо `settings.followRedirect`, либо тег с суффиксом `-tproxy-in`.

   Остальные — `network: "tcp,udp"`, `source: "xray"`, `tag` = тег inbound'а. При включённом nginx-фронте публичным является `PublicPort` (`web/service/inbound.go:150`), он и попадает в список вместо приватного порта inbound'а.
2. **AWG / WireGuard** — `tunnel_servers.listen_port` каждого включённого сервера (`database/model/tunnel.go:23`), `network: "udp"`, `source: "awg"` / `"wg"`, `tag` = `awg` / `wg`.
3. **MTProto** — порт inbound'а с протоколом mtproto. Такие inbound'ы намеренно не попадают в конфиг xray (`web/service/xray.go:162`, sidecar `mtg`), поэтому берутся напрямую из таблицы `inbounds`: `network: "tcp"`, `source: "mtproto"`, `tag` = `mtproto-<inboundId>`.
4. **`chainExtraPorts`** — ручной довесок для всего, чего панель не знает (сторонний демон на хосте). Формат — JSON-массив в настройке:

   ```json
   [{"port": 8080, "network": "tcp", "note": "stub site"},
    {"port": 5353, "network": "udp", "note": "dns"}]
   ```

   `network` ∈ `tcp` | `udp` | `tcp,udp`. `source: "extra"`, `tag` = `extra-<port>`.

> **Решение о формате:** у `proxy.json` формат extra ports — строка `"51820/udp"` (`proxy/config.go:75 ParseExtraPort`). В настройке панели берём типизированный JSON: он редактируется формой, переживает валидацию и несёт `note`. Парсер строкового формата остаётся в `proxy/config.go` для чтения legacy-конфигов (§4 рамки).

Дубликат порта из разных источников — ошибка сборки документа: панель не бампает ревизию, пишет в лог и показывает баннер в редакторе цепочки («порт 443 объявлен дважды: xray inbound-443 и extra»). Правило «один порт — один источник» перенесено из `proxy/relay.go:122-124`.

**Источник — таблица, а не `bin/config.json`.** Сгенерированный конфиг панель переписывает только при рестарте xray, то есть уже после того, как хук `chainPortsChanged` сдвинул ревизию (§3.4); звено, опросившее next hop в этом промежутке, положило бы в кэш старый список под новой ревизией, и больше ничто её не сдвинуло бы — добавленный inbound не доехал бы до звеньев никогда (стенд, #96). Таблица верна уже в момент бампа: хук работает в той же транзакции, что и вызвавшая его запись. При nginx-фронте inbound учитывается по `PublicPort` и публичному адресу, несколько inbound'ов за одним 443 дают один relayed port.

`x-ui chain ports` на панели печатает ровно этот список тем же кодом (не отдельный debug-экспорт); старое имя `x-ui relay-manifest` удаляется без алиаса (§5.8). Вручную этот список больше никуда не вставляют.

### Тронутые файлы

Новые:
- `proxy/chain.go` (+ `chain_test.go`) — клиент волны на боксе, кэш документа, применение диффа, `/chain/v1/*` на sub-порту.
- `proxy/chain_state.go` — атомарная запись/чтение `<stateDir>/document.json`.
- `sub/chainController.go` (+ `chain_test.go`) — `/chain/v1/document`, `/chain/v1/join` на sub-сервере панели.
- `web/service/chain_document.go` (+ `_test.go`) — сборка и усечение документа, ETag, `X-Chain-Outer`.
- `chainports/chainports.go` (+ `_test.go`) — бывший `relaymanifest/`: `Build()` для xray + awg/wg + mtproto + extra (§5.9).

Вставки:

| файл : место | вставка |
|---|---|
| `sub/sub.go` : `initRouter` после `NewSUBController`; поле `chain` в `Server` | регистрация `NewChainController` |
| `proxy/proxy.go:18` (`Run`) | старт цикла волны вместо проверки наличия манифеста |
| `proxy/relay.go:90` (`BuildRelayConfig`) | вход из `[]DocumentPort` вместо `manifestPath`+`extra` |
| `proxy/config.go` | v2-поля (`nextHop`, `hopSecret`, `stateDir`), чтение legacy-ключей с предупреждением |
| `web/service/inbound.go`, `web/service/tunnel_service.go`, `web/service/mtproto_service.go` | по одному хуку `ChainService.BumpRevision()` после изменения состава портов |
| `main.go:576` | `chain ports` вместо удалённой `relay-manifest`; новые подкоманды `chain status`/`chain rejoin`/`chain join-url` (§5.8) |

### Открытые вопросы владельцу

Открытых вопросов нет.

---

## 4. Вход и выход звена

### 4.1 Join-токен

| свойство | значение |
|---|---|
| формат | 32 символа `random.Seq(32)` (`util/random/random.go:41`) — та же длина и алфавит, что у `monToken` (`web/service/setting_monitoring.go:22`) и прочих секретов панели |
| срок | `chainJoinTokenHours`, **решено: 24 ч**; `join_token_expires` — абсолютное время в мс |
| одноразовость | в реестре лежит только `sha256` hex; при успешном join `join_token_hash` и `join_token_expires` очищаются, повтор получает `404` |
| показ | ровно один раз — в ответе `POST /panel/api/chain/add` (и `…/reissueToken/:id`); панель его больше не покажет |
| перевыпуск | `POST /panel/api/chain/reissueToken/:id` — новый токен, новый хэш; старый мёртв сразу (перезапись хэша). Звено остаётся тем же, ревизия **не** бампается (документ не меняется) |

**Решено окончательно: 24 часа, одноразовый, один токен на одно звено.** Ни более длинного TTL «на всякий случай», ни многоразовых токенов для массового разворачивания standby edge: токен — это пропуск в цепочку, и чем дольше он живёт и чем больше боксов им входит, тем меньше от него пользы как от улики «вошёл именно тот, кого я завёл». Заводить пять standby edge — значит завести пять звеньев и получить пять токенов; просроченный неиспользованный токен перевыпускается одной кнопкой (`reissueToken`).

Как токен попадает на бокс — два пути, оба поддерживаются:

1. **При установке:** `PROXY_JOIN_TOKEN=<32 симв.>` рядом с `PROXY_NEXT_HOP` и `PROXY_NEXT_HOP_SUB_PORT` в `install.sh` (по образцу нынешних `PROXY_*`, `install.sh:2656-2676`). Установщик пишет `proxy.json` v2 без `hopSecret` и стартует сервис — бокс входит в цепочку сам, без единого клика.
2. **Через join-страницу:** setup page (`proxy/setup.go`) перестаёт принимать relay-манифест и становится **join-страницей** по одноразовому секретному пути `/join/<token>` на sub-порту (файл ссылки `chain-join.url`, §5.4). Поля: next hop (хост, порт, схема — предзаполнены из `proxy.json`, если заданы) и join-токен. Кнопка «Join» выполняет §4.3; при успехе страница исчезает, как и раньше (`SetupServer.spent()`, `proxy/setup.go:97`). Путь страницы печатается в лог и в `x-ui chain join-url` — как сейчас печатался в `proxy-setup-url` (старое имя удалено, §5.8).

Что бокс знает **до** входа: адрес и sub-порт своего next hop и join-токен. Ничего больше — ни своего имени, ни портов, ни того, есть ли за next hop'ом ещё звенья.
Что он знает **после**: свой `hopId`, своё имя, свой hop secret, свой усечённый документ (§3.2). Всё это он записывает в `proxy.json` v2 и `document.json`.

### 4.2 Hop secret

Генерируется **панелью** в момент join: `random.Seq(32)`, в реестр кладётся `sha256` hex, открытое значение уезжает боксу в ответе и больше нигде не хранится. Бокс держит его в `proxy.json` (`0600`). Ротация: `POST /panel/api/chain/reissueToken/:id` у `joined`-звена переводит его в `pending` и требует повторного входа (§4.4) — отдельной «смены секрета без переустановки» в v1 нет.

### 4.3 Вход, пробрасываемый внутрь

`POST /chain/v1/join` — **без авторизации** (токен в теле), одинаково на sub-порту каждого звена и на sub-сервере панели:

```json
{"token":"<32 символа>","host":"a.example.net","subPort":2096,"subScheme":"https"}
```

> **Отступление от рамки карты.** «Решено при чартинге» говорит: «следующее звено сверяет токен по документу и отдаёт конфиг»; тикет #71 оставлял выбор между хэшем токена в документе и запросом глубже. Выбран второй вариант, и вот почему: сосед может сверить хэш, но не может ни выдать hop secret, ни перевести звено в `joined`, ни бампнуть ревизию — всё это только у панели. Локальная сверка потребовала бы того же обращения внутрь плюс второй копии хэшей токенов в документах; сосед в этой схеме — только транспорт, а панель — единственный судья. Цена — join не работает, пока путь до панели разорван, что и так было бы правдой.

Звено, получившее запрос, не разбирает его: проверяет только размер тела (≤ 4 КиБ) и наличие `token`, добавляет `X-Chain-Observed: <RemoteAddr>` (только если заголовка ещё нет — значит, оно прямой получатель) и `X-Chain-Forwarded: <n+1>`, и шлёт тот же JSON на **свой** next hop. При `n+1 > 16` — `400` `join_loop`. Ответ next hop'а возвращается звонящему байт в байт.

```
edge-b (bootstrap)      inner-2              inner-1           панель (sub-сервер)
   │ POST /chain/v1/join {token,host,subPort}
   ├────────────────────►│
   │                     │ +X-Chain-Observed: 198.51.100.44
   │                     │ +X-Chain-Forwarded: 1
   │                     ├────────────────────►│
   │                     │                     │ +X-Chain-Forwarded: 2
   │                     │                     ├──────────────────►│
   │                     │                     │                   │ sha256(token) → hop #4
   │                     │                     │                   │ не истёк, state=pending
   │                     │                     │                   │ secret=Seq(32)
   │                     │                     │                   │ secret_hash, state=joined,
   │                     │                     │                   │ joined_at, observed_addr,
   │                     │                     │                   │ host (если прислан)
   │                     │                     │                   │ chainRevision++  (одна транзакция)
   │                     │                     │◄──────────────────┤ 200 {hopId,name,secret,document}
   │                     │◄────────────────────┤ 200 (без изменений)
   │◄────────────────────┤ 200
   │ proxy.json v2 + document.json, старт relay и sub-сервера,
   │ первый GET /chain/v1/document → 200 или 404 (ретрай)
```

Ответ `200`:

```json
{"hopId":4,"name":"edge-b","secret":"<32 символа>","pollSeconds":30,"document":{ … §3.2 … }}
```

Отказы — **голый `404`** без тела (неизвестный хэш, истёкший токен, звено уже `joined`, токен уже израсходован): вошедший не должен различать «токена нет» и «токен не тот», а посторонний сканер — узнавать о существовании ручки. Исключения: `400` `join_loop` и `413` на слишком большое тело.

**Вход, пока кто-то уходит.** Draining-звено в пути не стоит (§2.7, инвариант 7), поэтому новое звено встаёт в живой путь, а не за уходящим: `reconcileTopology` пропускает draining так же, как пропускает `pending`. Join с токеном самого draining-звена невозможен — токен израсходован, а `reissueToken` по нему отказывает (§4.5.5).

**Первый inner входит в панель напрямую** — тем же `POST /chain/v1/join` на sub-сервере панели (`sub/chainController.go`), с теми же телом и ответом. Никакого особого случая в протоколе нет: разница только в том, что цепочка проброса пуста.

После join бокс начинает опрашивать свой next hop как обычное звено. Пока документ соседа не обновился (≤ `chainPollSeconds`), его `secretHash` там ещё не значится и опрос получает `404` — бокс молча повторяет с интервалом 5 с в течение первых 3 минут. Это ожидаемое состояние, а не ошибка; в логе — `chain: waiting for the next hop to pick up the new revision`.

### 4.4 Сверка хоста, повторный вход, смена хоста

**Сверка хоста.** Панель записывает `observed_addr` — адрес, с которого join пришёл на прямого соседа (заголовок `X-Chain-Observed`). Если он не совпадает с `host`, который владелец ввёл в реестре, панель **не отказывает**, а помечает звено в UI: «хост в реестре `a.example.net`, вход пришёл с `198.51.100.44`». Причины расхождения законны сплошь и рядом (NAT, второй интерфейс, домен ещё не прорезолвился, CDN), а отказ на этом месте превратил бы установку в гадание. То же расхождение бокс видит сам по `self.host` и показывает в `/chain/v1/status` как `observedHostMismatch`.

**Решено: кнопки «проверить хост» в UI не будет.** Панель не звонит на боксы вообще — ни по расписанию, ни по нажатию человека (принцип карты #20, [ADR 0004](../adr/0004-mon-server-single-source-panel-passive.md)). Разовый TCP-коннект из панели на `subPort` звена выглядит безобидно, но это исходящее соединение от real server к внешнему боксу, то есть ровно та связь, которой в этой конструкции быть не должно — и которая появилась бы в firewall-логах бокса как «сюда ходит вот этот адрес». Владелец проверяет хост теми же средствами, что и раньше: `observed_addr` после join, `last_seen_at`/`lastRevision` в реестре и мониторинг (§6).

**Повторный вход переустановленного бокса.** Запись в реестре та же: владелец жмёт «перевыпустить токен» у существующего звена. Звено переходит `joined` → `pending`, `secret_hash` **не** стирается сразу — он замещается при новом join. Такое `pending` — **перевход**, и оно отличается от только что заведённого звена ровно непустым `secret_hash`: перевходящее звено остаётся в живом пути, остаётся `nextHop` своих внешних соседей и остаётся в `hops[]` со `state:"pending"`. Иначе панель перецепила бы соседей мимо живого бокса, а доставить им это было бы некому — зеркало стенда #86, разбор в §4.5.7. Так старый бокс, если он ещё жив, продолжает получать документ до момента, когда новый действительно вошёл; в момент join старый секрет умирает, и старый бокс со следующего опроса получает `404`. Имя, хост, порядок, `next_hop_id`, роль и признак активности сохраняются — для соседей и для мониторинга ничего не меняется, ревизия бампается один раз, на самом join.

**Смена хоста.** Владелец правит `host` через `POST /panel/api/chain/update/:id` → `chainRevision++`. Внешний сосед на ближайшем опросе видит новый `nextHop.host`, переписывает outbound'ы и перезапускает relay (§3.5). Join не нужен: секрет и имя не менялись. Порядок для владельца: сначала поднять бокс на новом адресе (старый ещё жив), потом сменить хост в реестре, потом гасить старый — когда `last_revision` внешнего соседа в UI догнало текущую ревизию.

### 4.5 Уход звена: состояние `draining`

Удаление звена — не одно событие, а два: **начало ухода** (звено выходит из топологии, соседи перецепляются) и **завершение** (строка реестра и hop secret умирают). Между ними звено с внешними соседями проводит до `chainDrainMinutes` (default 10) в состоянии `draining` и всё это время продолжает обслуживать соседей.

> **История решения (отменено стендом #86).** До стенда здесь стояло:
>
> ~~«Отдельного состояния `draining` в v1 не вводим. Запись в реестр мгновенна, но реального разрыва клиентов она не вызывает — удалённый бокс продолжает relay'ить, пока жив, и ровно этим держит старые соединения. Опасен не сам delete, а слишком раннее выключение бокса владельцем, а эту проблему мягкое удаление и не решает: панель не гасит боксы, гасит их человек или провайдер. Поэтому минимальный безопасный ответ — не состояние, а подсказка `safeToPowerOffWhen`. Стоимость — ноль новых состояний и ноль новых инвариантов; `draining` со всеми его крайними случаями ввести всегда успеем, если практика покажет, что владельцы гасят боксы слишком рано.»~~
>
> Рассуждение опиралось на неверную посылку: «удалённый бокс продолжает relay'ить» — да, но relay'ит он **по старой конфигурации**, а перецепиться внешнему соседу нечем. На стенде ([#86](https://github.com/SBKubric/3ax-ui-proxy/issues/86), фаза 2, шаг 10, v1.9.0-chain.3) удаление inner'а `bridge` мгновенно погасило его hop secret; его внешний сосед `proxy` получал документ **только через него**, поэтому новую ревизию — ту самую, где `nextHop` уже указывает мимо `bridge`, — он не увидел никогда. Опрос начал отвечать `404`, `proxy` замер на предпоследней ревизии и продолжил relay'ить в `bridge`. Молча: ни `stale` (он приходит только через `chainStaleMinutes`), ни ошибки в панели. Починка потребовала `reissueToken` + `x-ui chain rejoin` — ровно ручной процедуры §4.6, хотя ничего не умирало. Проблема не в том, что владелец рано выключает бокс, а в том, что панель рвёт единственный канал доставки ровно той ревизии, которая от этого канала избавляет. Поэтому `draining` вводится.

#### 4.5.1 Что делает `del`

`POST /panel/api/chain/del/:id`, тело `{"force": false, "skipDrain": false}` (оба поля необязательны), в одной транзакции:

1. Проверить 2.6.4 (активный edge удаляется только через `force` и только последним).
2. Собрать **бывших внешних соседей** — строки с `next_hop_id = <id>` в состоянии `joined`, `legacy`, `pending`-перевход (4.5.8) или `draining`. Назовём этот список `drainOuter`.
3. **Если `drainOuter` пуст или задан `skipDrain`** — удалить строку немедленно (прежнее поведение). Секрет умирает сразу, и это безопасно: обслуживать некого. Так уходит любой edge (снаружи от него только клиенты), любое `pending`-звено, любое `legacy`-звено и последний inner без edge'ей.
4. **Иначе** — строка **не удаляется**: `state = "draining"`, `drain_revision = <ревизия после бампа>`, `drain_until = now + chainDrainMinutes·60000`, `drain_outer = JSON(имена из п. 2)`. `next_hop_id`, `host`, `sub_port`, `sub_scheme`, `secret_hash` и `position` сохраняются **как есть**: draining-звено продолжает дозваниваться до своего прежнего next hop и продолжает отвечать соседям по своему прежнему адресу.
5. `reconcileTopology` перецепляет живую топологию **мимо** draining-звена: для каждого из `drainOuter` `next_hop_id` становится `next_hop_id` уходящего, `position` живых inner'ов сжимается, edge'и подвешиваются к последнему **живому** вошедшему inner'у.
6. `chainRevision++` (ровно один раз, см. 4.5.6).

Ответ `obj`:

```json
{"state":"draining","hop":"inner-2","drainRevision":43,
 "drainUntil":1758380600000,
 "safeToPowerOffWhen":{"hops":["edge-a","edge-b"],"revision":43}}
```

Для немедленного удаления (п. 3) — тот же конверт с `"state":"deleted"`, `"drainUntil":0` и пустым `hops`. Поле `safeToPowerOffWhen.hops` — **массив** имён; прежнее скалярное `hop` (одно имя, выбранное «предпочитая inner'а») заменяется им: во время draining ждать надо **всех** бывших соседей, а не одного.

`del` по draining-звену идемпотентен: он не бампает ревизию и возвращает ту же карточку с исходными `drainRevision`/`drainUntil`. Продлить draining нельзя — иначе таймаут переставал бы быть границей.

#### 4.5.2 Документы во время draining

Реестр обращается с draining-звеном ровно как со звеном, которое **есть в списке, но не в пути**:

- **Топология.** Оно не является `nextHop` ни для кого: `nextHopOf` проходит сквозь него внутрь так же, как сквозь `pending` (§2.6.3), а `reconcileTopology` не делает его звеном живого пути и не считает его `lastEnteredId` для edge'ей. Инварианты 2 и 3 (§2.7) формулируются над **не-draining** звеньями; draining-inner сохраняет свой `position` только ради порядка в списке.
- **Документы соседей.** Для любого звена снаружи от draining'а документ не меняется вообще: усечение `hops[H:]` начинается **после** draining-записи, а `nextHop` берётся из перецепленного `next_hop_id`. `edge-a` при уходе `inner-2` получает тот же документ, что и раньше, с новым номером ревизии.
- **Документ его next hop'а.** Draining-звено остаётся в `hops[]` своего next hop'а — с `"state":"draining"`. Это единственное место, где его `secretHash` ещё живёт, и именно оно делает его аутентифицируемым (4.5.3). Для `inner-1` изменение — одно значение `state` в одной записи `hops[]`: перезагрузка таблицы авторизации, без рестарта relay (§3.5).
- **Документ для него самого.** Панель продолжает его строить, и он **не усекается иначе**, чем до удаления: `self.state = "draining"`, `nextHop` прежний, `hops` = само звено + всё, что было снаружи от него (бывшие внешние соседи **и всё снаружи от них**). Последнее — уточнение к формулировке тикета: если бы `hops` сводились к «оно + прямые соседи», сосед, получив от него усечённый документ, лишился бы хэшей **своих** внешних соседей и перестал бы их впускать.

Пример. Было `real ← inner-1 ← inner-2 ← {edge-a, edge-b}`, удаляем `inner-2` (ревизия 43).

Документ `inner-1` (его отдаёт панель):

```json
 "hops":[
   {"name":"inner-1","role":"inner","host":"10.0.0.7","subPort":2096,"secretHash":"3b1f…","state":"joined"},
   {"name":"inner-2","role":"inner","host":"203.0.113.9","subPort":2096,"secretHash":"9c4a…","state":"draining"},
   {"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,"secretHash":"77de…","state":"joined"},
   {"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,"secretHash":"01bc…","state":"joined"}]
```

Документ `inner-2` (его отдаёт `inner-1` обычным усечением — ничего особенного делать не надо):

```json
{"version":1,"revision":43,"generatedAt":1758380600000,
 "self":{"name":"inner-2","role":"inner","host":"203.0.113.9","state":"draining"},
 "nextHop":{"host":"10.0.0.7","subPort":2096,"subScheme":"https",
            "subPath":"/sub/","jsonPath":"/json/","tunPath":"/tun/"},
 "activeEdge":"edge-a",
 "hops":[
   {"name":"inner-2","role":"inner","host":"203.0.113.9","subPort":2096,"secretHash":"9c4a…","state":"draining"},
   {"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,"secretHash":"77de…","state":"joined"},
   {"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,"secretHash":"01bc…","state":"joined"}],
 "ports":[ … ]}
```

Документ `edge-a` — и от `inner-1`, и от draining'а `inner-2` — байт в байт одинаков, кроме `nextHop.host`: у первого он `10.0.0.7`, у второго тоже `10.0.0.7` (4.5.3). Ревизия одна, поэтому какой бы из двух путей ни сработал первым, второй ответит `304`.

#### 4.5.3 Правило на боксе: одно поле на проводе

Единственное новое поле волны — **`self.state`** в документе (`chain.Self`):

```json
"self":{"name":"inner-2","role":"inner","host":"203.0.113.9","state":"draining"}
```

Значения — те же, что у `hops[].state`: `joined` | `legacy` | `pending` | `draining`. Отсутствие поля читается как `joined` (старый бокс с новой панелью не ломается — он просто не умеет draining).

Правило применения ровно одно:

> Если `self.state == "draining"`, то, отдавая документ внешнему соседу, звено подставляет в его `nextHop` **свой собственный `nextHop`** (`document.nextHop` целиком: `host`, `subPort`, `subScheme` и пути), а не себя.

В коду бокса это одна ветка в `TruncateDocument` (`proxy/chain_http.go`): вместо `NextHop{Host: selfHost(doc, cfg), SubPort: cfg.SubPort, SubScheme: cfg.Scheme(), …}` для draining-звена берётся `doc.NextHop` как есть. Всё остальное — усечение `hops`, `activeEdge`, `ports`, `ETag` — не меняется.

> **Почему `self.state`, а не `drainTo`/`nextHop` в записи `hops[]`.** Рассматривались два варианта. Первый — добавить каждому элементу `hops[]` поле с адресом, на который этому соседу следует ходить: честнее по форме, но это `N` новых объектов на проводе, новое правило «чей `drainTo` главнее» и второй источник истины о топологии в документе, который усечением как раз и не должен её раскрывать. Второй — одно поле у `self` плюс правило «отдаю свой next hop как их». Выбран второй: одно скалярное поле вместо массива, ноль новых сущностей, и это ровно то знание, которое у draining-звена и так есть. Побочный эффект тоже в плюс: сосед не узнаёт **ничего нового** — адрес, который он получает, он получил бы и от панели, а то, что звено уходит, из его документа не видно.

Аутентификация в обе стороны:

- **Соседи → draining.** Не меняется вовсе: `outerNeighbour` на боксе сверяет предъявленный секрет с `secretHash` из своего документа, а `hops[]` draining-звена содержит всех, кого оно обслуживало. Секрет соседа не менялся.
- **Draining → его next hop.** Если next hop — звено: оно впускает draining'а, потому что его запись с `secretHash` осталась в документе этого звена (4.5.2). Если next hop — панель: `ChainWaveService.AuthenticateHop` расширяет фильтр `state IN (joined, legacy)` до `state IN (joined, legacy, draining)`. Draining-звену нужно получить **как минимум один** документ после удаления — тот, в котором `self.state` стал `draining`; без него оно продолжало бы называть соседям себя, и стенд повторился бы.
- **Сосед → его новый upstream.** Специально ничего делать не надо: реестр перецепил соседа ещё в транзакции `del` (п. 5), поэтому его `secretHash` уже лежит в `hops[]` нового upstream'а той же ревизией 43. Момент, когда сосед перепишет `nextHop` и позвонит новому upstream'у, наступает не раньше, чем upstream применит ревизию 43, — а её он применяет раньше соседа, потому что стоит внутри и получает её первым (волна идёт изнутри наружу). Гонки «перецепился раньше, чем его там ждут» не возникает; если она всё же случится (upstream перезапускался), сосед получит `404` и повторит через 5 с в течение первых 3 минут, как после join (§4.3).

Порядок событий для `edge-a` при уходе `inner-2`:

```
t0   панель: del inner-2 → state=draining, edge-a.next_hop_id=inner-1, revision 43
t0+  inner-1 опрашивает панель → revision 43, inner-2 помечен draining
t1   edge-a опрашивает inner-2 (он ещё его next hop по document.json)
     inner-2 отдаёт: self=edge-a, nextHop.host=10.0.0.7  ← правило 4.5.3
t1+  edge-a: nextHop.host изменился → переписывает outbound'ы, рестарт relay
t2   edge-a опрашивает inner-1 → 200/304, revision 43; last_revision(edge-a)=43
t2+  панель: все из drain_outer подтвердили ≥ 43 → строка inner-2 удалена
```

Если `edge-a` успел опросить `inner-1` раньше, чем `inner-2` (например, `inner-2` уже мёртв), шаг t1 просто не случается — draining на это и рассчитан как на запасной путь, а не как на единственный.

#### 4.5.4 Завершение

Draining заканчивается, когда выполнено **любое** из двух:

1. **Подтверждение.** Для каждого имени из `drain_outer` либо `last_revision ≥ drain_revision`, либо строки с таким именем в реестре больше нет (сосед сам успел уйти — ждать его бессмысленно). `last_revision` приезжает двумя путями, и достаточно любого: собственный опрос соседа у **нового** upstream'а (`X-Chain-Seen`) и `X-Chain-Outer`, который несёт подтверждение соседа внутрь через того, кто его услышал, — в том числе через само draining-звено, пока сосед ещё ходит к нему.
2. **Таймаут.** `now ≥ drain_until`. В лог — `chain: drain timeout for <name>, outer hops still behind: <имена>`; в UI карточка звена перед исчезновением показывает те же имена. Таймаут нужен для случая, когда сосед мёртв и не подтвердит никогда, — тогда чинить будут по §4.6, а строка не должна висеть вечно.

Завершение = `DELETE` строки. Hop secret умирает в этот момент: следующий опрос draining-бокса получает `404`, он помечает себя `stale` (§3.6) и **продолжает relay'ить** — гасит его владелец или провайдер, панель на боксы не ходит (ADR 0003).

Кто это считает: `ChainService.SweepDraining()` — (а) сразу после каждой записи `last_revision` (`ChainWaveService.RecordSeen`, там же, где приезжают `X-Chain-Seen` и `X-Chain-Outer`), и (б) по тикеру раз в 60 с, запускаемому вместе с sub-сервером, — чтобы таймаут срабатывал и в цепочке, где вообще никто больше не звонит. Отдельного воркера не заводим: sweep — это один `SELECT … WHERE state='draining'` и, в худшем случае, один `DELETE`.

#### 4.5.5 Что draining-звено **не** может

Draining — терминальное состояние. Оно не отменяется и не редактируется:

- `update`, `setActive`, `reissueToken` по draining-звену → отказ `hop_is_draining`. Реактивации нет: если владелец передумал, он заводит звено заново (`add` + новый токен + переустановка бокса) — это дешевле, чем инвариант «draining умеет возвращаться», и честнее по отношению к соседям, которые уже перецепились.
- `add` нового звена с тем же именем отказывает, пока строка жива (уникальный индекс `idx_chain_hops_name`). Владельцу, которому нужно то же имя немедленно, остаётся `del … {"skipDrain":true}`.
- `POST /chain/v1/join` с токеном, выписанным этому звену, невозможен: токен был израсходован при первом входе, а `reissueToken` отказывает.
- Мониторинг (§6) продолжает зондировать его как `inner:<name>` до завершения, но бейдж в редакторе — `draining`, а не UP/DOWN: звена в цепочке уже нет, и его DOWN не значит поломки.

#### 4.5.6 Ревизии: одна на начало, одна на завершение — и почему

Утверждение «`del` бампает один раз, завершение не бампает, потому что документы оставшихся звеньев не меняются» проверено против правила усечения (§3.2) и **верно наполовину**. Точная формулировка:

- **Начало (`del`) бампает ровно один раз.** Меняются: `nextHop` бывших внешних соседей, `self.state` уходящего и одна `hops[].state` в документе его next hop'а. Всё это одна транзакция — одна ревизия.
- **Завершение не меняет ни одного документа снаружи от уходящего.** Документ любого звена снаружи — это суффикс `hops[]` начиная с него самого; draining-запись лежит **до** этого суффикса, поэтому в него не попадает, а `nextHop` этих звеньев уже перецеплен ревизией `del`. Для `edge-a`, `edge-b` и всего, что дальше, завершение — событие без следа.
- **Но документ его next hop'а меняется:** из `hops[]` пропадает одна запись. Поэтому:
  - если next hop draining-звена — **панель**, завершение не меняет ничьего документа и **не бампает** ревизию;
  - если next hop — **звено**, завершение бампает ревизию **второй раз**. Это не оптимизационный вопрос, а отзыв доступа: без бампа у next hop'а в памяти остался бы `secretHash` строки, которой больше нет, и он впускал бы мёртвый секрет до следующей случайной ревизии.

Цена второго бампа мала и посчитана: по §3.5 изменение состава `hops[].secretHash` — «перезагрузить таблицу авторизации, без рестарта»; у всех остальных звеньев документ побайтово тот же, кроме `revision`/`generatedAt`, то есть последняя строка таблицы §3.5 — «ничего, только записать `document.json`». Ни одного рестарта relay во всей цепочке.

#### 4.5.7 Зеркальный случай: переустановка inner'а (`reissueToken`)

Стенд наступил на ту же грань с другой стороны. §4.4 разрешает перевыпустить токен вошедшему звену: оно переходит `joined → pending`, `secret_hash` не стирается, старый бокс продолжает работать до нового join'а. Но `pending`-звенья по §2.6.3 **невидимы в документах** — значит, в момент `reissueToken` панель перецепила бы внешнего соседа мимо живого inner'а, а доставить ему это некому: канал соседа к панели идёт через этот самый inner, который про новую ревизию узнаёт, но себя из неё не вычёркивает. Тот же тихий замёрзший сосед.

Различаем два `pending`:

| | условие | видимость |
|---|---|---|
| **новое звено** | `state='pending' AND secret_hash = ''` | невидимо: не в документах, не в пути, соседи его не видят (§2.6.3) |
| **перевход** | `state='pending' AND secret_hash <> ''` | видимо: остаётся в пути, остаётся `nextHop` соседей, остаётся в `hops[]` со `state:"pending"` |

Дискриминатор — непустой `secret_hash`: он есть только у того, кто уже входил. Отдельной колонки и отдельного состояния не заводим; `chain.StatePending` на проводе один, потому что боксу разница не нужна — соседу важно, что звено на месте, а самому перевходящему боксу важен только его собственный `hopSecret`.

Из этого следует и поведение `del` по перевходящему звену: п. 2 (4.5.1) считает его нормальным соседом, и уходит оно через `draining`, как всякое живое.

#### 4.5.8 Вставка (§2.6.3) не затронута — подтверждаем

Вставка нового inner'а между существующими ничего не удаляет: точка вставки (`A` в примере §2.6.3) остаётся `joined` и продолжает обслуживать `B` всё время, пока `M` в `pending`. Единственная ревизия, меняющая `B.nextHop`, приезжает к `B` через живого `A` — ровно тем каналом, который на удалении и рвался. Ни `draining`, ни правило 4.5.3 здесь не участвуют.

Единственное уточнение: `reconcileTopology` при вставке должен пропускать draining-звенья так же, как при удалении, — вставка в цепочку, где кто-то сейчас уходит, кладёт новое звено в живой путь, а не за draining'ом.

#### 4.5.9 Мёртвое звено

Если удаляемое звено уже не отвечает, draining ничего не даёт: обслуживать соседей некому, и завершение придёт по таймауту. Для этого случая у `del` есть `{"skipDrain": true}` — им пользуется runbook §4.6, где сосед всё равно чинится вручную через `reissueToken` + `x-ui chain rejoin`. Автоматически «мёртвый» не определяется: `last_seen_at` отстаёт по совершенно нормальным причинам (подтверждения едут внутрь через несколько опросов), и панель, решившая за владельца, что бокс мёртв, ошибалась бы ровно в тот момент, когда цепочка просто медленная.

### 4.6 Внезапная смерть inner'а

Симптом: внешний сосед мёртвого inner'а перестал получать документ, `stale`, но **продолжает relay'ить** на мёртвый адрес — то есть трафик уже не идёт, а конфигурацию починить некому. Автоматически он выправиться не может принципиально: он не знает ничего глубже своего next hop (в этом и смысл усечения), а его next hop мёртв. Значит — ручная процедура.

Runbook (в `docs/runbooks/proxy-front.md`, раздел «Смерть inner'а»):

1. **В панели:** удалить мёртвое звено из реестра — `del` с телом `{"skipDrain": true}`. Реестр перецепляет внешнего соседа на следующее внутрь звено и бампает ревизию. Панель знает всю топологию, поэтому шаг тривиален. `skipDrain` здесь по делу: draining (§4.5) держит удалённое звено как транзит для соседей, а транзита через мёртвый бокс не бывает — без флага строка просто провисела бы `chainDrainMinutes` впустую. Во всех остальных случаях — **живое** звено, которое владелец убирает из цепочки, — `del` вызывается без флага, и §4.6 не нужен вовсе: сосед перецепляется сам через draining.
2. **В панели:** перевыпустить join-токен внешнему соседу (`reissueToken`). Он перейдёт в `pending`; его новый вход и вернёт ему связь.
3. **На боксе внешнего соседа** (ssh):

   ```
   x-ui chain rejoin --next-hop <host нового next hop> [--sub-port 2096] [--scheme https] --token <перевыпущенный токен>
   ```

   Команда: переписывает `nextHop` в `proxy.json`, стирает `hopSecret`, выполняет `POST /chain/v1/join` на указанный адрес (проброс внутрь дальше делает уже он сам), записывает полученные секрет и документ, перезапускает relay и sub-сервер. Без `--token` — отказ с подсказкой, где его взять. Флаги обязательны: угадывать новый next hop боксу неоткуда.

   **Решено: rejoin всегда требует свежий join-токен и не требует `--yes`.** Re-join по действующему `hopSecret` (звено панели уже известно, меняется только next hop) отклонён: это превратило бы hop secret в долгоживущий пропуск, которым изъятый бокс мог бы перецепить себя на любой адрес и предъявить себя цепочке заново — ровно то, от чего защищает одноразовый токен, выдаваемый владельцем в панели. Подтверждения (`--yes`) или предварительного `x-ui chain status` команда не просит: токен и так одноразовый, он выпущен владельцем минуту назад именно для этого бокса, и не туда введённая команда просто не пройдёт join. Лишний флаг в стрессовой ситуации — ещё один способ ошибиться, а не защита.
4. Проверить `x-ui chain status` на боксе (`stale: false`, `relay.running: true`) и `lastRevision` в реестре панели.
5. Всё, что снаружи от вылеченного звена, чинится волной само — никуда больше ходить не нужно.

Почему нельзя обойтись одним шагом 3 без 2: join-токен одноразовый и уже израсходован при первом входе звена; без перевыпуска бокс не докажет соседу, кто он.

### 4.7 Переключение активного edge

**Подтверждаем: ни входа, ни выхода не требует.** Оба edge уже `joined`, у обоих есть hop secret, оба уже relay'ят и уже качают подписки. Переключение — одна запись в реестре (`is_active`) плюс `chainRevision++`; меняется ровно одно следствие: `GetProxyOverride()` (2.3) начинает возвращать другой хост, и панель подставляет его в новые конфиги и ссылки подписки. На звеньях не меняется ничего (§3.5). Переключение мгновенно для всех, кто перечитает подписку; уже выданные конфиги продолжают ходить через прежний edge, пока он жив, — и это правильно, иначе переключение рвало бы клиентов.

### Тронутые файлы

Новые:
- `proxy/join.go` (+ `join_test.go`) — join-клиент бокса, проброс `POST /chain/v1/join` внутрь, `X-Chain-Observed`/`X-Chain-Forwarded`.
- `proxy/joinpage.html` — join-страница вместо страницы вставки манифеста.
- `web/service/chain_join.go` (+ `_test.go`) — проверка токена, выпуск hop secret, `joined`, ревизия (одна транзакция).
- `web/service/chain_drain.go` (+ `_test.go`) — `draining`: начало в `Delete`, `SweepDraining()` (подтверждение и таймаут), завершение и второй бамп (§4.5).
- `docs/adr/0003-chain-document-and-join-token.md` — отменяет манифестную часть ADR 0001.

Вставки:

| файл : место | вставка |
|---|---|
| `proxy/setup.go` → `proxy/join.go` | приём токена вместо манифеста; страница остаётся одноразовой (§5.4) |
| `proxy/proxy.go:85` (`bootstrap`) | условие bootstrap — пустой `hopSecret`, а не отсутствие файла манифеста |
| `main.go:576` (usage) и блок подкоманд | `chain rejoin`, `chain status`, `chain join-url` |
| `install.sh:2656-2676` | `PROXY_JOIN_TOKEN`, `PROXY_NEXT_HOP`, `PROXY_NEXT_HOP_SUB_PORT` вместо `PROXY_UPSTREAM_*`/`PROXY_RELAY_MANIFEST`/`PROXY_EXTRA_PORTS` |
| `update.sh` | отказ на боксе с legacy-`proxy.json` с указанием на runbook |
| `docs/runbooks/proxy-front.md` | разделы «Вход звена», «Смерть inner'а», «Удаление звена» (в т. ч. draining и когда нужен `skipDrain`) |

### Открытые вопросы владельцу

1. **Второй бамп ревизии на завершении ухода.** Владелец формулировал требование как «`del` бампает один раз, завершение не бампает». Проверка против правила усечения (§4.5.6) показала, что это верно, когда next hop уходящего — панель, и неверно, когда next hop — звено: у него из `hops[]` пропадает запись уходящего, и без новой ревизии он продолжал бы впускать уже мёртвый секрет. В §4.5.6 записан второй бамп. Альтернатива — не бампать и смириться с окном до следующей случайной ревизии, в течение которого один конкретный next hop впускает мёртвый секрет (ничего, кроме собственного документа уходящего, этот секрет не открывает). Подтвердить выбор.
2. **`chainDrainMinutes` = 10 по умолчанию.** Десять минут — это 20 интервалов опроса при `chainPollSeconds = 30`, то есть запас на два-три пропущенных опроса и один рестарт бокса. Подтвердить, или назвать другое число.

## 5. Сторона бокса и миграция

Раздел описывает, что меняется на самом proxy front (звене цепочки): формат `proxy.json`, правила загрузки со старыми ключами, bootstrap/join-режим вместо setup page, переменные установщика, поведение `update.sh`, CLI и судьбу пакета `relaymanifest`.

Отправная точка: сегодня бокс читает `upstreamHost` + relay manifest (`proxy/config.go:19-66`), а недостающий манифест владелец вставляет в setup page (`proxy/setup.go:26-262`, [ADR 0001](../adr/0001-relay-manifest-via-setup-page.md)). В цепочке всё это приходит в **chain document** (документе цепочки) от **next hop** (следующего звена), поэтому конфиг бокса сводится к «кто мой next hop и чем я ему доказываю, что я — это я».

### 5.1 `proxy.json` v2

| ключ | тип | обяз. | дефолт | смысл |
|---|---|---|---|---|
| `version` | int | да | — | версия формата конфига. `2` — цепочка. Отсутствует/`0` → файл трактуется как v1 (см. §5.3) |
| `nextHop.host` | string | нет¹ | `""` | адрес next hop (IP или домен): куда relay'ит dokodemo-door и откуда берутся подписки и документ цепочки. Для первого inner front — адрес real server |
| `nextHop.subPort` | int | нет | `2096` | порт sub-сервера next hop: подписки, `/chain/v1/*` |
| `nextHop.subScheme` | string | нет | `https` | `http`\|`https`; к `https` бокс ходит с `InsecureSkipVerify` (как сегодня, `proxy/subserver.go:50-55`) |
| `hopSecret` | string | нет¹ | `""` | **hop secret** (секрет звена), выданный панелью при join. Bearer для `GET /chain/v1/document` у next hop. Пусто → bootstrap/join-режим |
| `subListen` | string | нет | `""` | адрес привязки своего sub-сервера (`""` = все интерфейсы) |
| `subPort` | int | нет | `2096` | порт своего sub-сервера: подписки, join page, `/chain/v1/*` |
| `relayListen` | string | нет | `"::"` | адрес привязки dokodemo-door; `0.0.0.0` на хостах без IPv6 |
| `domain` | string | нет | Host запроса | публичный хост бокса: ссылки подписки и URL join page |
| `cert` | string | нет | `""` | путь к TLS-сертификату sub-сервера |
| `key` | string | нет | `""` | путь к TLS-ключу sub-сервера |
| `stateDir` | string | нет | `/etc/x-ui/chain` | каталог кэша: последний принятый документ цепочки |
| `pollSeconds` | int | нет | `30` | локальный override периода волны; `0` = как в документе (`chainPollSeconds`) |
| `staleMinutes` | int | нет | `60` | сколько бокс молчит о недоступности next hop, прежде чем пометить себя `stale` (релей при этом не останавливается) |

¹ Не обязательны в файле, но обязательны к моменту старта релея: без `nextHop.host` их спрашивает join page или даёт `PROXY_NEXT_HOP`; `hopSecret` пишет сам бокс после успешного join.

> **Решение об именах:** ключи TLS не переименовываются в `certFile`/`keyFile`: в коде и установщике они называются `cert`/`key` (`proxy/config.go:63-64`, `install.sh:2745-2746`). Оставляем `cert`/`key` — переименование ничего не даёт и ломает существующие скрипты. Так же оставлен `domain` (рамка его не перечисляет, но без него бокс не может напечатать URL join page и ссылки подписки — `proxy/subserver.go:149-159`). `pollSeconds`/`staleMinutes` добавлены как локальные overrides для отладки одного бокса.

```json
{
  "version": 2,
  "nextHop": { "host": "10.0.0.7", "subPort": 2096, "subScheme": "https" },
  "hopSecret": "9f3c…32 символа…",
  "subListen": "",
  "subPort": 2096,
  "relayListen": "::",
  "domain": "edge-ams.example.com",
  "cert": "/root/cert/fullchain.pem",
  "key": "/root/cert/privkey.pem",
  "stateDir": "/etc/x-ui/chain"
}
```

Семантика маркера `version`: это версия **формата файла бокса**, не версия документа цепочки (`"version":1` в документе) и не ревизия реестра (`revision`). Бокс отказывается стартовать только при `version` **больше** известной ему (`proxy config …: version 3 is newer than this build understands`); `version: 2` — нормальная загрузка, отсутствие `version` — legacy (§5.3). Установщик всегда пишет `"version": 2`.

### 5.2 Что уходит из конфига

| ушедший ключ | где было | почему больше не нужен |
|---|---|---|
| `relayManifestPath` | `proxy/config.go:32` | список relayed ports приходит в документе цепочки (`ports[]`, с `network`), панель считает его сама; файла на боксе нет |
| `extraPorts` | `proxy/config.go:44` | те же `ports[]` c `source: "awg"\|"wg"\|"mtproto"\|"extra"`; ручные добавки живут в реестре (`chainExtraPorts`) |
| `upstreamHost` | `proxy/config.go:22` | переименован в `nextHop.host`: для inner front это следующее звено, а не real server |
| `upstreamBase` | `proxy/config.go:53` | собирается из `nextHop.subScheme`/`host`/`subPort`; заодно исчезает второй источник правды об адресе соседа |
| `subPath`, `jsonPath` | `proxy/config.go:61-62` | пути подписки приходят в `nextHop.subPath`/`jsonPath`/`tunPath` документа — панель знает их из своих настроек (`subPath`, `subJsonPath`, `subTunPath`) |
| `tunPath` | запланирован, [tunnel-subscription §7](tunnel-subscription.md) | в код бокса не попадает как ключ конфига: сразу из документа |

Ключ `SubEnabled()` (`proxy/config.go:128`) больше не зависит от `upstreamBase`: sub-сервер на звене есть всегда — он же несёт join page и `/chain/v1/*`.

### 5.3 Правила загрузки конфига (`proxy.LoadConfig`)

1. **v2 с `hopSecret`** — обычная загрузка: бокс читает кэш документа из `<stateDir>/document.json`, поднимает relay и полный sub-сервер, начинает волну.
2. **v2 без `hopSecret`** — bootstrap/join-режим (§5.4). Не ошибка.
3. **Файл с legacy-ключами** — любой из `upstreamHost`, `upstreamBase`, `relayManifestPath`, `extraPorts`, `subPath`, `jsonPath` (полный список полей v1 из `proxy/config.go:19-66`; `xrayConfigPath` ушёл ещё в ADR 0001 и сюда не возвращается) — загружается, но:
   - на каждый найденный legacy-ключ печатается одна строка WARN с заменой:
     ```
     WARN proxy config /etc/x-ui/proxy.json: "upstreamHost" is a v1 key, ignored — the chain takes it from "nextHop.host" (join this box: x-ui chain join-url)
     WARN proxy config /etc/x-ui/proxy.json: "relayManifestPath" is a v1 key, ignored — relayed ports now arrive in the chain document
     ```
   - значения legacy-ключей **не используются**, кроме двух подсказок для оператора: `upstreamHost` подставляется в поле «next hop» join page как предзаполненное значение, `subPort`/`subListen`/`cert`/`key`/`domain`/`relayListen` (общие для v1 и v2) читаются как есть;
   - бокс уходит в bootstrap/join-режим;
   - **никогда не fatal**: старый `proxy.json` не должен оставлять бокс без сервиса — на нём ещё крутится живой релей до переустановки.

> Почему мягко, если бокс всё равно переустанавливается (вопрос тикета #72): переустановка — это операция владельца, а `update.sh` прилетает на бокс раньше и автоматически. Жёсткая ошибка превратила бы обновление в даунтайм на всех существующих фронтах сразу; WARN + bootstrap оставляет бокс живым (sub-сервер с join page) и объясняет, что делать. Значения старых ключей при этом не мигрируются: цепочка всё равно выдаёт `hopSecret` и порты только через join.

### 5.4 Bootstrap/join-режим и join page

**Что работает в bootstrap-режиме:** только sub-сервер, на том же `subListen`/`subPort`/TLS, что и в рабочем режиме, и только с маршрутами join: `GET /join/<token>` (форма), `POST /join/<token>` (отправка), `POST /chain/v1/join` (проброс внутрь, §4). Релея нет: относящиеся к цепочке порты неизвестны до первого документа. Маршруты подписок в этом режиме отвечают `503`.

**Join page заменяет setup page** (`proxy/setup.go` → `proxy/join.go`, шаблон `proxy/joinpage.html`):

- путь и токен — схема ADR 0001 сохраняется: случайный одноразовый токен страницы (32 байта base64url, `proxy/setup.go:49-50`), путь `/join/<token>`, URL печатается в лог и кладётся рядом с конфигом — файл переименован `proxy-setup.url` → `chain-join.url` (`proxy.JoinURLPath(cfgPath)`, ср. `proxy/setup.go:230-234`);
- поля формы: **next hop host** (показывается и обязательно, только если `nextHop.host` пуст; иначе показан как факт), опционально **next hop sub port** (дефолт 2096), и **join token** — join-токен, выданный панелью в реестре цепочки. Поля манифеста нет;
- по отправке бокс пишет `nextHop` в `proxy.json`, шлёт `POST /chain/v1/join {token, host?}` на next hop, ждёт ответ `{hopId, name, secret, document}`;
- **успех:** страница показывает имя звена и его роль (`inner`/`edge`), next hop, ревизию документа и список relayed ports, которые сейчас поднимаются; бокс пишет `hopSecret` в `proxy.json` (0600) и документ в `<stateDir>/document.json`, затем **в том же процессе** стартует relay xray и полный sub-сервер — ровно как сегодня после приёма манифеста (`proxy/proxy.go:30-66`);
- **ошибка:** next hop недоступен, токен просрочен/использован → `400`/`502` с текстом, токен страницы **не сгорает**, форму можно отправить снова;
- страница исчезает после первого успешного join: тот же URL отвечает `404` (`proxy/setup.go:104,151`), файл `chain-join.url` удаляется;
- неинтерактивный путь: `PROXY_NEXT_HOP` + `PROXY_JOIN_TOKEN` при установке → join выполняется при первом старте сервиса, join page не поднимается вообще (если join не удался — страница всё-таки поднимается, чтобы владелец мог исправить хост или токен).

**TLS на join page.** По умолчанию сертификат у бокса есть уже на момент первой загрузки страницы: установщик выпускает **Let's Encrypt-сертификат на IP-адрес бокса** (§5.6, `PROXY_TLS=letsencrypt-ip`), домен для этого не нужен. Тогда join page отдаётся по **HTTPS**, и токен в неё едет по шифрованному каналу.

Если сертификата нет (`PROXY_TLS=none` или выпуск не удался), страница всё равно поднимается — но по **HTTP** и с постоянным баннером наверху формы:

```
⚠ This page is served over plain HTTP: the join token travels in clear text.
  Prefer installing the box with PROXY_JOIN_TOKEN over ssh instead.
```

Баннер — не блокировка: бокс во время установки часто и есть тот единственный канал, который у владельца работает, а отказ отдать страницу оставил бы его без входа вообще. Тот же текст (в русской локали — «токен уйдёт открытым текстом; лучше передать `PROXY_JOIN_TOKEN` через ssh») печатается в лог рядом со ссылкой на страницу.

### 5.5 Runtime-файлы и лог

| путь | что |
|---|---|
| `/etc/x-ui/proxy.json` | конфиг v2, 0600 |
| `/etc/x-ui/chain/document.json` | последний принятый документ цепочки (0600); читается при старте и используется, пока next hop недоступен |
| `/etc/x-ui/chain-join.url` | URL join page, пока она жива |
| `<bin>/proxy-relay.json` | сгенерированный конфиг relay-xray, `config.GetBinFolderPath() + "/proxy-relay.json"` (`proxy/relay.go:16-20`); перегенерируется на каждой применённой ревизии и удаляется при остановке |

Ожидаемые строки лога:

```
INFO  proxy-front: no hopSecret in /etc/x-ui/proxy.json — bootstrap mode. Join this box at: https://<host>:2096/join/<token>
INFO  proxy-front: joined chain as "ams-1" (edge), next hop 10.0.0.7:2096, revision 42
INFO  proxy-front: relaying ports [443 8443 51820] -> 10.0.0.7 via dokodemo-door (L4 passthrough)
INFO  proxy-front: chain revision 43 applied (ports +1 -0)
WARN  proxy-front: next hop 10.0.0.7:2096 unreachable (3 attempts) — keeping revision 43
WARN  proxy-front: chain document is stale (62m) — still relaying revision 43
```

### 5.6 `install.sh`, режим proxy

Новые переменные (`prompt_proxy_mode`, `install.sh:2635-2676`):

| переменная | дефолт | смысл |
|---|---|---|
| `PROXY_NEXT_HOP` | — | адрес next hop; в интерактиве спрашивается («Next hop address (inner front or the real server):») |
| `PROXY_NEXT_HOP_SUB_PORT` | `2096` | порт sub-сервера next hop |
| `PROXY_NEXT_HOP_SCHEME` | `https` | схема sub-сервера next hop |
| `PROXY_JOIN_TOKEN` | — | join-токен из реестра; в интерактиве спрашивается, пустой ответ допустим («оставлю join page») |
| `PROXY_TLS` | `letsencrypt-ip` | как бокс получает TLS для своего sub-порта: `letsencrypt-ip` \| `none` \| `manual` |

Остаются как есть: `XUI_PROXY_MODE`, `PROXY_DOMAIN`, `PROXY_SUB_PORT` (2096), `PROXY_RELAY_LISTEN` (`::`), `PROXY_CERT`, `PROXY_KEY`, `PROXY_SUB_LISTEN`.

**Новый шаг установки: сертификат на IP.** При `PROXY_TLS=letsencrypt-ip` (дефолт) установщик выпускает сертификат Let's Encrypt **на IP-адрес бокса** — домен не нужен, а домена у свежего одноразового фронта обычно и нет. IP-сертификаты Let's Encrypt короткоживущие (**≈ 6 дней**), поэтому выпуск сразу ставится на автопродление: установщик настраивает certbot/acme-клиент (тот же, что уже используется в `install.sh` для панели) с таймером обновления и хуком перезапуска sub-сервера, а пути кладёт в `cert`/`key` `proxy.json`. Короткий срок здесь — плюс: сертификат живёт не дольше самого бокса.

Значения:

- `letsencrypt-ip` (дефолт) — выпуск и автопродление, как выше. Если выпуск не удался (порт занят, CA недоступен, IP за NAT) — установка **не падает**: печатается WARN, бокс поднимается без TLS, join page идёт по HTTP с баннером (§5.4), а владелец может повторить выпуск позже.
- `manual` — прежнее поведение: TLS берётся из `PROXY_CERT`/`PROXY_KEY`, установщик ничего не выпускает и не продлевает. Пути обязательны и абсолютны, файлы должны быть непустыми PEM (и парой, если на боксе есть openssl); иначе установка **падает до каких-либо изменений** — без тихого отката на HTTP (#124). HTTP сознательно — это `none`.

Переустановка с `letsencrypt-ip` бокса, на котором уже лежит сертификат на его IP (`/root/cert/ip`, действует ещё больше суток, acme.sh его продлевает), оставляет этот сертификат и не выпускает новый: иначе через несколько переустановок за неделю Let's Encrypt упирается в лимит дубликатов, а неудачный выпуск означает HTTP (#124).
- `none` — TLS нет сознательно (бокс за внешним терминатором, стенд, отладка); sub-сервер и join page работают по HTTP.

Ушли: `PROXY_UPSTREAM_HOST`, `PROXY_UPSTREAM_BASE`, `PROXY_EXTRA_PORTS`, `PROXY_RELAY_MANIFEST`, `PROXY_SUB_PATH`, `PROXY_JSON_PATH`. Если любая из них задана — **fail fast** одним сообщением, по образцу уже существующей проверки `PROXY_XRAY_CONFIG` (`install.sh:2683-2686`):

```
PROXY_UPSTREAM_HOST is gone: a proxy front is now a chain hop. Pass PROXY_NEXT_HOP (and PROXY_JOIN_TOKEN, or use the join page). See docs/runbooks/proxy-front.md.
```

Решение — именно ошибка, а не WARN: это установка нового бокса, молчаливо проигнорированный `PROXY_EXTRA_PORTS` дал бы фронт без половины портов, и это выяснилось бы только на клиенте.

`config_proxy_mode` (`install.sh:2721-2751`) пишет `proxy.json` v2 из §5.1, создаёт `/etc/x-ui/chain` (0700) и больше не копирует манифест. Юнит не меняется: `ExecStart=… x-ui proxy -c /etc/x-ui/proxy.json` (`install.sh:2789`).

Футер (`print_proxy_footer`, `install.sh:2801-2830`):

- если `PROXY_JOIN_TOKEN` был задан и join прошёл — `Joined the chain as "ams-1" (edge) → next hop 10.0.0.7:2096` + список relayed ports;
- если токена не было (или join не удался) — как сегодня ждём до 10 с появления `/etc/x-ui/chain-join.url` и печатаем одноразовую ссылку + `Show the link again: x-ui chain join-url`;
- строки `Relay target` / `Subscription upstream` / `Relay manifest` заменяются на `Next hop` и `Chain state: /etc/x-ui/chain/`;
- хвост про host override остаётся, но указывает на реестр цепочки: «отметьте это звено активным edge в Settings → Subscription → Chain или `/proxy ams-1` в боте».

### 5.7 `update.sh` на боксе

Детект режима не меняется: `[[ -f /etc/x-ui/proxy.json ]]` (`update.sh:1981`). Дальше три ветки:

1. **v1-файл** (есть любой legacy-ключ из §5.3) → печатаем сообщение и **выходим до подмены бинаря** (`exit 1`), по образцу нынешней проверки `xrayConfigPath` (`update.sh:803-805`):
   ```
   /etc/x-ui/proxy.json is a v1 proxy-front config (key "relayManifestPath"): this release runs proxy fronts as chain hops. The box keeps running on the current binary. Re-install it as a chain hop: docs/runbooks/proxy-front.md §«Переустановка бокса».
   ```
   > **Почему до подмены бинаря:** сегодня такая проверка стоит в `config_after_update` (`update.sh:800-806`) и только предупреждает — бинарь к тому моменту уже заменён, и бокс падает в цикл рестартов. Переносим её в блок детекта proxy-режима (`update.sh:1978-1983`), чтобы «stop» действительно означал «релей продолжает работать на старой версии».
2. **v2** → обычное обновление: бинарь, юнит (`write_proxy_service_unit`, `update.sh:1668-1710`), `proxy.json` и `/etc/x-ui/chain/` не трогаются.
3. **v2 без `hopSecret`** → обновление проходит, в конце напоминание: `This box has not joined the chain yet — the join page is at: x-ui chain join-url`.

### 5.8 CLI (`main.go`)

| команда | статус | что делает |
|---|---|---|
| `x-ui proxy -c <config>` | без изменений | точка входа бокса; сама решает, v2 это, bootstrap/join или legacy (§5.3) |
| `x-ui chain join-url [-c <config>]` | **новая, замена** `proxy-setup-url` (старое имя удалено без алиаса) | печатает URL join page или «this box has already joined the chain» и `exit 1` |
| `x-ui chain status [-c <config>]` | новая | ходит на свой `GET /chain/v1/status` по `127.0.0.1:<subPort>` и печатает: имя, роль, next hop, ревизию, `stale`, `draining` (§4.5), relayed ports, время последней успешной волны |
| `x-ui chain rejoin --next-hop <host> [--sub-port <n>] --token <t> [-c <config>]` | новая | переписывает `nextHop`, обнуляет `hopSecret`, выполняет join немедленно (без страницы) и перезапускает релей; так runbook чинит цепочку после гибели inner front. `--token` обязателен (свежий токен из реестра, §4.6), подтверждения `--yes` нет |
| `x-ui chain ports [-o <file>]` | **на панели**, новая, замена `relay-manifest` (старое имя удалено без алиаса) | debug-экспорт вычисленного списка relayed ports (`port`, `network`, `tag`, `source`) — того самого, что уходит в документ цепочки |

> **Решение об именах: deprecated-алиасов нет.** `x-ui proxy-setup-url` и `x-ui relay-manifest` **удаляются** вместе с манифестом и setup page, а не остаются алиасами «на переходный период»: обе команды жили ровно в одном сценарии — установка бокса по ADR 0001, — а бокс из этого сценария в цепочку всё равно не попадает без переустановки (§5.3, §5.7). Алиас сохранял бы старое имя для скриптов, которые после переустановки бокса и так переписываются, и продолжал бы обещать «manifest», которого больше нет. Несуществующая подкоманда печатает usage — этого достаточно, чтобы понять, что имя сменилось.

Обновляется блок `Commands:` в usage (`main.go:568-578`).

### 5.9 Судьба пакета `relaymanifest`

Пакет переименовывается в **`chainports`** (`chainports/chainports.go`, `chainports/chainports_test.go`) и становится сборщиком списка relayed ports для документа цепочки.

Остаётся:

- `Build(rawXrayConfig []byte) ([]Port, error)` — парсит xray-конфиг панели и возвращает `Port{Port int, Network string, Tag string, Source string}` (`source: "xray"`);
- правила пропуска — `skipReason` в нынешнем виде, перенесённый с бокса (`proxy/relay.go:49-68`): `port <= 0`, `tag == "api"`, loopback-`listen`, unix-socket `@…`, TPROXY/REDIRECT-инбаунд (суффикс `-tproxy-in`, `sockopt.tproxy`, `followRedirect`). Это единственная часть ADR 0001, которая доживает до цепочки без изменений;
- `Network` остаётся `"tcp,udp"` для xray-инбаундов (`proxy/relay.go:118`), а для AWG/WG/MTProto/extra приходит от вызывающего сервиса.

Удаляется:

- `Validate()` со строгим декодированием и whitelist'ом (`relaymanifest.go:129-155`) — вставленных руками файлов больше нет, проверять нечего;
- `Marker`/`Version`/ключ `relayManifest` (`relaymanifest.go:21-22,59-63,90-94`) — версионируется документ цепочки, не манифест;
- `FromFile()` и `Manifest.JSON()` в нынешнем виде; типы `Inbound`/`Settings`/`StreamSettings` остаются как внутренние структуры разбора;
- сама CLI-команда `x-ui relay-manifest` — имя удаляется без алиаса, остаётся только `x-ui chain ports` (§5.8).

Потребители: `web/service/chain_service.go` (сборка документа), `x-ui chain ports`. Кнопка «Show manifest» в Settings → Subscription (`web/html/settings/panel/subscription/general.html:90-96`) заменяется на «Показать порты цепочки» в редакторе цепочки. Вызов `relaymanifest.Validate` из `proxy/relay.go:100` и `proxy/setup.go:141` уходит вместе с манифестом.

### 5.10 Переустановка существующего бокса (для runbook)

На стенде это: панель остаётся real server, новый VPS `bridge` ставится с нуля как **inner front**, а нынешний `proxy` переустанавливается как **edge front** (§10). Тот же порядок годится и для цепочки из одного звена — тогда переустанавливаемый бокс сразу edge с next hop = панель.

1. На панели: Settings → Subscription → *Chain* → создать звено (`name`, `role`, `next hop`), получить **join token** (TTL 24 ч, одноразовый). Для inner front next hop — сама панель; для edge next hop вычисляет панель (последний inner).
2. На боксе: `systemctl stop x-ui` — релей останавливается, клиенты на время переустановки идут мимо (или заранее переключить host override на другое edge).
3. Сохранить старый конфиг на всякий случай: `cp /etc/x-ui/proxy.json /root/proxy.json.v1`.
4. Удалить legacy-артефакты: `rm -f /etc/x-ui/proxy.json /etc/x-ui/relay-manifest.json /etc/x-ui/proxy-setup.url`.
5. Переустановить в режиме звена:
   ```bash
   XUI_PROXY_MODE=1 \
   PROXY_NEXT_HOP=<real-ip-или-inner-ip> PROXY_NEXT_HOP_SUB_PORT=2096 \
   PROXY_JOIN_TOKEN=<токен из п.1> \
   PROXY_DOMAIN=<домен бокса> PROXY_SUB_PORT=2096 \
   bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)
   ```
   Без `PROXY_JOIN_TOKEN` установщик напечатает ссылку join page — токен вставляется в неё.
6. Проверить: `x-ui chain status` → `joined`, ревизия совпадает с панелью; `ss -ltnup | grep -E ":(443|51820|2096)"`; на панели звено в состоянии `joined` с свежим `last_seen_at`.
7. Убедиться, что ключей на боксе нет: `grep -R "privateKey\|password" /etc/x-ui /usr/local/x-ui/bin || echo clean`.
8. Если бокс — edge: отметить его активным (`/proxy <name>` в боте или кнопка в реестре) и проверить подписку клиента.
9. Следующее звено снаружи переустанавливается тем же порядком, с `PROXY_NEXT_HOP` = адрес только что введённого звена.

### Тронутые файлы

Новые:

- `proxy/chain.go` (+ `chain_test.go`) — клиент волны, кэш документа, применение ревизии, `/chain/v1/status`.
- `proxy/join.go`, `proxy/joinpage.html` (+ `join_test.go`) — join page и `POST /chain/v1/join`; на их место встают `proxy/setup.go`, `proxy/setup_test.go`.
- `chainports/chainports.go` (+ `_test.go`) — бывший `relaymanifest/` (§5.9).
- `docs/adr/0003-chain-document-and-join-token.md`.

Правки:

| файл : место | правка |
|---|---|
| `proxy/config.go:19-176` | структура v2, `LoadConfig` c WARN'ами и bootstrap-режимом |
| `proxy/proxy.go:16-118` | `bootstrap()` → join-режим; перезапуск релея по новой ревизии |
| `proxy/relay.go:82-140` | `BuildRelayConfig` берёт порты из документа, а не из манифеста; `skipReason` уезжает в `chainports` |
| `proxy/subserver.go:47-285` | база upstream из `nextHop`, пути из документа, маршруты `/chain/v1/*` |
| `main.go:647-699`, `main.go:568-578` | команды §5.8 и usage |
| `install.sh:2628-2830` | `prompt_proxy_mode`, `config_proxy_mode`, `print_proxy_footer` (§5.6) |
| `update.sh:800-806`, `update.sh:1978-1983` | ветки §5.7 |
| `docs/runbooks/proxy-front.md` | §3 «Установка через setup page» → «Введение звена в цепочку», новый раздел «Переустановка бокса» (§5.10) |
| `README.md:253`, `README.ru_RU.md:253` | раздел *Proxy front (anti-blocking)* / *Прокси-фронт*: цепочка вместо одного фронта |

`x-ui.sh` не трогаем: proxy-режим в нём отдельных пунктов меню не имеет (только очистка NDP-proxy, `x-ui.sh:353-358`).

### Открытые вопросы владельцу

Открытых вопросов нет.

## 6. Мониторинг через каждое звено

Итог тикета [«Мониторинг через каждое звено»](https://github.com/SBKubric/3ax-ui-proxy/issues/73) карты [Proxy chain](https://github.com/SBKubric/3ax-ui-proxy/issues/68). Термины — по `CONTEXT.md` (chain, hop, edge front, inner front, active edge, standby edge, chain registry, chain document) и по [CONTEXT.md `3ax-ui-monitoring`](https://github.com/SBKubric/3ax-ui-monitoring/blob/main/CONTEXT.md) (mon-server, mon-client, target, path, probe account, tunnel probe, heartbeat). Расширяет [Контракт API панели для mon-server (v1)](monitoring-contract.md) и [Мониторинг: панельная часть](monitoring-panel.md) без разрыва совместимости; протокол mon-server↔mon-client — [mon-protocol.md](https://github.com/SBKubric/3ax-ui-monitoring/blob/main/docs/spec/mon-protocol.md), репо `SBKubric/3ax-ui-monitoring`.

Реализация — после мержа эпика мониторинга [#43](https://github.com/SBKubric/3ax-ui-proxy/issues/43) и тикетов реестра/волны этой карты; решения принимаются сейчас, чтобы контракт не потребовал `/mon/v2/`.

### 6.1 Расширение контракта (без версии)

По контракту §1: «совместимые изменения (новые необязательные поля, новые `reason`, новые `kind` событий) не меняют версию; mon-server обязан игнорировать неизвестные поля». Всё ниже — новые опциональные поля и новые допустимые значения существующих строковых enum'ов (`path`), а не новая семантика существующих полей: старый mon-server, который не знает про `chain`, продолжает работать как раньше — `override.host` в `GET /state` по-прежнему указывает на active edge (он и так вычисляется из реестра по решению архитектора §2 рамки), `path=proxy` по-прежнему значит «через host override». Версию контракта не бампаем.

#### `GET /state` — поле `chain`

```json
{
  "...": "…как в контракте v1…",
  "chain": {
    "revision": 42,
    "activeEdge": "ams-1",
    "hops": [
      {"name": "core-1", "role": "inner", "host": "10.0.0.7",     "state": "joined"},
      {"name": "ams-1",  "role": "edge",  "host": "203.0.113.10", "state": "joined"},
      {"name": "ams-2",  "role": "edge",  "host": "203.0.113.20", "state": "joined"},
      {"name": "fra-1",  "role": "edge",  "host": "203.0.113.30", "state": "pending"}
    ]
  }
}
```

- `chain.revision` — `chainRevision` реестра (монотонный `int64`, растёт на join/rename/delete/переключение active edge); отдельная величина от хэша `revision` контракта, но участвует в нём (см. ниже). Отсутствует (поле `chain` не приходит), если реестр цепочки пуст (панель без цепочки — старое поведение, `override` как раньше).
- `hops` — **все** звенья реестра, и `inner`, и `edge` (inner'ы тоже пробируются, §6.3), в порядке `role`, затем `name` asc. `role` ∈ `inner`\|`edge`, `state` = состояние звена в реестре (`pending`/`joined`/`legacy`). `host` у `pending` — адрес, введённый владельцем при создании звена (join ещё не подтвердил его), поэтому такие звенья не пробируются.
- Поле `edges` (только edge-звенья) — прежнее имя этого списка; `hops` его замещает. Старый mon-server, читающий `edges`, не сломается: панель отдаёт `edges` как усечённую до `role=edge` копию `hops`, пока в контракте живёт совместимость с ним; новый mon-server читает `hops` и игнорирует `edges`.
- `activeEdge` — `name` active edge или `null`, если ни одно не активно (реестр пуст либо host override выключен легаси-путём).

**Ревизия (§4.2 контракта) — канонический объект дополняется:**

```json
{"override": {"enabled": true, "host": "front.example.net"},
 "inbounds": [{"kind":"xray","inboundId":12,"protocol":"vless","port":443,"enable":true}, "…"],
 "probeSubId": "k3j9d8s7f6g5h4j3",
 "chain": {"activeEdge": "ams-1",
           "hops": [{"name":"core-1","role":"inner","host":"10.0.0.7","state":"joined"},
                    {"name":"ams-1","role":"edge","host":"203.0.113.10","state":"joined"}, "…"]}}
```

- `chain.hops` отсортированы по `role`, затем `name`, входят **все** звенья в состоянии `joined`/`legacy` — и edge, и inner (звенья, которые реально можно опрашивать; `pending` не влияет на targets, поэтому не входит в хэш — вход в реестр без подтверждения join не должен пересобирать targets).
- `activeEdge` — **в хэше**, а не только в `hops`: `proxy`-алиас указывает на активное edge, и когда host override переключается между уже известными edge (без изменения списка `hops`), targets `path=proxy` (для старого mon-server) и `path=edge:<active>` (де-факто тот же адрес) должны пересобраться — адрес поменялся, а `hops` как список нет.
- Переименование звена не входит в хэш само по себе, только если меняется набор/состав/host/state/activeEdge — рифмуется с тем, что `remark`/`tag` inbound'а не входят в хэш (переименование не двигает targets), но у звена переименование **меняет** `path` (`edge:<name>` / `inner:<name>`), поэтому по факту потянет пересборку, т.к. `name` — часть ключа объекта `hops[]`.

#### `GET /probe/configs?hop=<name>`

Третий режим ручки — наравне с «без параметров» (path `proxy`, через host override) и `?host=` (path `direct`):

- `GET /probe/configs?hop=<name>` — рендерит конфиги probe-набора с адресом **этого звена** (любого: и edge, и inner) вместо host override, независимо от роли и от того, активно ли оно. `host`, порт, SNI, ключи — как для `direct`/`proxy`, меняется только адрес.
- `?edge=<name>` — **сохранённый синоним** `?hop=`: параметр принимается и работает ровно так же (имя ищется среди всех звеньев), чтобы mon-server, написанный под первую редакцию контракта, продолжал работать. Оба параметра сразу — `409 unknown_hop`, если имена разные.
- `409 unknown_hop` — имени нет в реестре (прежний код `unknown_edge` остаётся синонимом для старых клиентов).
- `409 hop_not_joined` — имя есть, но звено в состоянии `pending` (join не завершён, relay на нём ещё не поднят; для `legacy` ручка работает — это донный edge миграции, эквивалент старого `proxyOverrideHost`). Прежний код — `edge_not_joined`.
- `?host=` и поведение без параметров — не меняются (см. §6.3 про то, что даёт `proxy` при наличии цепочки).

Почему проба inner'а осмысленна: звено relay'ит **те же** relayed ports один-в-один (§3.8), поэтому probe-конфиг с адресом inner'а проверяет отрезок `real ← …inner…`, то есть путь до real server без внешней части цепочки. Разница `inner:<name>` UP + `edge:<name>` DOWN сразу называет виновный сегмент — это то, ради чего пробинг inner'ов и включён.

#### `path`

Четыре значения вместо двух: `direct`, `proxy` (алиас = active edge, оставлен для старых mon-server), `edge:<name>` и `inner:<name>` (`<name>` — `[a-z0-9-]{1,32}`, как имя звена в реестре). Ключ результата `(monClientId, kind, inboundId, path)` не меняется по форме — просто у `path` больше допустимых значений. `/events` и `/stats` принимают `edge:<name>`/`inner:<name>` как обычную строку `path`, без валидации по реестру (панель не хранит topology мониторинга — она пассивный приёмник, [ADR 0004](../adr/0004-mon-server-single-source-panel-passive.md)): неизвестное имя звена в `path` события — не ошибка, просто ещё одна строка в ленте. Словарь `reason` не меняется — деградация звена выглядит так же, как деградация proxy front сегодня (`tcp_refused`, `tls_timeout`, …).

#### Удаление и переименование звена

- **Удаление** звена из реестра — каскадное удаление хранимого состояния по префиксу path: все строки `mon_targets`/`mon_events`/`mon_stats_current`/`mon_stats_rollup` с `path = 'edge:<name>'` (для inner — `path = 'inner:<name>'`) или (для активного на момент удаления edge) `path = 'proxy'`, если это имя было active edge, удаляются в той же транзакции, что и удаление hop (по образцу каскада для удалённого inbound, §3.6 monitoring-panel.md). Telegram молчит — как и при удалении inbound.
- **Переименование** — новое имя звена = новый `path` (`edge:<new-name>` / `inner:<new-name>`); отдельного переезда строк нет. **Решено: строки под старым именем живут обычный ретеншн** (`monRetentionDays`/`monRollupRetentionDays`) и стареют сами — отдельного, более короткого параметра для «заведомо мёртвого» path не заводим: ещё одна настройка ради недели лишних строк не окупается, а данные под старым именем — это настоящая история этого же звена, которую полезно видеть на графике рядом с переименованием. Мгновенного каскада на переименование нет — оно того же рода событие, что появление нового звена.

### 6.2 Что меняется у mon-server (протокол v1, репо `3ax-ui-monitoring`)

> Ниже — дельта для стороны mon-server; сама она специфицируется в `3ax-ui-monitoring`, здесь фиксируется контракт с панелью, на который эта дельта опирается.

- На каждый `GET /state` с изменившейся ревизией (или изменившимся `chain.revision`) mon-server читает `chain.hops`; для каждого звена в состоянии `joined` или `legacy` — **и edge, и inner** — зовёт `GET /probe/configs?hop=<name>` (host = хост этого звена) — как сегодня зовёт `?host=<real>` для `direct`. `pending`-звенья пропускаются (не отвечают — `409 hop_not_joined`, join ещё не завершён).
- **Набор targets** = inbound'ы × (`{direct}` ∪ `{joined/legacy hops}`), т.е. на inbound теперь `2 + N` target'ов вместо двух (`direct`, `proxy`), где `N` — число звеньев цепочки (inner'ы плюс активный edge и запасные).
- **Распределение по mon-client**: без изменений — все mon-client пробуют все звенья и `direct`, как сегодня пробуют оба path; фильтра «этот mon-client только на это звено» в v1 нет (тот же принцип, что «все inbound'ы всем mon-client» в mon-protocol.md §4.2). `paths` каждого mon-client в admin UI mon-server по-прежнему список строк, просто теперь длиннее (`direct`, `inner:core-1`, `edge:ams-1`, `edge:ams-2`, …) — коробки во враждебных регионах владелец по-прежнему ограничивает нужным подмножеством. Inner'ы часто доступны не отовсюду, и это нормальный способ их не пробовать с чужих коробок.
- **`proxy` как отдельный target**: mon-server, знающий про цепочку (v2 протокола mon-server↔mon-client-конфига, не путать с контрактом панели — версия контракта не растёт), **не** держит `path=proxy` отдельным target'ом — он разворачивает его в `edge:<active>` и меняет target при каждом переключении active edge. Старый mon-server (не читающий `chain`) продолжает получать `proxy` через ручку без параметров и не узнает про остальные звенья — деградация до текущего поведения, не поломка.
- **mon-client**: изменений нет — mon-protocol.md §4.2 подтверждает, что `link`/`conf` в `targets` несут адрес готовым («mon-server прозрачен, знание протоколов живёт только в mon-client»), значит очередной элемент `targets` с `path="edge:ams-2"` для mon-client ничем не отличается от сегодняшнего `path="proxy"` — он просто probe-target с URL/conf. Смена набора targets проходит штатным путём смены config revision (mon-protocol.md §4.1).

### 6.3 Пробинг inner-звеньев: да

**Решение: inner'ы пробируются наравне с edge, уже в v1.** Путь — `inner:<name>`, конфиги — той же ручкой `GET /probe/configs?hop=<name>` (§6.1), targets строит тот же цикл mon-server (§6.2).

Почему это работает без отдельного механизма: у inner front действительно нет своих inbound'ов и своего probe account — но их ему и не нужно. Звено relay'ит **весь список relayed ports один-в-один** (§3.8), поэтому probe-конфиг обычного probe-аккаунта, отрендеренный с адресом inner'а, ходит по той же цепочке внутрь и проверяет отрезок `real ← …inner…`. Для mon-client это неотличимо от пробы edge: очередной target с URL/conf (mon-protocol.md §4.2).

Что это даёт: разница `inner:<name>` vs `edge:<name>` называет виновный сегмент прямо, без вычитания графиков и гадания. `direct` UP, `inner:core-1` UP, `edge:ams-1` DOWN — сломан внешний отрезок или сам edge; `inner:core-1` DOWN при `direct` UP — сломан inner, и чинить надо §4.6, а не edge. Wave и `last_seen_at` реестра остаются как были, но они отвечают на другой вопрос («доехал ли документ»), а не «ходит ли трафик».

Цена — плюс `M` targets на inbound, где `M` — число inner'ов (обычно один-два), и необходимость помнить, что inner доступен не из каждого региона: mon-client'ы во враждебных сетях ограничиваются подмножеством `paths` в admin UI mon-server (§6.2), как и раньше.

### 6.4 Панель: UI

#### Бейдж на звено в редакторе цепочки

`web/html/settings/panel/subscription/chain.html`: у **каждого** звена — и edge, и inner — бейдж состояния, худший `target` по всем inbound'ам с `path = edge:<name>` (для inner — `path = inner:<name>`; для active edge — c учётом алиаса `proxy`, т.е. свёртка по обоим `path`), та же свёртка и приоритет, что у колонки Health (`DOWN > FLAPPING > UNKNOWN > UP > PAUSED`; `STALE` панели перекрывает всё). Для `pending`-звена бейдж не про здоровье, а «нет данных» (серый, отдельная метка, не путать с `UNKNOWN` target'а) — mon-server туда ещё не ходил.

- Метод `MonitoringService.WorstLiveTargetState` обобщается до `WorstLiveTargetState(inboundKind, inboundId, pathPrefix string)` (пустой `pathPrefix` = как сегодня, все path); новый метод-обёртка `MonitoringService.WorstLiveHopState(hopName, role string) string` — сворачивает по всем inbound'ам сразу для одного звена (для бейджа в редакторе не нужна раскладка по inbound), выбирая префикс `edge:`/`inner:` по роли.
- Новая ручка `GET /panel/api/chain/hops/health` (сессия, конверт `{success,msg,obj}`) в контроллере реестра цепочки (`web/controller/chain_controller.go`, файл из рамки §11) — отдаёт `[{name, role, state}]` для всех звеньев реестра одним запросом на страницу редактора; переиспользует `WorstLiveHopState`, дополнительных таблиц не заводит.

#### Страница Monitoring

`web/html/monitoring.html`:

- Группировка/фильтр по `path` — вместо статичных `direct`/`proxy` строится динамически по `GET /panel/api/chain/hops/health` (список имён звеньев с ролями) + фильтр-чипы `direct`, `inner:<name>` на каждый inner, `edge:<name>` на каждое edge, `all`; активному edge — маркер (та же звёздочка/цвет, что у active edge в редакторе). Существующая ручка `GET targets` (§7.4 monitoring-panel.md) уже отдаёт `path` каждой строки — новых полей от неё не требуется, фильтрация клиентская, как сегодня фильтр `down` у Health.
- Строка-сводка над таблицами: «активный edge DOWN, запасное ams-2 UP» — считается на клиенте из уже загруженных `targets` + `chain` (активное имя и список звеньев панель уже знает из реестра — тот же вызов, что у редактора, `GET /panel/api/chain/hops/health`, дергается и здесь). Формула — как в §6.5 (свёртка «здоров ли edge»): для active edge берётся его состояние, для остальных — лучший из standby.
- Изменённые ручки: `GET targets` без изменений формы ответа (значения `path` — новые строки, схема та же). Новая: `GET /panel/api/chain/hops/health` (переиспользуется и редактором, и страницей Monitoring — одна ручка, два потребителя).

### 6.5 Telegram

Расширение алерта `target DOWN` (§6 monitoring-panel.md), когда `path` упавшего target'а — active edge (`path = edge:<activeEdge>` либо легаси `path = proxy`, если старый mon-server):

- К стандартному сообщению `⛔ DOWN · …` добавляется вторая строка: «Запасные: edge-b UP, edge-c UNKNOWN. Переключить: /proxy edge-b».
- **Список запасных** — все joined/legacy edge, кроме active, с их сегодняшним состоянием (`WorstLiveHopState`, свёртка по всем inbound'ам, тем же способом, что бейдж в редакторе).
- **Выбор кандидата на переключение** — по приоритету: все inbound'ы `UP` (полностью здоров) > большинство inbound'ов `UP` (частично) > `UNKNOWN`/нет данных; `DOWN` кандидатом не рассматривается. **Решено: «большинство» = строго больше 50 % inbound'ов в `UP`; ничья большинством не считается** — при 2 inbound'ах «1 из 2» в «частично здоровые» не попадает и падает в приоритет ниже, а при 3 из 5 — попадает. Порог зашит, отдельной настройки нет. При равенстве — по имени (`asc`). Если кандидатов нет вовсе (все standby `DOWN` или список standby пуст) — строка «нет здорового запасного» вместо «Переключить: …».
- Алерт `target DOWN` для **standby**-edge (`path = edge:<не-active>`) — обычный, без второй строки: это просто ещё один path в ленте, как direct сегодня.
- **Дедупликация** — одна подсказка на переход состояния: подсказка приклеена к самому событию `DOWN` активного edge (`notified=false` → шлём один раз), а не к периодическому опросу; повторный DOWN того же target'а (новый `id` события, новый `since`) — новая подсказка со свежим списком standby.
- `/proxy <name>` (бот, §2.5) остаётся единственным действием переключения; кнопка **«Сделать активным»** у edge в `chain.html` дергает ту же ручку реестра, что переключает active edge (registry write + `chainRevision++`) — отдельного API не заводим, бот и кнопка — два потребителя одного действия.

### 6.6 Standby edge — «тёплый»

Standby edge не простаивает — он уже принят в цепочку (`joined`), его relay поднят и реально пробрасывает трафик на next hop, просто host override на него не указывает и клиенты о нём не знают (в конфиги не попадает). Мониторинг пробует его наравне с active (§6.2) именно поэтому: бейдж standby в редакторе и Telegram-подсказка означают не «жив ли процесс», а **«сработает ли эта цепочка прямо сейчас, если переключить на неё host override»** — то есть весь путь standby → …inner… → real server живой и пропускает трафик по каждому inbound'у, а не просто «сервер отвечает на ping». `UP` у standby — гарантия, что `/proxy <name>` даст рабочий переход, а не подстава.

### 6.7 Секвенирование

Реализация — **после мержа эпика мониторинга #43** и тикетов реестра и волны этой карты (без реестра `chain.hops` неоткуда взять). Тикеты, которые встанут в очередь на этот раздел (заводятся отдельно, здесь только перечислены как зависящие):

1. Контракт: поле `chain` с полным списком звеньев в `GET /state`, режим `?hop=` (и синоним `?edge=`) у `GET /probe/configs`, каскад удаления/старение переименования — правки `monitoring_service.go`, `monitoring.go` (контроллер), `monitoring_ingest.go` не нужен (path не валидируется).
2. mon-server (репо `3ax-ui-monitoring`): чтение `chain.hops`, построение targets `edge:<name>` и `inner:<name>`, разворачивание `proxy` в `edge:<active>` для v2-конфигов mon-client.
3. Панель UI: бейджи в `chain.html` (все звенья), ручка `/panel/api/chain/hops/health`, группировка/сводка на `monitoring.html`.
4. Telegram: подсказка о запасных в алерте DOWN активного edge, кнопка «Сделать активным».

### Тронутые файлы

**Репо `SBKubric/3ax-ui-proxy` (панель):**
- `web/service/monitoring_service.go` — `State()` отдаёт `chain`, ревизия учитывает `chain.hops`/`activeEdge`; `ProbeConfigs(hop)`; обобщение `WorstLiveTargetState` + новый `WorstLiveHopState`; каскад удаления hop по `path`-префиксу.
- `web/controller/monitoring.go` — `GET /probe/configs` разбирает `?hop=` (и синоним `?edge=`), коды `409 unknown_hop`/`409 hop_not_joined`.
- `web/controller/chain_controller.go` — новая ручка `GET /panel/api/chain/hops/health` (файл и группа `/panel/api/chain/...` — §2.4, здесь только добавляется один хендлер).
- `web/service/chain_service.go` (или как назван сервис реестра эпика #43) — вызов каскадного удаления состояния мониторинга из обработчика удаления hop.
- `web/html/settings/panel/subscription/chain.html` — бейдж состояния на edge-звено.
- `web/html/monitoring.html` — динамические фильтр-чипы по edge, строка-сводка.
- `docs/spec/monitoring-contract.md`, `docs/spec/monitoring-panel.md` — правки текста контракта под §6.1–6.2 этого документа.
- `web/translation/translate.en_US.toml`, `translate.ru_RU.toml` (и остальные 11) — ключи для строки-сводки, подсказки Telegram, метки «нет данных» у pending-edge.

**Репо `SBKubric/3ax-ui-monitoring`:**
- `docs/spec/mon-protocol.md`, `docs/spec/mon-server.md` — построение targets по `chain.hops`, разворачивание `proxy` → `edge:<active>`.
- Код mon-server: цикл `GET /state` → чтение `chain` → пересборка targets (модуль, аналогичный сегодняшнему построению `direct`/`proxy`).
- admin UI mon-server: `paths` per-mon-client теперь список из `direct` + динамических `inner:<name>`/`edge:<name>` вместо статичной пары.

### Открытые вопросы владельцу

Открытых вопросов нет.

## 7. UI панели и бот

Прототип тикета [«Прототип UI: редактор цепочки в Settings → Proxy front, бейджи здоровья и вывод /proxy в боте» #74](https://github.com/SBKubric/3ax-ui-proxy/issues/74) карты [Proxy chain](https://github.com/SBKubric/3ax-ui-proxy/issues/68). Термины — по `CONTEXT.md` (chain, hop, next hop, edge front, inner front, active edge, standby edge, chain registry, join token). Опирается на решения рамки: **chain registry** — п. 1, `GetProxyOverride()` как производная от активного edge — п. 2, `/proxy` в боте — п. 10, файлы `web/html/settings/panel/subscription/chain.html` и `web/assets/js/model/chain.js` — п. 11.

Прототип-стаб реализован веткой `prototype/proxy-chain-ui` (коммит `2520ef73`) в форке `SBKubric/3ax-ui-proxy`: статичные mock-данные во Vue, без единого сетевого вызова к бэкенду. Секция §7 фиксирует, куда должен переехать реальный код, когда появится реестр (`database/model/chain.go`) и контроллер `/panel/api/chain/*`.

### 7.1 Где живёт редактор

Вариант **B отклонён** (отдельная вкладка Settings рядом с *Subscription*): цепочка — это то же самое host override, только производное от реестра, а не от двух полей формы; отдельная вкладка развела бы «что клиент видит» (override) и «чем это управляется» (цепочка) по разным местам.

**Выбран вариант A — редактор заменяет поля `proxyOverrideEnable`/`proxyOverrideHost`** в `web/html/settings/panel/subscription/general.html` (секция после `pages.settings.proxyOverride"`), а на их месте остаётся одна read-only строка:

- заголовок — прежний `pages.settings.proxyOverride` ("Proxy front (anti-block)"),
- описание — новый `pages.settings.chain.overrideNote` ("Host override is now the active edge → chain editor"),
- control — тег с именем текущего active edge (`chainActiveEdgeName`, вычисляется из `GetProxyOverride()` на реальном бэкенде).

Сам редактор — новая секция `web/html/settings/panel/subscription/chain.html`, вставленная сразу после `{{ template "settings/panel/subscription/general" . }}` внутри той же вкладки Settings → Subscription (`web/html/settings.html`, `a-tab-pane key="4"`). Одна точка вставки, аддитивно (ADR 0002).

Кнопка «Show manifest» (`pages.settings.relayManifestShow`) в `general.html` заменяется на «Показать порты цепочки» в редакторе — тот же список, что `x-ui chain ports` (§5.9).

### 7.2 Разметка редактора и `data-testid`

Ant Design Vue, `<a-collapse>` с одной панелью `pages.settings.chain.title`. Порядок сверху вниз:

```
┌ Proxy chain ──────────────────────────────────────────────┐
│ real-server                                    [Real server]│  chain-real-server
├ Inner hops ─────────────────────────────────────────────────┤
│ core-1  [inner] [joined] UP                      [Delete]   │  chain-hop-core-1
├ Edges (standby fan out under the last inner hop) ────────────┤
│   edge-a  [edge] [joined] UP      [active]                  │  chain-hop-edge-a
│   edge-b  [edge] [joined] DOWN            [Make active][Del]│  chain-hop-edge-b
│   edge-c  [edge] [pending] UNKNOWN                           │  chain-hop-edge-c
│     jt_8f2c••••••••3a91 [Copy]  expires 2026-09-21 12:00 UTC │  ...-token / ...-token-copy
│                                          [Reissue][Delete]   │
│   edge-legacy [edge] [legacy] UNKNOWN            [Delete]    │  chain-hop-edge-legacy
├───────────────────────────────────────────────────────────── │
│ [+ Add hop]                                                  │  chain-add-hop
├───────────────────────────────────────────────────────────── │
│ Host override is now the active edge → chain editor below.   │  chain-override-note
└───────────────────────────────────────────────────────────────┘
```

Модалка «Add hop» (`chain-add-hop-modal`): поля Name (`chain-add-hop-name`), Host (`chain-add-hop-host`), Role — select inner/edge (`chain-add-hop-role`), Next hop — select real-server или любой inner (`chain-add-hop-next`), **Position** — select места вставки для inner-звена (`chain-add-hop-position`).

**Решено: селектор позиции нужен уже в MVP.** Для `role=inner` модалка показывает select «вставить после …» со значениями `real server` (вставка в начало, `position 0`) и именем каждого существующего inner; дефолт — последний inner, то есть сегодняшнее «добавить в конец». Выбранное значение уезжает в `POST /add` как `position` (§2.4), а панель делает перецепку соседей в одной транзакции (§2.6.3). Без селектора вставка между двумя inner'ами требовала бы либо второго действия «переместить», либо ручного редактирования `position` — а сам сценарий (§2.6.3) спека уже считает штатным и безопасным. Для `role=edge` select скрыт: все edge подвешены к последнему inner'у (инвариант 2 в §2.7).

Звено, которое уходит, остаётся в списке со `[draining]` вместо `[joined]`, без кнопок (`update`, `Make active`, `Reissue`, `Delete` по нему отказывают, §4.5.5) и с подсказкой `drainingHint`; строка исчезает сама, когда панель завершит уход. Удаление — `a-modal-confirm`; после подтверждения диалог показывает ответ `del`: либо «удалено», либо «уходит, ждём: <имена>, до <время>». Попытка удалить active edge вместо диалога подтверждения показывает предупреждение `pages.settings.chain.deleteActiveRefused` («Choose another edge with "Make active" first, then delete this one.») и не удаляет звено.

Кнопка «Make active» показывает в диалоге подтверждения **одну строку предупреждения** `pages.settings.chain.switchNote`: «Links already handed out keep going through the current edge until clients refresh the subscription.» / «Уже выданные ссылки продолжат ходить через прежнее edge, пока клиент не перечитает подписку.» Это не отказ и не второй шаг — просто напоминание о свойстве переключения (§4.7); бот печатает ту же строку после `/proxy <имя>` (§2.5).

#### Таблица `data-testid` (для Playwright, `e2e/`)

| testid | элемент |
|---|---|
| `chain-editor` | корневой `a-collapse` |
| `chain-real-server` | строка real server |
| `chain-inner-list` | список inner-звеньев |
| `chain-edge-list` | список edge-звеньев |
| `chain-hop-<name>` | строка конкретного звена (inner или edge) |
| `chain-hop-<name>-health` | бейдж здоровья (`mon-state-*`, см. §7.4) |
| `chain-hop-<name>-make-active` | кнопка «Make active» (только joined, не active) |
| `chain-hop-<name>-active-marker` | тег «active» |
| `chain-hop-<name>-delete` | кнопка удаления |
| `chain-hop-<name>-token` | блок join-токена (только pending) |
| `chain-hop-<name>-token-copy` | кнопка копирования токена |
| `chain-hop-<name>-reissue` | кнопка «Reissue» |
| `chain-add-hop` | кнопка открытия модалки добавления |
| `chain-hop-<name>-freshness` | вторичный маркер свежести (`lastRevision` против `chainRevision`) |
| `chain-hop-<name>-draining` | бейдж «draining» с подсказкой `drainingHint` и списком ещё не подтвердивших соседей (§4.5) |
| `chain-add-hop-modal` / `-name` / `-host` / `-role` / `-next` / `-position` | модалка и её поля (`-position` — только для `role=inner`) |
| `chain-override-readonly` | read-only строка в `general.html` |
| `chain-override-note` | текст-подсказка внизу редактора |

### 7.3 UI API (реализуется вместе с реестром, §2.4)

Контроллер `web/controller/chain_controller.go`, группа `/panel/api/chain/` (сессия, конверт `{success,msg,obj}`), маршруты из §2.4: `GET list`, `POST add` (ответ несёт join-токен один раз), `POST update/:id`, `POST del/:id` (активный edge — отказ `active_edge_in_use`, клиент показывает `chain.deleteActiveRefused`), `POST setActive/:id` (только `joined`/`legacy` edge), `POST reissueToken/:id`, `GET hops/health` (§6.4). Не путать с `/chain/v1/*` на sub-сервере (§3.3, §4.3), которыми пользуются сами звенья.

### 7.4 Бейджи здоровья

Переиспользуют классы страницы мониторинга (`web/html/monitoring.html`, `mon-state mon-state-<state>`, стили в `custom.min.css`): `mon-state-up` (зелёный), `mon-state-down` (красный), `mon-state-unknown`/`mon-state-none` (серый). Источник значения — ручка `GET /panel/api/chain/hops/health` (§6.4), которая сворачивает данные мониторинга по звену.

**Бейдж есть и у inner-звеньев**: мониторинг пробирует их наравне с edge (§6.3), путь — `inner:<name>`, свёртка и классы те же. Отдельного «MVP без здоровья inner» больше нет.

**Свежесть — вторичный маркер, а не бейдж.** Рядом с бейджем здоровья у каждого `joined`-звена показывается маркер свежести из реестра: `lastRevision` звена против текущего `chainRevision` — «rev 42 (актуальна)» или «rev 39 из 42» (отстаёт) плюс подсказка с `lastSeenAt`. Это разные вопросы: бейдж отвечает «ходит ли через это звено трафик» (мониторинг), маркер — «доехала ли до него конфигурация» (волна, §3.3). Отстающая ревизия при `UP` — нормальное переходное состояние в пределах `chainPollSeconds`; отстающая ревизия часами при `UP` означает, что звено relay'ит по старому документу и не видит своего next hop (§3.6), а `DOWN` при свежей ревизии — что документ доезжает, но трафик не ходит. Маркер серый и мелкий, `data-testid` — `chain-hop-<name>-freshness`.

### 7.5 Локализация

Ключи добавлены **в конец** секций `[pages.settings]` и `[tgbot.commands]` в `translate.en_US.toml` и `translate.ru_RU.toml` (порядок ключей ниже — как в файле).

#### `pages.settings.chain.*`

| ключ | en_US | ru_RU |
|---|---|---|
| `title` | Proxy chain | Цепочка прокси |
| `desc` | Prototype #74: edit the chain… | Прототип #74: редактор цепочки… |
| `realServer` | Real server | Real server |
| `innerHops` | Inner hops | Внутренние звенья |
| `edges` | Edges (standby fan out under the last inner hop) | Edge (запасные веерятся под последним внутренним) |
| `roleInner` / `roleEdge` | inner / edge | inner / edge |
| `state.pending` / `state.joined` / `state.legacy` / `state.draining` | pending / joined / legacy / draining | pending / joined / legacy / уходит |
| `drainingHint` | Deleted; still serving its former neighbours until they re-point (until {{ .Until }}). Power the box off after that. | Удалено; обслуживает бывших соседей, пока они не перецепятся (до {{ .Until }}). После этого бокс можно гасить. |
| `active` | active | активное |
| `makeActive` | Make active | Сделать активным |
| `delete` | Delete | Удалить |
| `deleteConfirmTitle` | Delete this hop? | Удалить это звено? |
| `deleteActiveRefusedTitle` | Can't delete the active edge | Нельзя удалить активное edge |
| `deleteActiveRefused` | Choose another edge with "Make active" first, then delete this one. | Сначала выберите другое edge через «Сделать активным», затем удалите это. |
| `reissue` | Reissue | Переиздать |
| `tokenExpires` | expires | истекает |
| `addHop` / `addHopTitle` | Add hop | Добавить звено |
| `hopName` / `hopHost` / `hopRole` / `hopNextHop` | Name / Host / Role / Next hop | Имя / Хост / Роль / Следующее звено |
| `hopPosition` | Insert after | Вставить после |
| `switchNote` | Links already handed out keep going through the current edge until clients refresh the subscription. | Уже выданные ссылки продолжат ходить через прежнее edge, пока клиент не перечитает подписку. |
| `freshness` | rev {{ .Rev }} of {{ .Head }} | ревизия {{ .Rev }} из {{ .Head }} |
| `legacyReinstall` | Re-install this box as a chain hop. | Переустановите бокс как звено. |
| `overrideNote` | Host override is now the active edge → chain editor below. | Подмена хоста теперь берётся из активного edge → редактор цепочки ниже. |

#### `tgbot.commands.chain*`

| ключ | en_US | ru_RU |
|---|---|---|
| `chainHeader` | 🔗 **Proxy chain** (revision N) | 🔗 **Цепочка прокси** (ревизия N) |
| `chainHopLine` | marker `name` · role · state · health | тот же шаблон, значения не переводятся |
| `chainActiveMarker` / `chainInactiveMarker` | ★ / · | ★ / · |
| `chainUsage` | Switch the active edge: `/proxy <name>` | Переключить активное edge: `/proxy <имя>` |
| `chainSwitched` | ✅ Active edge is now `name`. | ✅ Активное edge теперь `name`. |
| `chainUnknown` | ❗ No hop named `name`. Showing the chain instead. | ❗ Нет звена с именем `name`. Показываю цепочку. |
| `chainSwitchNote` | Links already handed out keep using the previous edge until clients refresh the subscription. | Уже выданные ссылки продолжат ходить через прежнее edge, пока клиент не перечитает подписку. |

Существующие `tgbot.commands.proxyStatus`/`proxyEnabled`/`proxyDisabled`/`proxyUsage` (одиночный override) в этом прототипе ещё используются как текст для мок-вывода, а тикетом бота (§2.5) **удаляются** — вместе с переездом `/proxy` на цепочку они становятся недостижимы, алиасов и «переходного периода» для них нет. `tgbot.commands.proxyDesc` не удаляется, а переписывается под цепочку.

### 7.6 Бот: `/proxy` и `/proxy <name>`

По §2.5: `/proxy` без аргумента — список звеньев (роль, состояние, здоровье, маркер active); `/proxy <name>` — переключение active edge; неизвестное имя — тот же список с префиксом-предупреждением. Формат сообщения — HTML (`ParseMode: HTML`, как остальной `tgbot.go`), `stateLabel`/`healthLabel` берутся из значений реестра/мониторинга напрямую (не локализуются — технические токены, как `path` на странице мониторинга).

#### `/proxy` (en)

```html
🔗 <b>Proxy chain</b> (revision 42)

· <code>core-1</code> · inner · joined · UNKNOWN
★ <code>edge-a</code> · edge · joined · UP
· <code>edge-b</code> · edge · joined · DOWN
· <code>edge-c</code> · edge · pending · UNKNOWN
· <code>edge-legacy</code> · edge · legacy · UNKNOWN

Switch the active edge: <code>/proxy &lt;name&gt;</code>
```

#### `/proxy` (ru)

```html
🔗 <b>Цепочка прокси</b> (ревизия 42)

· <code>core-1</code> · inner · joined · UNKNOWN
★ <code>edge-a</code> · edge · joined · UP
· <code>edge-b</code> · edge · joined · DOWN
· <code>edge-c</code> · edge · pending · UNKNOWN
· <code>edge-legacy</code> · edge · legacy · UNKNOWN

Переключить активное edge: <code>/proxy &lt;имя&gt;</code>
```

#### `/proxy edge-b` (en) — переключение на joined, но не active edge

```html
✅ Active edge is now <code>edge-b</code>.
Links already handed out keep using the previous edge until clients refresh the subscription.
```

#### `/proxy edge-b` (ru)

```html
✅ Активное edge теперь <code>edge-b</code>.
Уже выданные ссылки продолжат ходить через прежнее edge, пока клиент не перечитает подписку.
```

#### `/proxy nope` (en/ru) — неизвестное имя → тот же список, с предупреждением сверху

```html
❗ No hop named <code>nope</code>. Showing the chain instead.
<!-- далее — тот же вывод, что у "/proxy" (en) выше -->
```

```html
❗ Нет звена с именем <code>nope</code>. Показываю цепочку.
<!-- далее — тот же вывод, что у "/proxy" (ru) выше -->
```

Реализация (тикет реестра, не этот прототип): `case "proxy"` в `web/service/tgbot.go` (сейчас — строка 733) читает реестр вместо `proxyOverrideEnable/Host`; аргумент без совпадения по `name` — та же ветка листинга с `tgbot.commands.chainUnknown` перед `chainHeader`; попытка `/proxy <pending-or-legacy-name>` (не `joined`) — по аналогии с HTTP `409` возвращает ошибку вместо переключения (сообщение не мокировано в этом тикете, решить в реестровом).

### Тронутые файлы

- `web/html/settings/panel/subscription/chain.html` — новый, редактор цепочки (стаб).
- `web/html/settings/panel/subscription/general.html` — секция `proxyOverride` заменена на read-only строку.
- `web/html/settings.html` — подключение `chain.html` во вкладку Subscription; mock-данные (`chain`, `chainAddModal`), геттеры (`innerHops`, `edgeHops`, `chainActiveEdgeName`) и методы (`makeActive`, `confirmDeleteHop`, `reissueToken`, `copyToken`, `openAddHopModal`, `submitAddHop`, `stateColor`, `stateLabel`, `healthClass`) добавлены в существующий Vue-инстанс.
- `web/translation/translate.en_US.toml`, `web/translation/translate.ru_RU.toml` — ключи `pages.settings.chain.*` (конец `[pages.settings]`) и `tgbot.commands.chain*` (конец `[tgbot.commands]`).
- (не в этом прототипе, для реестрового тикета) `web/controller/chain_controller.go`, `web/assets/js/model/chain.js`, `web/service/tgbot.go` (`case "proxy"`), `e2e/chain-ui.spec.ts`.

Ветка: `prototype/proxy-chain-ui`, коммит `2520ef73` (не запушен), репозиторий `SBKubric/3ax-ui-proxy`.

### Открытые вопросы владельцу

Открытых вопросов нет.

## 8. План реализации

Каждый пункт — отдельный тикет карты (графуируют из fog после мержа этой спеки), ветка `<номер>-<название>`, свой PR в `main`, тесты по `docs/agents/testing.md` (`docker run --rm -v $PWD:/src -w /src golang:1.26 go test -race -count=1 ./...` + `make e2e`). Порядок выбран так, чтобы панель и бокс можно было релизить независимо, а стенд проверять по шагам.

| # | тикет | что входит | зависит от |
|---|---|---|---|
| 1 | Реестр: модель и сервис | `database/model/chain.go` (константы для `role`/`state`, не голые строки; колонки `drain_*`), `web/service/chain_service.go` (инварианты §2.7, ревизия, миграция legacy override), `web/service/chain_drain.go` (`draining`, §4.5), `setting_chain.go`, производный `GetProxyOverride()` (§2.3); unit-тесты на четыре сценария §2.6 и на уход звена | — |
| 2 | Порты и документ | `chainports/` (переименование `relaymanifest`, §5.9), `web/service/chain_document.go` (сборка, усечение §3.2, ETag), хуки `BumpRevision()` в inbound/tunnel/mtproto; `x-ui chain ports` (без алиаса, §5.8) | 1 |
| 3 | Sub-сервер панели: `/chain/v1/*` | `sub/chainController.go` — `document` (§3.3), `join` (§4.3), `status`; `web/service/chain_join.go` | 2 |
| 4 | UI реестра и API | `web/controller/chain_controller.go` (§2.4), `chain.html` по прототипу `prototype/proxy-chain-ui` (§7), `chain.js`, read-only блок в `general.html`, локализация; e2e `e2e/tests/chain-editor.spec.ts` | 1, 3 |
| 5 | Бот `/proxy` | тело `case "proxy"` (§2.5, §7.6), ключи `tgbot.commands.chain*` | 1 |
| 6 | Бокс: `proxy.json` v2, волна, join page | `proxy/config.go` v2 + legacy-WARN (§5.3), `proxy/chain.go` (клиент волны, применение §3.5, `stateDir`), `proxy/join.go` + `joinpage.html` (§5.4), `BuildRelayConfig` от `ports[]`, `/chain/v1/*` на sub-порту; CLI `chain status`/`chain rejoin`/`chain join-url` (§5.8) | 2 |
| 7 | Установка и обновление | `install.sh` (§5.6), `update.sh` (§5.7), футер; runbook `docs/runbooks/proxy-front.md` (вход звена, переустановка §5.10, смерть inner §4.6, удаление звена §4.5); README EN/RU | 6 |
| 8 | Релиз и стенд | релиз-тег панели и бокса; e2e на стенде `real ← bridge (inner) ← proxy (edge)` по §10; закрытие [#76](https://github.com/SBKubric/3ax-ui-proxy/issues/76) — предпосылка | 4, 5, 7 |
| 9 | Мониторинг через звенья | контракт (§6.1), панельные ручки и бейджи (§6.4), Telegram-подсказка (§6.5); отдельно — mon-server в `3ax-ui-monitoring` (§6.2) | 8 и мерж эпика [#43](https://github.com/SBKubric/3ax-ui-proxy/issues/43) |

Тикеты 1–5 (панель) и 6–7 (бокс) могут идти параллельно после тикета 2: контракт между ними — документ §3.1 и ручки §3.3/§4.3.

## 9. Ответы на вопросы карты (Not yet specified)

- **Компрометация edge.** Изъятый бокс знает свой hop secret, адрес и sub-порт next hop, список relayed ports и своё имя (§3.7). Ротация: удалить звено из реестра (§4.5) — у edge снаружи никого нет, поэтому `draining` его не касается, строка и секрет умирают сразу, и секрет мёртв на следующем же документе соседа; при необходимости перевыпустить токен и переустановить бокс на новом хосте. Общего секрета, который нужно было бы ротировать по всей цепочке, нет.
- **«Тёплые» запасные edge.** Да: standby edge — полноправное `joined`-звено, relay'ит и качает подписки, только host override на него не указывает; мониторинг пробирует его наравне с активным, бейдж значит «переключение на него сработает прямо сейчас» (§6.6).
- **Видно ли, где именно рвётся цепочка.** Да: мониторинг пробирует и inner-звенья (`path = inner:<name>`, §6.3) наравне с edge, поэтому пара `inner:<name>` UP + `edge:<name>` DOWN называет сломанный сегмент без вычитания графиков.
- **Уход звена с внешними соседями.** Звено не удаляется мгновенно, а переходит в `draining` (§4.5): реестр перецепляет соседей сразу, но уходящее продолжает отдавать им документ, подставляя в их `nextHop` свой собственный next hop. Строка и секрет умирают, когда все бывшие соседи подтвердили ревизию удаления или истекли `chainDrainMinutes`. Без этого сосед, у которого путь к панели шёл через удалённое звено, не узнавал бы о перецепке никогда (стенд [#86](https://github.com/SBKubric/3ax-ui-proxy/issues/86), [#97](https://github.com/SBKubric/3ax-ui-proxy/issues/97)).
- **Next hop недоступен дольше порога.** Звено хранит последний документ и продолжает relay'ить, ретраит с замедлением, после `chainStaleMinutes` помечает себя `stale` в `/chain/v1/status` и логе; relay не гасится и в bootstrap звено само не уходит (§3.6). Владельца зовёт мониторинг: `direct` UP, `edge:<name>` DOWN.

## 10. E2E на стенде `real ← bridge (inner) ← proxy (edge)`

Предпосылка: [#76](https://github.com/SBKubric/3ax-ui-proxy/issues/76) — третий VPS. Стенд: `real` (панель), `proxy` (нынешний front; host override уже указывает на него) и **новый** VPS `bridge`.

Топология стенда — `real ← bridge (inner) ← proxy (edge)`:

- **`bridge`** — новая машина, ssh-хост `bridge`, `194.87.80.122`, Debian 13, 1 vCPU, 380 МБ RAM. Ставится **с нуля** как inner front, next hop = `real`. До установки: на ней сейчас слушает `socat` на 443 — его надо **остановить и удалить** (`systemctl disable --now` юнита или `kill` + `apt purge socat`), иначе relay не сможет занять 443. Память маленькая, поэтому проверяем заодно, что xray-relay в 380 МБ укладывается (`systemd-cgtop`, `journalctl -u x-ui` без OOM).
- **`proxy`** — остаётся **edge**: тот же адрес, что и сегодня в host override, поэтому клиентам ничего менять не нужно. Переустанавливается как звено и после входа становится активным edge.

Порядок — снизу вверх, изнутри наружу: сначала панель, потом inner, потом edge.

1. **Панель.** Обновить панель на `real` до релиза. В реестре появилось `legacy`-звено `edge` с хостом = адрес `proxy` — host override работает как раньше (§2.3), клиенты ходят через `proxy` мимо цепочки. Проверить, что Settings → Subscription → *Proxy front* стал read-only и указывает на редактор цепочки.
2. **Inner `bridge` с нуля.** На `bridge`: остановить и удалить `socat`, проверить, что 443 свободен (`ss -ltnup | grep :443` пуст). В панели завести звено `name=bridge`, `role=inner`, `host=194.87.80.122`, next hop = панель (`real`) — получить join-токен. Поставить бокс:
   ```bash
   XUI_PROXY_MODE=1 \
   PROXY_NEXT_HOP=<ip real> PROXY_NEXT_HOP_SUB_PORT=2096 \
   PROXY_JOIN_TOKEN=<токен> PROXY_SUB_PORT=2096 \
   bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)
   ```
   Проверить: `x-ui chain status` → `joined`, ревизия совпадает с панелью, `stale: false`; в реестре свежий `last_seen_at`; установщик выпустил IP-сертификат Let's Encrypt (`PROXY_TLS=letsencrypt-ip`, §5.6) и sub-порт отвечает по HTTPS.
3. **Edge `proxy` через join page.** В панели завести звено `name=proxy`, `role=edge`, `host` = адрес `proxy`; next hop панель вычислила сама = `bridge`. Переустановить бокс `proxy` по §5.10 **без** `PROXY_JOIN_TOKEN` — установщик печатает одноразовую ссылку join page (`x-ui chain join-url`), токен из реестра вводится в неё: это проверка второго пути входа. После успеха — сделать звено активным (`/proxy proxy` в боте или кнопка в редакторе) и **удалить `legacy`-звено**. С этого момента host override производный от реестра и указывает на тот же адрес, что и раньше.
4. **Клиент.** Подписка `/sub/<id>` отдаёт адрес `proxy`; VLESS ходит по пути `proxy → bridge → real`, AWG UDP 51820 ходит, `Profile-Web-Page-Url` указывает на `proxy`.
5. **Состав портов.** На панели добавить inbound на новом порту → за ≤ 2 × `chainPollSeconds` порт открыт и на `bridge`, и на `proxy` (`ss -ltnup`); выключить/удалить inbound — порт закрылся на обоих.
6. **Standby.** Второй edge на том же VPS завести нельзя (один хост — одно звено), поэтому запасное проверяется в состоянии `pending`: токен выдан, в списке `/proxy` звено видно как `pending`, `setActive` на него отказывает, `GET /probe/configs?hop=<имя>` отвечает `409 hop_not_joined`.
7. **Выключение override.** `/proxy off` → override выключен, подписки отдают адрес `real`; `/proxy proxy` → обратно, с предупреждением про уже выданные ссылки (§2.5).
8. **Удаление inner (draining, §4.5).** Удалить `bridge` из реестра (`del`) → ответ `{"state":"draining","drainUntil":…,"safeToPowerOffWhen":{"hops":["proxy"],"revision":R}}`, в редакторе цепочки у `bridge` бейдж «draining». На `bridge`: `x-ui chain status` → `draining: true`, relay жив. За ≤ `chainPollSeconds` `bridge` отдаёт `proxy` документ, где `nextHop.host` = адрес `real`; ещё за ≤ `chainPollSeconds` `proxy` перецепился напрямую на `real` (`x-ui chain status` → `nextHop.host` = адрес `real`), трафик ходит. `chain rejoin` не нужен — перецепку делает волна. Затем панель видит `last_revision(proxy) ≥ R` и **сама** удаляет строку `bridge`; проверить, что после этого опрос `bridge` получает `404` и он помечает себя `stale`, а relay всё ещё жив (панель боксы не гасит). Отдельно проверить таймаут: повторить, погасив `proxy` до перецепки, — строка `bridge` исчезает через `chainDrainMinutes` с записью в лог. Вернуть `bridge` вставкой (§2.6.3) с новым join-токеном.
9. **Смерть inner.** Погасить `bridge` целиком → `proxy` через `chainStaleMinutes` помечает себя `stale` и продолжает relay'ить на мёртвый адрес; мониторинг показывает `direct` UP, `inner:bridge` DOWN, `edge:proxy` DOWN (§6.3). Починка по §4.6: `del` мёртвого звена с `{"skipDrain": true}` + `reissueToken` для `proxy` + `x-ui chain rejoin --next-hop <ip real> --token <новый>` на боксе `proxy`.
10. **Гигиена и обновление.** `grep -R "privateKey\|password" /etc/x-ui /usr/local/x-ui/bin` на обоих боксах пуст; `update.sh` на боксе с v2-конфигом проходит штатно, на сохранённом v1-конфиге (`/root/proxy.json.v1`, подложенном на место) — останавливается **до** подмены бинаря (§5.7).
