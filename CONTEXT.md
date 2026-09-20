# 3AX-UI proxy front

Панель управления Xray (форк 3x-ui) с режимом «прокси-фронт»: одноразовые серверы, которые принимают клиентский трафик и пробрасывают его по цепочке на скрытый реальный сервер.

## Language

**Real server**:
Сервер с панелью, inbound'ами, клиентами и историей трафика; его адрес никогда не попадает в клиентские конфиги.
_Avoid_: upstream, hidden server, panel box

**Proxy front**:
Одноразовый сервер-звено, который принимает трафик от клиентов или от внешнего звена и relay'ит его на next hop; при блокировке выбрасывается и заменяется.
_Avoid_: proxy box, relay server, front

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
