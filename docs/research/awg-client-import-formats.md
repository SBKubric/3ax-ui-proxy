---
status: done
issue: https://github.com/SBKubric/3ax-ui-proxy/issues/32
map: https://github.com/SBKubric/3ax-ui-proxy/issues/30
date: 2026-09-12
---

# Форматы импорта AWG/WG-конфигов в клиентах: QR, `.conf`, `vpn://`

Вопрос тикета: что реально принимают клиентские приложения при импорте
AmneziaWG/WireGuard-конфига, влезает ли AWG-конфиг в QR, есть ли у Amnezia
механизм подписки, и что страница tunnel subscription должна рисовать на каждый
конфиг. Источники — исходники клиентов на GitHub (ветки/теги указаны у каждого
факта), docs.amnezia.org, wireguard.com, таблицы ISO/IEC 18004 от Denso Wave.
Проверено 2026-09-12; на устройствах ничего не запускалось — всё «по коду».

## Итог в трёх абзацах

**Один универсальный вход — сырой текст `.conf`.** Его принимают все клиенты,
которые нам интересны: AmneziaVPN (все платформы: QR, файл, вставка из буфера),
нативные AmneziaWG-приложения (Android/iOS: QR и файл; Windows/macOS: только
файл), официальные WireGuard-приложения (только для чистого WG — на строке
`Jc = …` они падают с «Unknown attribute»/«Invalid key»). Никакого JSON и
никакого `vpn://` для импорта не требуется.

**AWG-конфиг в QR влезает, но код плотный.** Реалистичный конфиг с ключами
по 44 символа: чистый WG — 381 байт, AWG 2.0 без I-пакетов — 452, AWG 2.0 с
S3/S4 и I1–I5 — 675, AWG 3.x — 916 байт (ISO-лимит байтового режима v40-L —
2953). На уровне коррекции L это версии QR 13 / 14 / 18 / 21 (77–109 модулей),
на M — 15 / 17 / 21 / 25. Сканеры телефонов формально держат все 40 версий,
но при 220 px на модуль приходится ~2 px — граница читаемости ML Kit. Значит:
QR от `.conf` рендерить на уровне L и размером не меньше 4 px на модуль
(≈ 320–450 px для AWG 3.x), плюс всегда давать «Скачать .conf» и «Копировать».

**Подписок у Amnezia нет.** `vpn://` — это не URL, а base64url(qCompress(JSON))
одноразового «ключа»; ни одно приложение Amnezia/AWG/WireGuard не умеет
периодически перечитывать конфиг по URL, аналога `Profile-Web-Page-Url` нет.
Схема `vpn://` как deep link зарегистрирована только в AmneziaVPN для Android.
Рекомендация: на каждую запись подписки — QR сырого `.conf` + скачивание +
копирование; `vpn://` — опциональная третья кнопка «Открыть в AmneziaVPN»
только для Android и только после проверки на устройстве.

## 1. Что принимает AmneziaVPN (amnezia-vpn/amnezia-client)

Ветка `dev` (HEAD `2a8242d3`, 2026-09-09; из неё режутся релизы — тег `5.0.1.5`
совпадает), `master` отстаёт на ~2 месяца и парсит WG/AWG иначе (см. ниже).
Импорт живёт в `client/core/controllers/selfhosted/importController.cpp`
([raw](https://raw.githubusercontent.com/amnezia-vpn/amnezia-client/dev/client/core/controllers/selfhosted/importController.cpp)).

### 1.1. Единая точка входа и сниффинг формата

Всё — файл, QR, вставка текста, `--import` в CLI, Android-intent — сходится в
`ImportController::extractConfigFromData`. Тип определяется по подстрокам
(`checkConfigFormat`, строки 36–72): `Servers/serversList` → бэкап;
`containers` / `api_key` / `auth_data` / (`hostName`+`userName`+`password`) →
Amnezia JSON; **`[Interface]` + `[Peer]` → WireGuard**; `inbounds`+`outbounds`
→ Xray; `client` + `dev tun|tap` → OpenVPN. До этого отдельно разбираются
`vless://`, `vmess://`, `trojan://`, `ss://`, `ssd://`.

Если формат не распознан, строка считается `vpn://`-ключом (строки 164–175):

```cpp
config.replace("vpn://", "");
QByteArray ba = QByteArray::fromBase64(config.toUtf8(),
        QByteArray::Base64UrlEncoding | QByteArray::OmitTrailingEquals);
QByteArray baUncompressed = qUncompress(ba);
if (!baUncompressed.isEmpty()) ba = baUncompressed;
config = ba;
configType = checkConfigFormat(config);
```

Следствия: префикс `vpn://` необязателен, несжатый payload тоже принимается, и
**после декодирования содержимое снова сниффится** — то есть, по коду,
`vpn://` + base64url(qCompress(сырой `.conf`)) должен импортироваться как
WireGuard/AWG-конфиг без обёртки в Amnezia JSON. Это вывод из кода, на
устройстве не проверен (см. §6).

### 1.2. `.conf` → внутренний JSON (`extractWireGuardConfig`, строки 505–634)

- Строки `key = value` собираются в плоскую карту, секции `[Interface]`/`[Peer]`
  игнорируются.
- **Обязательны** `Endpoint` (через `QUrl::fromUserInput`, порт по умолчанию
  51820), `PrivateKey`, `Address`, `PublicKey`; без любого из них —
  `ImportInvalidConfigError`.
- Копируются `PresharedKey`, `MTU` (иначе 1280 mobile / 1376 desktop),
  `PersistentKeepalive`, `AllowedIPs`; `DNS` → `dns1`/`dns2` **только если в
  строке два IPv4-адреса** (регэксп, строки 621–628); один адрес или IPv6-DNS
  отбрасываются молча. Сырой текст при этом сохраняется в `last_config.config`.
- **AWG vs WG**: AWG, если непустой хотя бы один ключ из
  `configKey::awgProtocolKeys()` (`configKeys.h`, строки 106–133,
  [raw](https://raw.githubusercontent.com/amnezia-vpn/amnezia-client/dev/client/core/utils/constants/configKeys.h)):
  `Jc Jmin Jmax S1 S2 S3 S4 H1–H4 I1–I5 HeaderProtectionKey
  ContentPaddingAddition RekeyAfterTime RekeyTimeout RejectAfterTime
  KeepaliveTimeout MaxHandshakeAttempts RandomTrailers DisableCookies`. Все
  присутствующие копируются как есть; контейнер — `amnezia-awg`, иначе
  `amnezia-wireguard`. Тот же список на теге `5.0.1.5` (релиз-ноты: «Added AWG
  3.1 support», [release](https://github.com/amnezia-vpn/amnezia-client/releases/tag/5.0.1.5)).
- `master` требует наличия **всех** `Jc, Jmin, Jmax, S1, S2, H1–H4`, не знает
  ключей AWG 3 — старые сборки с `master`-логикой могут отвергнуть конфиг, где
  панель что-то опустила. Форк пишет весь набор 2.0 всегда
  (`tunnel/config.go` `writeObfuscation`), так что это нас не задевает.

### 1.3. QR

`extractConfigFromQr` (строки 245–292) принимает: сырой текст, распознаваемый
`checkConfigFormat` (**QR с plain `.conf` работает**), сырой JSON, сырые байты
`qCompress`, base64url(+qCompress) JSON. Есть многокадровый QR для больших
конфигов: чанки по 850 байт сжатого payload, кадр = base64url(QDataStream:
`qint16 1984 | quint8 count | quint8 id | QByteArray`), до 255 кадров, ECC LOW
(`qrCodeUtils.cpp` 9–28, [raw](https://raw.githubusercontent.com/amnezia-vpn/amnezia-client/dev/client/core/utils/qrCodeUtils.cpp)).
Нам он не нужен: сам Amnezia для «AmneziaWG native format» делает **одиночный
plain-QR текста `.conf`** (`exportController.cpp` 224–227, 258–261) и пишет
«This config is too large for a QR code», если не влезает.

### 1.4. Буфер обмена и файлы

Отдельного чтения буфера нет: кнопка «Insert» вставляет текст в поле
(`PageSetupWizardTextKey.qml`, placeholder `vpn://`), дальше
`extractConfigFromData`. Файловый диалог на десктопе:
`Config files (*.vpn *.ovpn *.conf *.json)` (`PageSetupWizardConfigSource.qml`
347); расширение на разбор не влияет. Документация:
[supported formats](https://docs.amnezia.org/documentation/supported-configuration-formats/),
[connect via config](https://docs.amnezia.org/documentation/instructions/connect-via-config/),
[share](https://docs.amnezia.org/documentation/instructions/share-connection/)
(«AmneziaWG native = `.conf`, можно использовать в приложении AmneziaWG и на
роутере»; `vpn://` — «нельзя использовать в AmneziaWG app»).

### 1.5. Формат `vpn://`

`exportController.cpp` 43–47, 106–110, 370–373
([raw](https://raw.githubusercontent.com/amnezia-vpn/amnezia-client/dev/client/core/controllers/selfhosted/exportController.cpp)):

```
vpn:// + base64url_nopad( qCompress( QJsonDocument(serverJson).toJson(), 8 ) )
```

`qCompress` = 4-байтовый big-endian размер несжатых данных + zlib-поток.
`serverJson` — объект сервера Amnezia (`hostName`, `port`, `containers[]`,
`defaultContainer`, `description`, `dns1/dns2`, для «connection only» —
без `userName`/`password`); для AWG `containers[0] = {"container":
"amnezia-awg", "awg": {"last_config": "<JSON-строка>", "isThirdPartyConfig":
true, …}}`, а внутри `last_config` — `config` (сырой INI), `hostName`, `port`,
`client_priv_key`, `client_ip`, `psk_key`, `server_pub_key`, `mtu`,
`persistent_keep_alive`, `allowed_ips` и AWG-ключи. Формат нигде не
задокументирован и не версионирован — его знает только код клиента.

Для нашего AWG 3.x-конфига `vpn://` с обёрткой `{"containers":[{"awg":
{"last_config": …}}]}` — 998 байт JSON → 551 байт qCompress → 742 символа
ссылки → QR v22 на уровне M (посчитано segno; см. §4). То есть `vpn://` в
QR ничем не лучше plain-`.conf`, а на Windows/iOS/macOS не является
кликабельной ссылкой.

### 1.6. Deep links

- **Android — да**: `client/android/AndroidManifest.xml` 102–152
  ([raw](https://raw.githubusercontent.com/amnezia-vpn/amnezia-client/dev/client/android/AndroidManifest.xml)):
  `ImportConfigActivity` с `ACTION_VIEW`, `scheme="vpn"`, `host="*"`,
  `BROWSABLE` (ссылка на веб-странице открывает приложение), плюс `ACTION_SEND`
  `text/plain` и `ACTION_VIEW` файлов `*.vpn`, `*.cfg`, `*.conf`.
- **iOS/macOS — нет URL-схемы**: в `Info.plist.in` нет `CFBundleURLTypes`,
  только типы документов (`vpn`, `conf`, `cfg`, `ovpn`, `backup`);
  `QtAppDelegate.mm` обрабатывает только `fileURL`.
- **Windows/Linux — нет** регистрации протокола; есть CLI `--import <data>`.
- `amnezia://`, Universal/App Links — нет.

### 1.7. Подписки / ре-фетч

Единственный сетевой механизм — Amnezia Gateway (API v2, `config_version: 2`,
`subscriptionController.cpp`): перезапрос конфига при подключении, если
контейнеров нет или истёк `expires_at`, и при смене страны. Тело запросов
шифруется RSA-ключом `PROD_AGW_PUBLIC_KEY`, вшитым при сборке из переменной
окружения (`gatewayController.cpp` 150–183, `client/CMakeLists.txt` 28–37);
сервер не опубликован. Сторонний хостинг с официальной сборкой невозможен.
Clash/V2Ray-подписок, `Profile-Web-Page-Url`, таймерного обновления — нет.

## 2. Нативные AmneziaWG-приложения

| Приложение | Входы импорта | Принимаемые `[Interface]`-ключи | Неизвестный ключ | Имя туннеля |
|---|---|---|---|---|
| [amneziawg-android](https://github.com/amnezia-vpn/amneziawg-android) (`master`; релизы v2.0.0 → v3.0.1 «awg3» → v3.1.20260814; только Google Play, F-Droid — нет, issue #6 открыт) | файл `.conf`/`.zip` (`TunnelImporter.kt`: `require(isZip)` → «bad extension»), QR (zxing `ScanContract`), вручную | `address dns excluded/includedapplications listenport mtu privatekey jc jmin jmax s1–s4 h1–h4 i1–i5 headerprotectionkey contentpaddingaddition rekeyaftertime rekeytimeout rejectaftertime keepalivetimeout maxhandshakeattempts randomtrailers disablecookies` ([Interface.java](https://raw.githubusercontent.com/amnezia-vpn/amneziawg-android/master/tunnel/src/main/java/org/amnezia/awg/config/Interface.java)) | `BadConfigException(… UNKNOWN_ATTRIBUTE)` | `[a-zA-Z0-9_=+.-]{1,15}` ([Tunnel.java](https://raw.githubusercontent.com/amnezia-vpn/amneziawg-android/master/tunnel/src/main/java/org/amnezia/awg/backend/Tunnel.java)); из файла — имя файла без `.conf`, из QR — диалог |
| [amneziawg-apple](https://github.com/amnezia-vpn/amneziawg-apple) (`master`; App Store «AmneziaWG» 3.1.4, iPhone/iPad/Mac) | iOS: файл (`com.wireguard.config.quick`, text, zip), QR, вручную; macOS: файл/zip, вручную; буфера обмена нет | тот же список ([TunnelConfiguration+WgQuickConfig.swift](https://raw.githubusercontent.com/amnezia-vpn/amneziawg-apple/master/Sources/Shared/Model/TunnelConfiguration+WgQuickConfig.swift)) | `ParseError.interfaceHasUnrecognizedKey` | только непустое и уникальное; лимита 15 в коде нет |
| [amneziawg-windows-client](https://github.com/amnezia-vpn/amneziawg-windows-client) (3.1.0 «awg3.1»; парсер из `amneziawg-windows/v3 conf/parser.go`) | **только файл** `*.zip;*.conf` и «Add empty tunnel»; QR нет | тот же список + `preup postup predown postdown table` ([parser.go](https://raw.githubusercontent.com/amnezia-vpn/amneziawg-windows/master/conf/parser.go)) | `Invalid key for [Interface] section` | `^[a-zA-Z0-9_=+.-]{1,32}$` + зарезервированные имена Windows ([name.go](https://raw.githubusercontent.com/amnezia-vpn/amneziawg-windows/master/conf/name.go)); имя = имя файла |
| [amneziawg-tools](https://github.com/amnezia-vpn/amneziawg-tools) `awg-quick`/`awg setconf` (3.1.20260812) | файл `/etc/amnezia/amneziawg/<name>.conf` | как выше; I1–I5 — `strdup` без ограничения длины; `Jc/Jmin/Jmax/S1–S4` ≤ 65535, `H1–H4` — `u32_range` ([config.c](https://raw.githubusercontent.com/amnezia-vpn/amneziawg-tools/master/src/config.c)) | «Line unrecognized» | `[a-zA-Z0-9_=+.-]{1,15}\.conf` |

Ни в одном из них нет импорта по URL и обновления по расписанию (в импортёрах
нет сетевого кода; в Windows-клиенте единственный URL — самообновление
приложения). Amnezia-документация об
[альтернативных клиентах](https://docs.amnezia.org/documentation/alternative-clients/)
относит «Subscription Key» только к AmneziaVPN и стороннему DefaultVPN.

Ограничения на размер конфига по I-параметрам: в amneziawg-go нет константы
максимального размера спец-пакета; реальные пределы — UDP-датаграмма
(`MaxSegmentSize` 65535 / 1700 iOS / 2016 Windows) и 64 КиБ на строку UAPI
(`bufio.Scanner` по умолчанию, [uapi.go](https://github.com/amnezia-vpn/amneziawg-go/blob/master/device/uapi.go)).
Генератор форка даёт `<r 32..256>` и заголовки в десятки байт — далеко от
любого предела; лимитирует только QR.

## 3. Официальные WireGuard-приложения и AWG-ключи

Все четыре парсера отвергают конфиг с AWG-параметрами — импорт **падает, а не
игнорирует** лишние ключи:

- wireguard-android `Interface.parse`: только `address dns excludedapplications
  includedapplications listenport mtu privatekey`; иначе `BadConfigException(…
  UNKNOWN_ATTRIBUTE)` → пользователю «Unable to import tunnel: Unknown
  attribute in [Interface]» ([Interface.java](https://raw.githubusercontent.com/WireGuard/wireguard-android/master/tunnel/src/main/java/com/wireguard/config/Interface.java)).
- wireguard-apple: `interfaceSectionKeys = [privatekey listenport address dns
  mtu]` → `ParseError.interfaceHasUnrecognizedKey` ([swift](https://raw.githubusercontent.com/WireGuard/wireguard-apple/master/Sources/Shared/Model/TunnelConfiguration+WgQuickConfig.swift)).
- wireguard-windows `conf/parser.go`: «Invalid key for [Interface] section».
- wireguard-tools `wg setconf`: «Line unrecognized: `Jc=4'» ([config.c](https://git.zx2c4.com/wireguard-tools/plain/src/config.c));
  `wg-quick` вырезает только `Address MTU DNS Table Pre/Post* SaveConfig` и
  остальное отдаёт `wg`.

Входы официальных приложений те же, что у AWG-форков: Android — файл/zip, QR,
вручную; iOS — файл, QR, вручную; macOS и Windows — файл/zip и вручную, QR нет
([wireguard.com/install](https://www.wireguard.com/install/)). QR ожидает сырой
INI — официальная подсказка в сканере Android: «Tip: generate with `qrencode -t
ansiutf8 < tunnel.conf`» (`strings.xml`). Имя: Android `[a-zA-Z0-9_=+.-]{1,15}`
(ограничение ядра, [объяснение Донэнфельда](https://lists.zx2c4.com/pipermail/wireguard/2019-December/004772.html)),
Windows — до 32, Apple — без лимита длины в коде.

Вывод для подписки: запись `kind: wg` можно импортировать и в официальный
WireGuard, и в Amnezia-клиенты; запись `kind: awg` — только в AmneziaVPN и
нативные AmneziaWG. Страница должна это подписывать, чтобы пользователь не
получил «Unknown attribute».

## 4. QR: ёмкость против реального конфига

`tunnel/testdata/awg_client.conf` — 342 байта, но с ключами-заглушками
(`C1_PRIV`, `SERVER_PUB`, `C1_PSK`). Реальные ключи — 44 символа base64;
пересчёт по форме `GenerateClientConfig` (`tunnel/config.go`) и генераторов
`tunnel/obfuscation.go` (H — диапазоны до 2³¹, I1 `<r 32..256>`, I2–I5 с
заголовками QUIC/STUN/DTLS/`<t>`, 3.0 — `HeaderProtectionKey` 44 символа плюс
шесть диапазонов и `RandomTrailers = on`):

| Вариант | Байт | Строк | QR L | QR M | QR Q | QR H |
|---|---|---|---|---|---|---|
| чистый WG (Address v4+v6, DNS v4+v6, MTU, PSK) | 381 | 12 | v13 (77²) | v15 (85²) | v18 (97²) | v20 (105²) |
| AWG 2.0 минимум (как testdata, без S3/S4/I) | 452 | 21 | v14 (81²) | v17 (93²) | v20 (105²) | v23 (117²) |
| AWG 2.0 полный (S3/S4, I1–I5, H-диапазоны) | 675 | 28 | v18 (97²) | v21 (109²) | v25 (125²) | v29 (141²) |
| AWG 3.x (+ HeaderProtectionKey, таймеры, RandomTrailers) | 916 | 36 | v21 (109²) | v25 (125²) | v30 (145²) | v34 (161²) |

Посчитано `segno` 1.6 в байтовом режиме (base64-ключи и строчные буквы не
укладываются в алфавитно-цифровой режим). Ёмкость байтового режима по
[Denso Wave](https://www.qrcode.com/en/about/version.html), сверена с
`qrcode.util.BIT_LIMIT_TABLE` и zxing `Version.java`:

| Версия | Модули | L | M | Q | H |
|---|---|---|---|---|---|
| 10 | 57 | 271 | 213 | 151 | 119 |
| 15 | 77 | 520 | 412 | 292 | 220 |
| 20 | 97 | 858 | 666 | 482 | 382 |
| 25 | 117 | 1273 | 997 | 715 | 535 |
| 30 | 137 | 1732 | 1370 | 982 | 742 |
| 40 | 177 | 2953 | 2331 | 1663 | 1273 |

Итак, даже AWG 3.x укладывается в один QR с большим запасом по ISO, вопрос
только в читаемости плотного кода. Первичного «максимума версии» для сканеров
нет: zxing и ML Kit реализуют все 40 версий; ML Kit
[требует](https://developers.google.com/ml-kit/vision/barcode-scanning/android)
не меньше 2 px на модуль и предупреждает, что плотным кодам нужно больше
пикселей. Вторичный ориентир — вендорский бенчмарк Dynamsoft (2026-07): на
кодах v20+ ZXing распознаёт 5 %, ZBar 27 %, коммерческие — 97 %. Практический
вывод: держать версию как можно ниже (уровень L, без лишних строк) и рисовать
QR крупно.

Что это значит для форка:

- Модалка панели рисует QRious с `level: "L"` и `app.qrCodeSize || 450` px
  (`web/html/modals/qrcode_modal.html`, `setQrCode`) — для v21 это ≈ 4 px на
  модуль, нормально.
- Страница подписки рисует QRious 220 px (`web/assets/js/subscription.js`,
  `drawQR`) — для v21 это ≈ 2 px на модуль, на пределе. Блоку AWG/WG нужен свой
  размер: не меньше 4 px на модуль (≈ 330 px для AWG 2.0 полного, ≈ 440 px для
  3.x), либо считать размер от версии.
- Telegram-бот шлёт PNG 320 px на уровне **Medium** (`go-qrcode`,
  `web/service/tgbot.go` `SendAwgConfigsToClients`) — для AWG 3.x это v25 при
  2.7 px на модуль; стоит опустить до Low или поднять размер до 512.
- Экономия байтов без потери смысла: в форке 2.0 всегда пишет `S1 = 0`,
  `S2 = 0`, `H1..H4 = 1..4` даже для «дефолтных» значений — это нужно
  `master`-логике Amnezia (требует все девять ключей), так что оставляем.
  `AllowedIPs` с пробелом после запятой, `PersistentKeepalive` и IPv6 в
  `Address`/`DNS` — единицы байтов, не влияют на версию.

## 5. Сторонние клиенты (для полноты)

- **Hiddify** (`ray2sing` main `caf5e9ac`): подписка и буфер принимают сырой
  `[Interface]…` текст и `wg://`/`wireguard://`/`awg://` URI с параметрами
  `jc= jmin= … i5=`, но обе ветки кода собирают обычный sing-box `wireguard` с
  `if true || isAwg` — AWG-параметры выбрасываются, работает только чистый WG
  ([awg.go](https://github.com/hiddify/ray2sing/blob/main/ray2sing/awg.go)).
  Заголовки `Profile-Title`/`Profile-Web-Page-Url` — это его собственный
  формат подписки, к Amnezia отношения не имеет.
- **v2rayN / v2rayNG / NekoBox / Streisand / FoXray / Happ** — Xray или
  upstream sing-box: `infra/conf/wireguard.go` Xray-core не знает `Jc`/`awg`
  (grep пуст), значит AWG невозможен; `.conf` и `wireguard://` для чистого WG
  импортируются (v2rayN `WireguardFmt.cs`, v2rayNG `WireguardFmt.kt`, NekoBox
  `RawUpdater.kt`). **Karing** — «no further expansion of the wg protocol»
  (issues #727, #1136). **Shadowrocket** — по истории версий App Store
  (2.2.78/2.2.79) есть AmneziaWG-обфускация; набор параметров и формат
  импорта не проверяемы (закрытый код).
- Форки sing-box с типом `awg` (Hiddify `extended`, `amnezia-vpn/amnezia-box` c
  ключами 3.x) существуют, но из ссылок/конфигов в приложениях недоступны.

Итог: `wireguard://`-URI и sing-box/Xray-клиенты для AWG не подходят; ради них
ничего не рендерить.

## 6. Рекомендация для страницы подписки

Для каждой записи `{kind, name, conf}` из ответа `/tun/<subId>`:

1. **QR сырого текста `.conf`** — уровень L, размер ≥ 4 px на модуль (для
   AWG 3.x — ~440 px; проще всего считать от версии QR или взять 450 как в
   модалке панели). Это единственный формат, который сканируют и AmneziaVPN
   (Android/iOS/desktop c камерой), и AmneziaWG (Android/iOS), и WireGuard
   (Android/iOS, только `kind: wg`).
2. **Кнопка «Скачать .conf»** — для Windows и macOS (AmneziaWG-, WireGuard- и
   Amnezia-десктоп принимают только файл; на Android/iOS файл тоже
   импортируется). Имя файла = имя туннеля: оно становится именем интерфейса
   при импорте из файла, поэтому санировать до `[a-zA-Z0-9_=+.-]{1,15}`
   (лимит Android/`wg-quick`; Windows допускает 32, но 15 покрывает всех).
   Если имя клиента не проходит регэксп — fallback вида `awg-<n>`/`wg-<n>`.
3. **Кнопка «Копировать»** — в AmneziaVPN вставляется в поле «текстовый ключ»,
   в нативных клиентах — в «Create from scratch»; закрывает случай, когда
   страница открыта на самом телефоне и камера бесполезна.
4. **Подпись, какое приложение брать**: `awg` → AmneziaVPN или AmneziaWG
   (ссылки на Google Play / App Store / releases для Windows), с явным «в
   официальном WireGuard не откроется»; `wg` → любой из них плюс официальный
   WireGuard.
5. **`vpn://` — опционально, только Android, только после прототипа.**
   Кликабельна лишь на Android (схема зарегистрирована в AmneziaVPN); на iOS
   и десктопах это просто текст для вставки, не лучше `.conf`. По коду `dev`
   достаточно `vpn://` + base64url(qCompress(`.conf`)) без Amnezia-JSON, но
   формат неофициальный (Qt-заголовок 4 байта + zlib, base64url без `=`), не
   версионирован и различается между `master` и `dev`, так что перед
   реализацией нужен ручной прогон на устройстве. Если делать — как третью
   кнопку «Открыть в AmneziaVPN» с `navigator.userAgent`-проверкой Android;
   в QR не класть.
6. **Не делать**: Amnezia JSON / `.vpn`, `wireguard://`-URI, многокадровый QR,
   ссылку на «подписку» в Amnezia-клиентах (механизма нет). Периодического
   обновления у туннельных клиентов не будет — это надо честно написать на
   странице («при смене сервера отсканируйте заново»), а канал уведомления
   остаётся Telegram-бот (`SendAwgConfigsToClients` уже шлёт `.conf` + QR).

Что стоит перепроверить руками до спеки UI: (а) импорт `vpn://` с сырым `.conf`
внутри на Android-сборке 5.0.1.x; (б) распознавание QR v21 на 330–450 px
камерой AmneziaVPN (Qt/zxing-cpp) и AmneziaWG (zxing-android-embedded) на
среднем телефоне; (в) как AmneziaVPN на iOS открывает скачанный `.conf` из
Safari (по `Info.plist` тип документа зарегистрирован — должно предлагать
«Открыть в AmneziaVPN»).
