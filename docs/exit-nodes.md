# Свои выходные ноды: провайдер `exits`, цепочки и группы `EXIT-*`

Как подключить свою выходную ноду (VLESS + Reality), ходить на неё напрямую
или цепочкой через узел подписки, и направлять в неё отдельные наборы geosite.
Решения — [ADR-0049](adr/0049-exit-groups-as-set-targets.md) (группы как цель
набора) и [ADR-0050](adr/0050-exit-chains-via-subscription.md) (цепочки),
разведка — [exit-chains.md](recon/exit-chains.md), формат файла —
[контракт миксина](contracts/nikki-mixin-rulesets.md).

**Что делает демон, а что вы.** Узлы своих нод и цепочки демон пишет сам — в
файл провайдера `exits`, из экспортов репо выходных нод. Группы `EXIT-*` —
в профиле, их один раз добавляете вы (ADR-0002): демон только ссылается на
них из наборов.

## Раскладка

Пример — нода `kz-nqz-exit-01` с двумя выходами (выход выбирается по UUID):

| Выход | Группа | Для чего |
|---|---|---|
| `resi` — ISP-прокси, домашний IP | `EXIT-RESI` | **только AI**: `category-ai-!cn`, `openai`, `anthropic`. Прокси даёт 2–5 с на новое соединение и отказывает при частых соединениях — Google, PayPal и стриминги на нём не живут |
| `dc` — IP самого VPS | `EXIT-DC` | то, что режется по IP: Telegram, Instagram |

Что демон пишет в `/etc/nikki/run/providers/exits.yaml` (провайдер `exits`):

| Узел | Путь |
|---|---|
| `kz-nqz-exit-01-dc` | напрямую на ноду |
| `kz-nqz-exit-01-dc ⇢ 🇷🇺💳Россия` | через узел подписки (хоп) — по цепочке на каждый хоп пула |
| то же для `resi` | |

В каждой группе выхода три варианта, выбор ручной:

```
EXIT-DC = select [ EXIT-DC-DIRECT (прямо) | EXIT-DC-HOP (url-test над цепочками) | REJECT ]
```

- **`url-test` меряет всю цепочку до ноды**, а не хоп: хопы ss и ws провайдера
  подменяют адрес по SNI и ломают Reality, хотя сами живы. Такие цепочки
  отсеиваются сами.
- **`select` + `REJECT` — fail closed.** Сам `select` не переключается: нода
  легла — соединения рвутся, а не уходят через чужой IP.
- **В `AUTO` ноды не добавлять:** url-test смешал бы их с подпиской.
- **`udp: true` — часть kill switch.** Правило, чей узел не умеет UDP, mihomo
  не отклоняет, а пропускает: с `udp: false` QUIC к домену из набора уходил в
  хвост `MATCH,DIRECT` — с домашнего IP. Теперь UDP доходит до ноды: UDP 443 и
  весь UDP `resi` она блокирует (браузер переходит на TCP), остальной UDP `dc`
  выходит с IP VPS.

## 1. Экспорты нод на роутер

В репо выходных нод: `ansible-playbook node-export.yml` → `exports/<нода>.json`
(формат `schema: 1` — README там). Затем отсюда:

```sh
./scripts/exits-push.sh                                   # все exports/*.json соседнего репо
./scripts/exits-push.sh --host root@127.0.0.1 --ssh-port 18022   # тестовая VM
```

Скрипт кладёт файлы в `/etc/netmoded/exits/` (0600) потоком по ssh, не
печатая их, и зовёт `POST /api/exits/reload`: демон пересобирает
`exits.yaml` и записывает SNI своих нод в `sniffer.skip-domain` миксина (без
этого sniffer роутера ломает прямой вход на ноду для клиентов LAN).

**Сначала этот шаг, потом профиль:** профиль ссылается на `exits.yaml`, и
файл должен уже существовать.

## 2. Профиль — один раз

Файлы на роутер — только `ssh … 'cat > …' < файл`: `scp` там не работает
(в dropbear нет sftp-сервера).

```sh
ssh root@192.168.9.1
P=/etc/nikki/profiles/$(uci get nikki.config.profile | sed 's/^file://')
cp "$P" "$P.bak-$(date +%F)"
```

**Переезд с ручной раскладки** (ноды в `proxies:` по прежней версии этой
инструкции): убрать узлы ноды из `proxies:` и из `PROXY`, старые `EXIT-*`
заменить новыми. Имена групп те же — наборы в панели переназначать не нужно.

```sh
yq -i '.proxies |= map(select(.name | test("^kz-nqz-exit-01-") | not))' "$P"
yq -i '(.proxy-groups[] | select(.name == "PROXY") | .proxies) |= map(select(test("^kz-nqz-exit-01-") | not))' "$P"
yq -i '.proxy-providers.exits = {"type": "file", "path": "./providers/exits.yaml"}' "$P"
yq -i '.proxy-groups |= map(select(.name | test("^EXIT-") | not)) | .proxy-groups += [
  {"name": "EXIT-DC-HOP", "type": "url-test", "use": ["exits"], "filter": "-dc ⇢ ",
   "url": "https://www.gstatic.com/generate_204", "interval": 300, "tolerance": 100, "timeout": 8000, "empty-fallback": "REJECT"},
  {"name": "EXIT-RESI-HOP", "type": "select", "use": ["exits"], "filter": "-resi ⇢ ", "empty-fallback": "REJECT"},
  {"name": "EXIT-DC-DIRECT", "type": "select", "use": ["exits"], "filter": "-dc$", "empty-fallback": "REJECT"},
  {"name": "EXIT-RESI-DIRECT", "type": "select", "use": ["exits"], "filter": "-resi$", "empty-fallback": "REJECT"},
  {"name": "EXIT-DC", "type": "select", "proxies": ["EXIT-DC-DIRECT", "EXIT-DC-HOP", "REJECT"]},
  {"name": "EXIT-RESI", "type": "select", "proxies": ["EXIT-RESI-DIRECT", "EXIT-RESI-HOP", "REJECT"]}
]' "$P"
```

**Прямой узел — первым, отдельной группой `…-DIRECT`.** В `select` mihomo ставит
`proxies:` раньше узлов провайдера, и если бы прямой узел шёл через `use:`, по
умолчанию выбиралась бы `…-HOP` — а при выключенном пуле она пустая
(`COMPATIBLE`), и выход молча не работает. Так и случилось на роутере
14.11.2026 — проверено и исправлено на VM.

**`empty-fallback: REJECT` у `…-HOP` и `…-DIRECT` обязателен** (kill switch, проверено на VM:
без него пустой `exits.yaml` после перезапуска выпускал resi с домашнего IP). Пустая группа (пул хопов
выключен) иначе становится `COMPATIBLE`, а он у mihomo ведёт себя как `DIRECT`:
выбранное «через хоп» уходило бы с домашнего IP — fail-open. Замерено на
роутере 14.11.2026: пустая `…-HOP` отвечала за 30 мс, то есть напрямую.

**Автоподбор хопа для resi.** `EXIT-DC-HOP` — `url-test` над цепочками dc:
это и есть замеры, по которым выбирается хоп (через VPS в freedom — дёшево).
`EXIT-RESI-HOP` — `select` без проверок: внутри одна цепочка
`<нода>-resi ⇢ авто`, а её хоп раз в 30 с выставляет демон — лучший по
замеру цепочки dc той же ноды (гистерезис 150 мс; живых нет — `REJECT`).
Проверок через ISP-прокси нет вовсе: на частых новых соединениях он
отказывает, а участок «хоп → VPS» у dc и resi общий.

Проверить и применить:

```sh
mihomo -t -d /etc/nikki/run -f "$P"
/usr/local/bin/netmode-apply nikki
```

## 3. Пул хопов

Без пула цепочек нет — только прямые узлы (умолчание; так безопаснее для
прокси resi). Пул — до 8 узлов подписки, режимы как у авто-пула: `allow`
(отмеченные), `deny` (все, кроме), `provider` (балансировщик подписки), `off`.
Брать vision- и hysteria2-узлы; ss/ws цепочку не держат (url-test их отсеет,
но зачем). В панели: «Настройки» → «Цепочки до своих нод» (`#hops`) — там же
свои ноды, SNI для sniffer и кнопка «Перечитать ноды». То же с роутера:

```sh
T=$(cat /etc/netmoded/token); B=http://192.168.9.1:8088
FP=$(curl -s -H "Authorization: Bearer $T" $B/api/nikki/hops | jsonfilter -e @.fingerprint)
curl -s -X PUT -H "Authorization: Bearer $T" -H "If-Match: $FP" -H 'Content-Type: application/json' \
  -d '{"mode":"allow","nodes":["🇷🇺💳Россия","🇩🇪⚡Германия"]}' $B/api/nikki/hops
```

Применение — джоб с перезапуском Nikki; после него демон сверяет у движка,
что группы `nm-hop-*` есть. После обновления подписки пул пересобирается сам.

**Первые секунды после перезапуска** `url-test` стоит на первой цепочке, пока
не замерил остальные, — она может быть мёртвой. Это проходит само.

## 4. Наборы и проверка

«Узлы Nikki» → «Что в туннель · наборы» → у набора «Выход…» → `EXIT-RESI` или
`EXIT-DC` → «Применить». **Порядок решает, кто победит:** домен из двух
наборов уходит по верхнему (`geosite:google` включает `youtube`). «Изменить
порядок» в панели переставляет наборы; свои правила всегда раньше наборов.

Выбор «прямо / через хоп / закрыть» — на главной, карточка Nikki, блок «Свои
выходы» (держится до перезапуска движка). Или напрямую в Clash API:

```sh
S=$(uci get nikki.mixin.api_secret)
curl -s -X PUT -H "Authorization: Bearer $S" -d '{"name":"EXIT-DC-HOP"}' http://127.0.0.1:9090/proxies/EXIT-DC
curl -s -H "Authorization: Bearer $S" http://127.0.0.1:9090/proxies/EXIT-DC-HOP | jsonfilter -e '@.now'
```

Проверка выхода: набор `category-ip-geo-detect` (в нём `ipinfo.io`) — в
`EXIT-DC`, с домашней машины `curl -s https://ipinfo.io/json` → IP VPS; в
`EXIT-RESI` → домашний провайдер страны ноды.

## 5. Подписка для телефонов (Happ)

Роутер раздаёт подписку `http://192.168.9.1:8088/sub/happ/<токен>` — только
из домашней сети. Токен свой, не токен панели. В панели: «Настройки» →
«Подписка для телефонов» (`#phones`) — выпустить, показать и скопировать, кто
забирал. Выпустить или перевыпустить с роутера
(старая ссылка сразу перестаёт работать):

```sh
T=$(cat /etc/netmoded/token)
curl -s -X POST -H "Authorization: Bearer $T" http://192.168.9.1:8088/api/sub/rotate   # → {"url": …}
curl -s -H "Authorization: Bearer $T" http://192.168.9.1:8088/api/sub/link             # ссылка и кто забирал
```

Ссылку — в Happ («+» → из буфера или QR). Записи:

| Запись | Что делает |
|---|---|
| `Умный · прямо` / `Умный ⇢ хоп` | наборы панели: `EXIT-RESI`/`EXIT-DC` (или по стране — `EXIT-KZ-RESI`) → свои выходы (прямо или через цепочку), «в туннель» → узлы авто-пула, остальное — по политике панели |
| `<нода> · <выход> · прямо` | весь трафик через выход, напрямую |
| `<нода> · <выход> ⇢ <хоп>` | весь трафик через выход, цепочкой через хоп |

Хопы и туннель появятся после первого обновления подписки этой версией
демона (`/etc/netmoded/outbounds.json`). Вне дома Happ живёт на последней
скачанной версии. Прямые записи дома работают, только пока SNI своих нод в
`sniffer.skip-domain` (их пишет `exits-push.sh`).

## Kill switch

Трафик, отправленный в `EXIT-*-RESI`, выходит только с IP ISP-прокси или не
выходит вовсе ([разведка](recon/exit-chains.md), «Kill switch resi»):

- у групп `…-DIRECT` и `…-HOP` — `empty-fallback: REJECT`; группы хопов
  `nm-hop-*` демон пишет так же;
- на ноде у пользователя `resi` единственный выход — ISP-прокси;
- узлы выходов — с `udp: true`, иначе правило на `EXIT-*` для UDP (QUIC)
  пропускается и пакет уходит по хвосту правил напрямую;
- **не закрыто:** в выход попадает только то, что совпало с правилом. Домен
  вне набора, соединение без видимого имени (DoH + ECH, по IP) идут по хвосту
  — при политике «напрямую» с домашнего IP;
- каждый перезапуск nikki идёт под замком `inet netmode_guard`: на это время
  трафик LAN наружу закрыт целиком. nikki не поднялся — замок **остаётся**
  (LAN без интернета); снять — режим «Выключено» в панели;
- **не закрыто:** при загрузке роутера, пока nikki ещё не стартовал, трафик LAN
  идёт напрямую; на телефоне — включите в Android «Постоянная VPN» и
  «Блокировать соединения без VPN» для Happ, иначе при выключенном VPN телефон
  ходит напрямую.

## Откат и восстановление

- Цепочки убрать: пул `{"mode":"off"}` — останутся прямые узлы.
- Профиль из копии: `cp "$P.bak-<дата>" "$P" && /usr/local/bin/netmode-apply nikki`.
- **Перед удалением группы `EXIT-*` из профиля снимите её с наборов в панели.**
  Правило на исчезнувшую группу не даст mihomo подняться (`proxy [EXIT-DC] not
  found` в `/var/log/nikki/core.log`, `netmode-apply` — код 6). Если уже
  случилось — выключить склейку, поправить, включить:

```sh
uci set nikki.mixin.mixin_file_content=0 && uci commit nikki && /usr/local/bin/netmode-apply nikki
# … починить профиль или вернуть группу …
uci set nikki.mixin.mixin_file_content=1 && uci commit nikki && /usr/local/bin/netmode-apply nikki
```

Выключенная склейка уносит и группы `nm-hop-*`: цепочки отказывают, прямые
узлы и движок живы.
