# 3AX-UI proxy front

Панель управления Xray (форк 3x-ui) с режимом «прокси-фронт»: одноразовые серверы, которые принимают клиентский трафик и пробрасывают его по цепочке на скрытый реальный сервер.

## Language

**Real server**:
Сервер с панелью, inbound'ами, клиентами и историей трафика; его адрес никогда не попадает в клиентские конфиги.
_Avoid_: upstream, hidden server, panel box

**Proxy front**:
Одноразовый сервер-звено, который принимает трафик от клиентов или от внешнего звена и relay'ит его на next hop; при блокировке выбрасывается и заменяется.
_Avoid_: proxy box, relay server, relay панель, front

**Relay**:
Процесс на proxy front (xray dokodemo-door), который на уровне L4 пробрасывает relayed ports на next hop без терминации TLS.
_Avoid_: forwarder, tunnel

**Relayed port**:
Публичный порт real server, который relay открывает у себя и пробрасывает один-в-один. Складывается из xray inbound ports и extra ports.

**Xray inbound port**:
Relayed port, который панель берёт из своего xray-конфига по правилам пропуска и кладёт в документ цепочки.

**Relay manifest**:
Историческое: санированная выписка из xray-конфига real server, которую владелец вставлял в setup page (ADR 0001). В цепочке заменена документом цепочки; правила отбора портов живут в пакете `chainports`.
_Avoid_: panel config, exported config.json, panel-xray.json

**Join page** (страница входа):
Одноразовая страница на proxy front по секретной ссылке, в которую владелец вводит адрес next hop и join token; исчезает, как только вход принят. Преемница setup page из ADR 0001.
_Avoid_: setup page, bootstrap page, onboarding, wizard

**Extra port**:
Relayed port, который real server обслуживает вне xray (AmneziaWG, WireGuard, MTProto) и который поэтому нельзя узнать из xray-конфига.
_Avoid_: host port, additional port

**Host override**:
Глобальная настройка панели, подменяющая адрес real server на адрес active edge во всех выдаваемых конфигах и ссылках подписки.
_Avoid_: proxy override, address substitution

**Tunnel subscription**:
Публичный маршрут подписки, отдающий по subId клиентские конфиги AmneziaWG и WireGuard той же подписки; дополняет xray-подписку, не меняя её.
_Avoid_: AWG subscription, conf feed, tunnel feed

## Chain

**Chain** (цепочка):
Связный список proxy front'ов между клиентами и real server: каждое звено relay'ит на следующее, подписки идут той же дорогой. На панель — одна цепочка.
_Avoid_: multi-hop, relay chain, route

**Hop** (звено):
Один proxy front в составе цепочки.
_Avoid_: node, link, box

**Next hop** (следующее звено):
Тот, на кого звено relay'ит трафик и у кого берёт подписки и документ цепочки: inner front или real server.
_Avoid_: upstream, target host

**Edge front** (внешнее звено):
Звено, которое видят клиенты; кандидат на host override.
_Avoid_: entry node, public front, exit

**Inner front** (внутреннее звено):
Звено, которое знают только соседние звенья; в клиентские конфиги не попадает.
_Avoid_: middle hop, intermediate, transit

**Active edge** (активное edge):
Edge front, на который сейчас указывает host override.
_Avoid_: current front, primary

**Standby edge** (запасное edge):
Edge front, уже вошедший в цепочку, но не активный; ждёт переключения host override.
_Avoid_: spare, backup, reserve

**Chain registry** (реестр цепочки):
Список звеньев на панели с ролями, порядком, next hop и active edge; описывает цепочку, но не управляет боксами напрямую.
_Avoid_: topology, node list, inventory

**Chain document** (документ цепочки):
Версионированная выписка из реестра, которую звенья передают наружу с усечением: звено видит себя, всё снаружи от себя и relayed ports, но ничего глубже.
_Avoid_: chain manifest, config bundle

**Wave** (волна):
Прокатывание новой ревизии документа цепочки изнутри наружу: каждое звено спрашивает свой next hop и применяет свою часть.
_Avoid_: push, sync, broadcast, propagation

**Join token** (join-токен):
Одноразовый секрет, выданный панелью новому звену; по нему next hop узнаёт звено и впускает его в цепочку.
_Avoid_: enrollment key, pairing code, invite

**Hop secret** (секрет звена):
Долгоживущий секрет, который панель выдаёт звену при входе; next hop сверяет его по хэшу из документа цепочки и только по нему отдаёт волну. У каждого звена свой; изъятое edge раскрывает только свой.
_Avoid_: chain secret, shared secret, API key, chain token

## Monitoring

**mon-server**:
Единственный внешний сервис мониторинга на отдельном сервере: ведёт реестр mon-clients, получает у real server конфиги probe accounts и текущий host override, раздаёт mon-clients их targets, считает состояние каждого target и сообщает real server переходы и статистику.
_Avoid_: monitoring hub, collector, watchdog server

**mon-client**:
Коробка в целевом регионе, которой mon-server назначает набор targets; для каждого поднимает туннель (xray-core, awg) и раз в минуту шлёт mon-server tunnel probe через туннель и heartbeat мимо него.
_Avoid_: agent, probe node, sensor

**Target**:
Пара «inbound real server × path», которую проверяет один mon-client через probe account этого inbound'а. Единица состояния UP/DOWN и статистики.
_Avoid_: check, monitor, endpoint

**Path**:
Через какой адрес target достигает real server: `direct` (настоящий адрес real server), `edge:<name>` или `inner:<name>` (конкретное звено цепочки по имени), а пока у цепочки нет пробируемых звеньев — `proxy` (адрес из host override). Других значений нет.
_Avoid_: mode, route

**Probe account**:
Служебный клиент с именем `probe-…`, который панель заводит по запросу mon-server; все probe accounts панели живут под общим subId. В клиентском xray-inbound'е — один на inbound, общий для всех mon-clients и path. В туннельном сервере (AWG) — отдельный пир на каждую пару «mon-client × path», потому что у пира один endpoint и одна сессия и общий пир делят конкурирующие пробы. Отличается от пользовательских префиксом имени, не считается пользователем и чистится панелью по таймауту, когда mon-server перестаёт его подтверждать.
_Avoid_: monitoring client, service user, test client

**Tunnel probe**:
Ежеминутный запрос mon-client к mon-server, отправленный внутрь туннеля target'а; его успех означает, что inbound работает для реальных клиентов по этому path.
_Avoid_: ping, healthcheck

**Heartbeat**:
Ежеминутный запрос mon-client к mon-server мимо туннеля; несёт результаты tunnel probes и диагностику, а в ответ получает номер актуальной ревизии конфига. Отсутствие heartbeat означает, что мёртв сам mon-client, а не туннель.
_Avoid_: keepalive, ping

**Stale**:
Состояние target в панели, когда mon-server не присылал статистику дольше порога; отличается от DOWN тем, что молчит мониторинг, а не inbound.
_Avoid_: unknown, expired

**Registration request**:
Заявка mon-client на вход в реестр mon-server: подаётся без секрета, живёт пять минут в состоянии pending и превращается в запись реестра только после одобрения администратором в админке mon-server.
_Avoid_: enrollment, join request, handshake

**Pairing code**:
Короткий код, который mon-client печатает в свой лог и прикладывает к registration request; администратор сверяет его в админке, чтобы одобрить именно свою коробку.
_Avoid_: PIN, OTP, verification token

**Client token**:
Постоянный секрет mon-client, выданный mon-server при одобрении registration request; им подписаны heartbeat, tunnel probe и запрос конфига. Отзыв токена выкидывает mon-client из реестра до новой заявки.
_Avoid_: API key, bearer, credential

**Config revision**:
Хэш конфига, который mon-server собрал для конкретного mon-client (его targets, paths и параметры проб); возвращается в ответе на heartbeat, и его смена — единственный сигнал mon-client перечитать конфиг.
_Avoid_: version, generation, panel revision

**Unverified cycle**:
Цикл проб mon-client, за который heartbeat так и не был подтверждён mon-server; его провалы не считаются, потому что адресат tunnel probe — сам mon-server, и его недоступность нельзя отличить от падения туннеля.
_Avoid_: offline cycle, buffered cycle

**Admin UI**:
Веб-интерфейс mon-server за логином и паролем: одобрение registration requests, реестр mon-clients и все настройки mon-server; состояние targets он не показывает — это страница Monitoring панели.
_Avoid_: dashboard, console, mon-server panel
