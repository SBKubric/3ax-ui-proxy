# 3AX-UI proxy front

Панель управления Xray (форк 3x-ui) с режимом «прокси-фронт»: одноразовый сервер, который принимает клиентский трафик и пробрасывает его на скрытый реальный сервер.

## Language

**Real server**:
Сервер с панелью, inbound'ами, клиентами и историей трафика; его адрес никогда не попадает в клиентские конфиги.
_Avoid_: upstream, hidden server, panel box

**Proxy front**:
Одноразовый сервер, который клиенты видят вместо real server; при блокировке выбрасывается и заменяется.
_Avoid_: proxy box, relay server, front

**Relay**:
Процесс на proxy front (xray dokodemo-door), который на уровне L4 пробрасывает relayed ports на real server без терминации TLS.
_Avoid_: forwarder, tunnel

**Relayed port**:
Публичный порт real server, который relay открывает у себя и пробрасывает один-в-один. Складывается из xray inbound ports и extra ports.

**Xray inbound port**:
Relayed port, который relay узнаёт из экспортированного xray-конфига панели.

**Extra port**:
Relayed port, который real server обслуживает вне xray (AmneziaWG, WireGuard, MTProto) и который поэтому перечисляется в конфиге proxy front явно.
_Avoid_: host port, additional port

**Host override**:
Глобальная настройка панели, подменяющая адрес real server на адрес proxy front во всех выдаваемых конфигах и ссылках подписки.
_Avoid_: proxy override, address substitution
