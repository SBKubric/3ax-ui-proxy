# 3AX-UI proxy front

Панель управления Xray (форк 3x-ui) с режимом «прокси-фронт»: одноразовый сервер, который принимает клиентский трафик и пробрасывает его на скрытый реальный сервер.

## Language

**Real server**:
Сервер с панелью, inbound'ами, клиентами и историей трафика; его адрес никогда не попадает в клиентские конфиги.
_Avoid_: upstream, hidden server, panel box

**Proxy front**:
Одноразовый сервер, который клиенты видят вместо real server; при блокировке выбрасывается и заменяется.
_Avoid_: proxy box, relay server, relay панель, front

**Relay**:
Процесс на proxy front (xray dokodemo-door), который на уровне L4 пробрасывает relayed ports на real server без терминации TLS.
_Avoid_: forwarder, tunnel

**Relayed port**:
Публичный порт real server, который relay открывает у себя и пробрасывает один-в-один. Складывается из xray inbound ports и extra ports.

**Xray inbound port**:
Relayed port, который relay узнаёт из relay manifest.

**Relay manifest**:
Санированная выписка из xray-конфига real server — только адрес, порт, протокол, тег и признак TPROXY каждого inbound'а, без ключей и паролей; единственный вход relay об xray inbound ports.
_Avoid_: panel config, exported config.json, panel-xray.json

**Setup page**:
Одноразовая страница на proxy front по секретной ссылке, через которую владелец вставляет relay manifest; исчезает, как только манифест принят.
_Avoid_: bootstrap page, onboarding, wizard

**Extra port**:
Relayed port, который real server обслуживает вне xray (AmneziaWG, WireGuard, MTProto) и который поэтому перечисляется в конфиге proxy front явно.
_Avoid_: host port, additional port

**Host override**:
Глобальная настройка панели, подменяющая адрес real server на адрес proxy front во всех выдаваемых конфигах и ссылках подписки.
_Avoid_: proxy override, address substitution

**mon-server**:
Единственный внешний сервис мониторинга на отдельном сервере: ведёт реестр mon-clients, получает у real server конфиги probe accounts и текущий host override, раздаёт mon-clients их targets, считает состояние каждого target и сообщает real server переходы и статистику.
_Avoid_: monitoring hub, collector, watchdog server

**mon-client**:
Коробка в целевом регионе, которой mon-server назначает набор targets; для каждого поднимает туннель (xray-core, awg) и раз в минуту шлёт mon-server tunnel probe через туннель и heartbeat мимо него.
_Avoid_: agent, probe node, sensor

**Target**:
Пара «inbound real server × path», которую проверяет один mon-client через свой probe account. Единица состояния UP/DOWN и статистики.
_Avoid_: check, monitor, endpoint

**Path**:
Через какой адрес target достигает real server: `proxy` (адрес proxy front из host override) или `direct` (настоящий адрес real server).
_Avoid_: mode, route

**Probe account**:
Служебная учётная запись клиента в inbound'е (или AWG-клиент), созданная по запросу mon-server для одного target; отличается от пользовательских соглашением по имени и не считается пользователем.
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

**Tunnel subscription**:
Публичный маршрут подписки, отдающий по subId клиентские конфиги AmneziaWG и WireGuard той же подписки; дополняет xray-подписку, не меняя её.
_Avoid_: AWG subscription, conf feed, tunnel feed
