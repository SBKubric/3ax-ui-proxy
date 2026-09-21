# Runbook: тестовый стенд proxy chain

Стенд для приёмки карты [Proxy chain](https://github.com/SBKubric/3ax-ui-proxy/issues/68) по спеке `docs/spec/proxy-chain.md` §10 и эпика мониторинга. Продуктовые процедуры (как заводить звено, переустанавливать бокс, чинить цепочку) — в [proxy-front.md](proxy-front.md); здесь — только то, что относится к самому стенду: кто есть кто, где лежат бэкапы, как сбросить, что проверять и какие грабли уже собраны.

Секретов в этом файле нет и быть не должно: учётные данные панели, join-токены, hop secret'ы живут только на хостах и в локальном файле оператора вне репозитория.

## 1. Инвентарь

Пять VPS, все доступны по SSH под root по ключу; алиасы в `~/.ssh/config` рабочей машины.

| SSH-хост | IP | ОС / ресурсы | Роль в цепочке | Что стоит |
|---|---|---|---|---|
| `real` | 201.50.117.213 | Ubuntu 24.04, ~870 MB | real server | панель x-ui (`/usr/local/x-ui`), xray, sub-сервер https:2096, nginx (держит 80 после переустановки) |
| `bridge` | 194.87.80.122 | Debian 13, 1 vCPU / 380 MB | inner front | x-ui в режиме звена (`x-ui proxy`), relay dokodemo, sub-сервер 2096 |
| `proxy` | 170.168.112.15 | — | edge front (активное) | до фазы 2 — старый proxy front v1 (`upstreamHost` + relay-manifest); после — звено v2 |
| `monserver` | — | — | mon-server | эпик мониторинга (`SBKubric/3ax-ui-monitoring`) |
| `monclient` | — | — | mon-client | пробы через `direct`, `edge:<name>`, `inner:<name>` |

Топология: `клиент → proxy → bridge → real`. Панель знает всю цепочку; `proxy` знает только `bridge`; `bridge` знает только `real`.

Панель на `real`: порт и `webBasePath` были перегенерированы при переустановке 2026-09-21 — смотреть `ssh real '/usr/local/x-ui/x-ui setting -show'` (пароль там не печатается; он у оператора в `panel-creds.env`, см. §4). Панель после приёмки выключается владельцем.

Данные стенда на панели: один inbound `vless-reality` tcp/443, один клиент `stand-client` (subId `standsub01`), подписка `/sub-…/standsub01`. AWG/WG/MTProto не заведены, поэтому relayed ports = `443 tcp,udp` (+ `51820 udp`, если AWG-сервер существует в базе).

## 2. Версии и доставка

- Код цепочки — стек PR #88 → #90 → #91 → #94, #89, #92 → #93; интеграционная ветка `86-chain-stand` (= всё вместе), с неё режутся pre-release теги `v1.9.0-chain.N`.
- Скрипты берутся из ветки: `RAW=https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/86-chain-stand`. Бинарник — из релиза по тегу.
- Панель: обновление `bash <(curl -Ls $RAW/update.sh) --beta` (берёт свежайший релиз, включая pre-release). Свежая установка с печатью пароля: `printf '2\n' | bash <(curl -Ls $RAW/install.sh) v1.9.0-chain.N` — без `printf '2\n'` установщик без TTY молча уходит в `update.sh` и пароль не печатает.
- Звено: `install.sh v1.9.0-chain.N` в режиме `XUI_PROXY_MODE=1` с `PROXY_NEXT_HOP…`/`PROXY_JOIN_TOKEN` (proxy-front.md §3.2); обновление — `update.sh --beta` (ворота v1-конфига срабатывают до подмены бинаря).
- Релиз собирает GitHub Actions `Release 3AX-UI` (~15 мин: unit, Playwright e2e, восемь сборок). Проверка: `gh release view v1.9.0-chain.N -R SBKubric/3ax-ui-proxy --json assets --jq '[.assets[].name]'` должен содержать `x-ui-linux-amd64.tar.gz`.

## 3. Бэкапы и сброс

На `real` в `/root`:

| файл | что |
|---|---|
| `x-ui.db.pre-chain.bak` | `sqlite3 .backup` базы до цепочки (панель 1.8.1.4, override на proxy) — **эталон для отката** |
| `x-ui.db.pre-chain` (+ `-wal`, `-shm`) | сырые файлы той же базы (wal не был чекпойнтнут — использовать `.bak`) |
| `x-ui.db.1789958003.bak`, `x-ui.db.sqlitebackup.bak` | более ранние копии |
| `x-ui.db.freshinstall.bak` | база от чистой установки 1.9.0-chain.1 (с печатанными учётными данными) |
| `x-ui.db.phase2.bak` | перед фазой 2 (если фаза 2 запускалась) |

На `proxy` в `/root/backup/`: `proxy.json` и `relay-manifest.json` v1 (до переустановки звеном).

Перед любым разрушающим шагом: `ssh real 'sqlite3 /etc/x-ui/x-ui.db ".backup /root/x-ui.db.$(date +%s).bak"'`.

**Откат панели к «до цепочки»:** `systemctl stop x-ui; cp /root/x-ui.db.pre-chain.bak /etc/x-ui/x-ui.db; rm -f /etc/x-ui/x-ui.db-wal /etc/x-ui/x-ui.db-shm` и переустановка бинарника 1.8.1.x (`install.sh v1.8.1.5`), затем `x-ui setting -username … -password … -port … -webBasePath …`, потому что старые учётные данные неизвестны.

**Сброс звена в bootstrap:** `systemctl stop x-ui; rm -f /etc/x-ui/proxy.json /etc/x-ui/chain/document.json /etc/x-ui/chain-join.url` и переустановка (`install.sh` в режиме звена) или правка `proxy.json` (убрать `hopSecret`) + `systemctl start x-ui` → поднимется join page.

**Сброс реестра на панели:** удалить звенья через `POST /panel/api/chain/del/:id` (активное edge — только последним, с `{"force":true}`); ревизия не сбрасывается — это нормально.

## 4. Доступ к панели из скриптов

Учётные данные — в локальном файле оператора `panel-creds.env` (`PANEL_USER`, `PANEL_PASS`, `PANEL_PORT`, `PANEL_BASE`), `chmod 600`, не коммитить и не вставлять в чаты и тикеты.

```sh
. panel-creds.env
P="https://201.50.117.213:${PANEL_PORT}${PANEL_BASE}"
curl -sk -c /tmp/c -X POST "${P}login" --data-urlencode "username=$PANEL_USER" --data-urlencode "password=$PANEL_PASS"
curl -sk -b /tmp/c "${P}panel/api/chain/list"
curl -sk -b /tmp/c -H 'Content-Type: application/json' -X POST "${P}panel/api/chain/add" -d '{"name":"bridge","host":"194.87.80.122","role":"inner"}'
```

Остальные ручки — proxy-front.md §11. Join-токен возвращается один раз в ответе `add`/`reissueToken`; в логи писать замаскированным.

## 5. Сценарий приёмки (спека §10)

Фаза 1 (без разрыва клиентов) — выполнена 2026-09-21 на `v1.9.0-chain.1`, итог в [#86](https://github.com/SBKubric/3ax-ui-proxy/issues/86):

1. Панель на `real` обновлена; миграция создала звено `legacy` (edge, active) из `proxyOverrideHost`.
2. `bridge` заведён как inner, установлен по `PROXY_JOIN_TOKEN`, `x-ui chain status` joined, relay 443+51820, подписка через bridge идентична.
3. Волна: удаление pending-звена подняло ревизию, bridge подхватил её первым опросом.

Фаза 2 (с разрывом клиентов на время переустановки proxy и тестов смерти inner):

4. `update.sh --beta` на `real` и `bridge` — проверка исправленного обновления.
5. Повтор выпуска IP-сертификата на `bridge` (ACME по IPv4 по умолчанию).
6. `proxy` заводится как edge (next hop = bridge), переустанавливается звеном, делается активным, `legacy` удаляется. Клиентские ссылки не меняются (тот же хост).
7. Добавление/удаление inbound на панели → порт появляется/исчезает на обоих звеньях за ≤ 60 с.
8. Pending standby: `setActive` на него отказывает.
9. Удаление inner → proxy перецепляется на real волной; возврат bridge через `add` + `x-ui chain rejoin`.
10. Смерть inner (`systemctl stop x-ui` на bridge) → proxy `nextHop.reachable=false`, relay не гасится; починка по proxy-front.md §7 (`del`, `reissueToken`, `chain rejoin` на proxy).
11. Гигиена: `grep -RI "privateKey\|password" /etc/x-ui || echo clean` на обоих звеньях.

Проверки, которые повторяются после каждого шага: `x-ui chain status` на звеньях (ревизия == `chainRevision` панели, `stale:false`), `ss -ltnup | grep -E ':(443|51820|2096) '`, подписка через edge (`Profile-Web-Page-Url` указывает на edge), `openssl s_client -connect <edge>:443 </dev/null | head -5` совпадает с `real:443`.

## 6. Грабли, собранные на стенде

- **`update.sh --beta` до фикса** уходил в несуществующую ветку `dev` и ронял сервис после `systemctl stop` (юнит удалён). Исправлено на ветке: `REPO_BRANCH=main`, обёртка `x-ui.sh` из тарбола, загрузки до остановки. На старых версиях скрипта — не запускать.
- **socat на bridge:443** (`tcp-443.service`, проброс на чужой адрес) мешал relay — отключён и удалён с согласия владельца.
- **IPv6.** Боксы без глобального IPv6; acme.sh на dual-stack ждал 10 с и падал по таймауту API. Установщик теперь ходит к CA по IPv4 по умолчанию (`PROXY_TLS_IPV6=1`/`XUI_TLS_IPV6=1` включают IPv6), GitHub-загрузки — сначала `-4`.
- **IP-сертификат Let's Encrypt** живёт ~6 дней, продление каждые 3 дня требует свободный порт 80: звено, которое relay'ит 80, сертификат по IP держать не может (`PROXY_TLS=manual|none`).
- **`x-ui chain ports` из чужого cwd** падал (`bin/config.json` относительно cwd) — исправлено: путь от исполняемого файла, fallback на cwd для `go run`.
- **Футер установщика** печатал `WebBasePath` без слэшей, панель нормализует в `/…/` — исправлено; при сборке URL руками добавлять слэши.
- **Заведение pending-звена не бампает ревизию** (спека §2.6.3) — это не баг; бамп происходит при join, удалении, смене хоста, `setActive`.
- **`grep -R` по `/usr/local/x-ui/bin`** ловит бинарные `geosite.dat` — проверять только `/etc/x-ui` с `-I`.
- **Переустановка панели** генерирует AWG/WG-дефолты и ставит nginx на 80; возврат базы стенда эти дефолты отбрасывает, nginx остаётся.
- **`subDomain` на панели** включает валидатор домена перед `/chain/v1/*`: звенья должны ходить на панель по этому домену, не по IP. На стенде `subDomain` пуст.

## 7. Что после приёмки

- Итог фазы 2 — комментарием в #86, расхождения со спекой — правкой спеки тем же PR.
- Мониторинг через звенья (#87) — после мержа эпика #43: `monserver`/`monclient` пробируют `direct`, `edge:proxy`, `inner:bridge`.
- Панель на `real` владелец выключает; боксы можно оставить как есть или сбросить по §3.
