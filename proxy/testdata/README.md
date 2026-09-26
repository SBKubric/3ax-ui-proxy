# Эталоны фронта коробки

Сняты с `BuildFront` (`proxy/front.go`) на документах из `proxy/front_test.go`
(`edgeFrontDocument`, `innerFrontDocument`) — цепочка `real ← inner-1 ← {edge-a
(active), edge-b}`, у edge-a target-сосед `www.neighbour.example:443` (#140).

| файл | что покрывает |
|---|---|
| `front_edge_stream.conf`, `front_edge_http.conf` | edge с target-соседом: своё имя сервера — сырым потоком на 443 next hop'а, неизвестный SNI — сырым потоком на target-сосед, без SNI — HTTP-сторона с IP-сертификатом перед sub-сервером коробки (sub/json, `/chain/v1/`, `/join/`) |
| `front_edge_notarget_stream.conf` | edge без target-соседа в документе: неизвестный SNI получает заглушку, имени для клиентов нет (предупреждение в логе) |
| `front_inner_stream.conf`, `front_inner_http.conf` | inner: имя active edge — сырым потоком на next hop, имя standby edge и неизвестный SNI — заглушка |

Эталоны не заморожены: осознанное изменение вывода перегенерируется командой

```
go test ./proxy/ -run TestFront -update
```

и уезжает в коммит вместе с правкой сборщика.

Что легко сломать незаметно:

- маршруты коробки всегда `Raw`: между коробками поток идёт без
  PROXY-заголовка, иначе фронт соседа не прочтёт SNI;
- loopback-порты (`NewFrontLayout`) выбираются в обход relayed ports и старого
  sub-порта — relay слушает свои порты на всех адресах, loopback тоже;
- один документ — одни и те же порты, иначе каждый опрос перезагружал бы nginx.
