# Наборы geosite для Nikki — спека (что строим)

Статус: **согласована владельцем 2026-09-07**, заменяет черновик с UCI-хранилищем
и снимком каталога в бинаре. Пошаговый план реализации —
`../plans/2026-09-07-geosite-rulesets.md`.

---

## 1. Context

Панель роутера (`netmoded`, Go + Preact, OpenWrt) должна дать владельцу выбрать
наборы доменов **geosite** из `MetaCubeX/meta-rules-dat` (ветка `meta`, 1899
имён) и политику остального трафика: «только выбранное в туннель» либо «всё,
кроме выбранного». Демон переводит выбор в конфигурацию Nikki (mihomo),
перезапускает движок и сверяет по Clash API, что наборы загрузились. Каталог
показывается мгновенно и **ничего не пишет во флеш**. Всё это только в режиме
`nikki`; b4 наборов не знает.

Макет: Claude Design, проект `9ade19de…`, файл `netmoded Panel.dc.html`,
вкладка «Что в туннель · наборы» в карточке Nikki (строки 175–245 макета:
две вкладки, сегментный переключатель политики, чипы выбранного, «Наборы
пакетом», поиск, строки наборов с тегами, блок «Изменений: N» с «Применить /
Сбросить», подпись про источник).

Сегодня владелец правит `/etc/nikki/profiles/main.yml` руками: там семь
`rule-providers` (`path: ./rules/<имя>.mrs`), `tg-ip` из geoip с `no-resolve`,
хвост `GEOIP,PRIVATE,DIRECT,no-resolve` и `MATCH,DIRECT`. Панель заменяет
ровно эту ручную правку.

Роутер (192.168.9.1) в момент планирования был недоступен: механика Nikki снята
с исходников upstream (`nikkinikki-org/OpenWrt-nikki` 2026.04.08) и mihomo
(Alpha, 2026-09-07). Что снять с железа до реализации — §11.

### Решения владельца (закрыты 2026-09-07)

| Вопрос | Решение |
|---|---|
| Каталог наборов | **живой список из GitHub, в памяти демона**, не снимок в бинаре: репозиторий меняется. Ничего на флеш |
| Хранилище выбора | **`/etc/nikki/mixin.yaml`, файл демона целиком** (`nikki.mixin.mixin_file_content=1`), не UCI-секции |
| Старые rule-providers в `main.yml` | владелец уберёт сам один раз; README говорит, что оставить |
| geoip | у имён, для которых есть `geo/geoip/<имя>.mrs`, **автоматически** второй провайдер (`behavior: ipcidr`) и `RULE-SET …,no-resolve`; в панели метка «+ip» |
| Цель туннеля | `BYPASS` (`Fallback[PROXY, REJECT]`, fail-closed: мёртвый туннель даёт REJECT, а не утечку) |
| Откуда качать `.mrs` | опция `download: direct \| tunnel`, умолчание `direct`; при `tunnel` — `proxy: PROXY` у провайдеров |
| Третье состояние | `profile` (наших правил нет) показывается явно + кнопка «Вернуть профилю» |
| Кэш `.mrs` во флеше (`/etc/nikki/run/rules/`) | приемлемо: так уже сегодня |
| Интервал обновления наборов | 86400 |
| Исполнение | ветка `geosite-rulesets` от `master`, субагент на задачу, коммит на задачу |

### Как это работает в Nikki (снято с `nikki/files/nikki.init:155-197`, `ucode/mixin.uc`)

`/etc/nikki/run/config.yaml` собирается на **старте службы**:
профиль ⊕ `/etc/nikki/mixin.yaml` (при `mixin.mixin_file_content=1`) ⊕ вывод
`mixin.uc` из UCI, через `yq eval-all '... | . as $item ireduce ({}; . * $item) |
.rules = .nikki-rules + .rules | del(.nikki-rules) | …'`. Глубокое слияние,
последний побеждает; `rule-providers` сливаются по ключу; `nikki-rules`
**приписываются перед** `rules` профиля. Следствия:

- наш `MATCH` закрывает правила профиля, не трогая их;
- `GEOIP,PRIVATE,DIRECT,no-resolve` обязан стоять в нашем блоке перед `MATCH`,
  иначе в политике «всё, кроме» локальные адреса ушли бы в туннель;
- применить без перезапуска нельзя; перезапуск — только `netmode-apply nikki`
  (`Executor.ApplyMode`, единственное место старта/стопа движков, SPEC §4).

mihomo (`rules/provider/parse.go:51-59`, `component/resource/fetcher.go:100-111`,
`hub/executor/executor.go:318-336`): http-провайдер без `path` кладёт файл в
`<home>/rules/<md5(url)>`; с `path: ./rules/x.mrs` — по этому пути (так у
владельца). Первое неудачное скачивание **не валит старт**: провайдер остаётся
пустым и молча не матчит ничего, цикл повтора заводится. `GET /providers/rules`
отдаёт `{providers: {name: {name, type, vehicleType, behavior, format, ruleCount,
updatedAt}}}` (`hub/route/provider.go:113-138`, `rules/provider/provider.go:107-118`);
`updatedAt` нулевое = ни разу не скачан. `mihomo -t` провайдеров не качает.

Каталог (`git/trees` ветки `meta`, 2026-09-07): `geo/geosite/` — 5698 файлов, 1899
имён в `.mrs/.yaml/.list`; `geo/geoip/` — 260 `.mrs`; у 13 имён есть оба файла
(telegram, google, cloudflare, facebook, netflix, twitter, fastly, tor, cn, private,
hm, sb, st). Имена: строчные латиница, цифры, `-`, `@`, `!`. Медианный `.mrs` —
115 байт; `cn` 538 КБ. Рекурсивное дерево GitHub **обрезает** (93 854 записи из-за
`asn/`) — только пошаговый обход.

---

## 2. Формат `/etc/nikki/mixin.yaml`

Демон пишет и читает **только свою** форму. Третья строка шапки — машинное
состояние; тело порождается из неё.

```yaml
# netmoded: файл пишет панель (раздел «Узлы Nikki» → «Что в туннель · наборы»).
# Не правьте руками — при следующем применении он переписывается целиком.
# netmoded-rulesets: policy=only download=direct sets=youtube,telegram+ip
rule-providers:
  nm-geosite-youtube:
    type: http
    behavior: domain
    format: mrs
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/youtube.mrs
    path: ./rules/nm-geosite-youtube.mrs
    interval: 86400
  nm-geosite-telegram:
    type: http
    behavior: domain
    format: mrs
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/telegram.mrs
    path: ./rules/nm-geosite-telegram.mrs
    interval: 86400
  nm-geoip-telegram:
    type: http
    behavior: ipcidr
    format: mrs
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/telegram.mrs
    path: ./rules/nm-geoip-telegram.mrs
    interval: 86400
nikki-rules:
  - 'RULE-SET,nm-geosite-youtube,BYPASS'
  - 'RULE-SET,nm-geosite-telegram,BYPASS'
  - 'RULE-SET,nm-geoip-telegram,BYPASS,no-resolve'
  - 'GEOIP,PRIVATE,DIRECT,no-resolve'
  - 'MATCH,DIRECT'
```

- `only`: наборы → `BYPASS`, хвост `MATCH,DIRECT`. `except`: наборы → `DIRECT`,
  хвост `MATCH,BYPASS`. `GEOIP,PRIVATE,DIRECT,no-resolve` — всегда перед `MATCH`.
- `download=tunnel`: у каждого провайдера строка `proxy: PROXY`.
- `profile`: файл из двух первых строк шапки плюс `# netmoded-rulesets: policy=profile`,
  флаг `mixin_file_content=0`.
- Правила в одинарных кавычках: `@` и `!` в именах, запятые — так безопаснее для yq.
- Порядок наборов в `sets=` = порядок правил = порядок выбора в панели. Признак
  `+ip` хранится в файле: при чтении обратно каталог не нужен.
- **Чужой файл**: есть непустые строки не-комментарии, а первой строки `# netmoded:`
  нет → `GET` отдаёт `foreign: true`, `PUT` отвечает `409 foreign_mixin`, файл не
  трогается. Комментарный `mixin.yaml` из поставки nikki и отсутствующий файл —
  состояние `profile`.
- **Битый свой файл** (шапка есть, состояние не разбирается или число `RULE-SET,nm-geosite-`
  строк не равно числу наборов) → `GET` отдаёт `500 mixin_corrupt` с путём; `PUT`
  переписывает его (это наш файл, повторное применение — лечение).
- Запись атомарная: temp + fsync + rename, права 0644 (секретов нет). Каталог
  `/etc/nikki` не создаётся: его нет — nikki не разворачивался.

---

## 3. Живой каталог в памяти демона

- Источник — GitHub Git Trees API, пошагово: `GET /repos/MetaCubeX/meta-rules-dat/git/trees/meta`
  → запись `geo` → `GET` её дерева → `GET` деревьев `geosite` и `geoip` (по `url`
  записей). Четыре запроса на холодную загрузку, ~800 КБ JSON (по проводу gzip,
  ~90 КБ). Берутся только `*.mrs`.
- Кэш только в памяти: `Catalog{Commit, ETag, FetchedAt, Names, ip}`. TTL 6 ч.
  Повтор — `If-None-Match` с ETag корневого дерева: `304` не тратит лимит (60/ч без
  токена) и продлевает `FetchedAt`. После TTL старый список отдаётся **сразу**, а
  обновление идёт в фоне (`safe.Do`, ADR-0021), одно за раз.
- Загрузка **ленивая**, по первому обращению панели; при старте демон в сеть не
  ходит (обязан подниматься без интернета). Холодный запрос — 1–3 с, укладывается
  в бюджет побочного списка панели (`T.SIDE` 8 с).
- Лимиты как у `happ.Fetch` (`internal/happ/fetch.go`): таймаут 15 с на запрос,
  потолок тела 4 МБ (больше — отказ, не усечение), только https, за
  перенаправлениями не идём, `User-Agent: netmoded`, `Accept: application/vnd.github+json`.
  `truncated: true` в дереве — отказ «каталог неполный».
- Отказ GitHub при пустом кэше → `503 catalog_unavailable` с причиной; при
  непустом — старый список с `stale: true`.
- Литералы `api.github.com`, `/repos/…/git/trees`, `raw.githubusercontent.com` —
  в `docs/recon/evidence.json` (ссылка на документацию GitHub Trees API и на
  снятые 2026-09-07 числа); `scripts/check-evidence.sh` получает `internal/geosite`
  в `DIRS` и шаблон `*api.github.com*|/repos/*|*raw.githubusercontent.com*`.

### Что именно держится в памяти и зачем

**Дерево в памяти не держится.** Ответы GitHub (4 JSON-документа, ~800 КБ) разбираются
и выбрасываются сразу; остаётся только результат: отсортированный список из 1899
имён, 13 имён с признаком geoip, sha коммита, ETag и время загрузки — около 60 КБ
в куче Go плюс готовое сжатое тело ответа для панели (~6 КБ). Это и есть «каталог».

**Нужен ли кэш вообще.** Строго — нет: панель может ходить в GitHub через демона
при каждом открытии вкладки, а проверку имён при `PUT` можно и не делать — ошибку в
имени всё равно поймает сверка после перезапуска («набор не загрузился»). Кэш нужен
по трём практическим причинам:

1. **Время.** Холодная загрузка — четыре последовательных запроса к `api.github.com`
   с телефона через роутер, 1–3 с; макет обещает поиск «мгновенно». С кэшем вкладка
   открывается за миллисекунды всё время, пока демон жив.
2. **Лимит GitHub.** 60 запросов в час без токена на IP роутера. Без кэша каждое
   открытие вкладки или обновление страницы стоит четырёх; пятнадцать обновлений —
   и каталог «недоступен» на час. С кэшем и `If-None-Match` продление свежести
   стоит одного запроса раз в 6 часов, и тот отвечает `304`, который в лимит не
   входит.
3. **Проверка имён без сети в момент применения.** `PUT` сверяет имена с кэшем и не
   зависит от GitHub ровно тогда, когда владелец жмёт «Применить» ночью с плохим
   аплинком. Опечатка отбивается сразу кодом `unknown_set`, а не после
   пятнадцатисекундного перезапуска туннеля.

Что кэш **не** делает: не пишется на диск (перезапуск демона — снова холодный
старт, и это осознанно: «ничего на флеш»); не обновляется по расписанию (только по
обращению панели: если вкладку не открывают, демон в сеть не ходит); не нужен для
`GET /api/nikki/rulesets` (там всё из файла и Clash API) и не нужен для работы
самих правил (их качает mihomo сам, каталог ему не нужен).

Второй уровень — браузер: `Cache-Control: private, max-age=3600` и ETag, так что
переключение вкладок и обновление страницы в течение часа до демона не доходят вовсе.

---

## 3а. Алгоритм работы, сквозной

**Старт демона.** Ничего нового: `mixin.yaml` не читается, GitHub не опрашивается.
Кэш каталога пуст.

**Открытие вкладки «Что в туннель · наборы» (режим nikki).**
1. Панель зовёт `GET /api/nikki/rulesets`. Демон читает `/etc/nikki/mixin.yaml`:
   файла нет или он комментарный → `policy: profile`; наша шапка → состояние из
   строки `# netmoded-rulesets:`; чужое содержимое → `foreign: true`. Затем
   `GET /providers/rules` у Clash API: для каждого набора — есть ли провайдер
   `nm-geosite-<имя>` (и `nm-geoip-<имя>` при `ip`) и ненулевой `updatedAt`.
   Движок молчит → `live: false`, поля загрузки `null`. Ответ — за десятки мс.
2. Панель зовёт `GET /api/nikki/rulesets/catalog`. Кэш свежий (моложе 6 ч) → ответ
   сразу. Кэш есть, но старше 6 ч → ответ сразу из старого, в фоне один запрос
   корня дерева с `If-None-Match`: `304` → продлить свежесть; новый коммит → ещё три
   запроса, новый список подменяет старый атомарно. Кэша нет → синхронно четыре
   запроса (корень → `geo` → `geosite` + `geoip`), 1–3 с; отказ → `503
   catalog_unavailable`, панель показывает причину и «Повторить», выбранное при этом
   видно из шага 1.
3. Панель держит оба ответа в памяти вкладки; поиск по 1899 строкам — на клиенте.

**Правка черновика.** Политика, «откуда качать», чипы, паки, строки поиска меняют
только `draft` в памяти панели; появляется «Изменений: N». Ни одного запроса к
демону. F5 — черновик пропал, применённое осталось.

**Нажатие «Применить» → `PUT /api/nikki/rulesets` с `If-Match`.** Демон, до любой
записи и в этом порядке:
1. форма тела (политика, скачивание, дубли, `profile` без наборов);
2. текущий файл: чужой → `409 foreign_mixin`; отпечаток не совпал → `409 stale_rulesets`;
3. имена: всё, что не в кэше каталога и не среди уже применённых, — кэша нет →
   попытка загрузить синхронно; не вышло → `503 catalog_unavailable`; вышло, а имени
   нет → `400 unknown_set` с перечнем; признак `ip` проставляется из каталога;
4. `uci changes nikki` непуст → `409 foreign_staged_changes`;
5. движок отвечает, а группы `BYPASS` нет → `503 group_missing` (молчит — пропуск);
6. джоб занят → `409 job_busy`; иначе `202 {job}`; панель засевает полосу и замок.

**Джоб `rulesets` (до 15 с по ETA, замок панели на всё).**
1. Собрать текст файла (§2) и записать атомарно во временный + `rename`. Отказ →
   `failed` «ничего не изменено».
2. Флаг `nikki.mixin.mixin_file_content`: нужен `1` (или `0` для `profile`); читается
   `uci get`, и только если отличается — `uci set` + `uci commit nikki`. Отказ →
   `uci revert nikki.mixin`, `failed` с текстом, действует ли уже файл (если флаг и
   так стоял в `1` — действует).
3. Режим не `nikki` → `done`: записано, вступит при включении Nikki. Перезапускать
   выключенный намеренно движок нельзя.
4. `netmode-apply nikki`: стоп nikki и b4, `firewall restart`, старт nikki. На старте
   `nikki.init` склеивает `main.yml` ⊕ `mixin.yaml` ⊕ UCI в `run/config.yaml`; mihomo
   поднимается, качает `.mrs` в `/etc/nikki/run/rules/` (или через `PROXY` при
   `tunnel`). Отказ скрипта → `failed` тем же текстом, что у смены режима; файл и
   флаг остаются — повтор возможен.
5. `profile` или ноль наборов → `done`.
6. Ожидание Clash API до 20 с (опрос `GET /providers/rules` раз в секунду). Не
   ответил → `failed` «загрузились ли наборы, неизвестно».
7. Сверка: набор загружен, когда все его провайдеры имеют ненулевой `updatedAt`.
   Ни один → `failed` с первыми тремя именами и подсказкой про
   `raw.githubusercontent.com` и `core.log`. Часть → `done`, недогруженные видны в
   `GET` тегом «не загрузился» (mihomo повторит скачивание сам). Все → `done`.

**После джоба.** Панель видит переход `done/failed` в `/api/status`, перечитывает
`GET /api/nikki/rulesets`, показывает тост («Применено · наборов: N» или «Не
загрузились M из N: …»), снимает замок; черновик сброшен к применённому.

**Дальше без участия панели.** mihomo обновляет каждый `.mrs` раз в сутки
(`interval: 86400`) по своему циклу. Перезагрузка роутера: `netmode-apply` при
загрузке стартует nikki, тот снова склеивает `mixin.yaml` — выбор переживает
перезагрузку без демона. Кэш каталога после перезапуска демона пуст до первого
открытия вкладки.

**«Вернуть профилю».** Тот же `PUT` с `policy: profile`: файл из шапки, флаг `0`,
перезапуск; правила снова целиком из `main.yml`.

**Что видно по ssh.** `cat /etc/nikki/mixin.yaml` — весь выбор человекочитаемо;
`uci get nikki.mixin.mixin_file_content` — включён ли он; `grep nm-geosite
/etc/nikki/run/config.yaml` — что реально получил mihomo; `ls /etc/nikki/run/rules/`
— что скачалось.

---

## 4. Контракт API

Три маршрута, в `docs/api/openapi.yaml`; `routes.gen.ts` порождается `make api`
(`nikkiRulesets`, `nikkiRulesetsCatalog`); гейт `scripts/check-routes.sh`.

### `GET /api/nikki/rulesets`

```json
{
  "fingerprint": "sha256:9a1b…",
  "policy": "only",
  "download": "direct",
  "tunnel_group": "BYPASS",
  "sets": [
    {"name": "youtube",  "ip": false, "loaded": true,  "rules": 1284, "updated_at": "2026-09-07T10:00:00Z"},
    {"name": "telegram", "ip": true,  "loaded": false, "rules": 0,    "updated_at": null}
  ],
  "live": true,
  "foreign": false
}
```

- `fingerprint` — от канонической формы (политика, скачивание, наборы с признаком ip);
  `If-Match` на `PUT`, `409 stale_rulesets` при расхождении.
- `loaded`/`rules`/`updated_at` — из `GET /providers/rules`: набор загружен, когда
  загружены **все** его провайдеры (`updatedAt` ненулевое; `ruleCount` не критерий —
  три набора в репозитории пусты); `rules` — сумма. Движок молчит → `live:false` и
  `null` в трёх полях: это ответ «не знаем», не отказ.
- `foreign:true` — файл чужой; `policy` тогда `profile`, `sets` пуст.
- `500 mixin_corrupt` — свой файл не разбирается; `503 uci_unavailable`.

### `PUT /api/nikki/rulesets`

Тело `{"policy": "profile"|"only"|"except", "download": "direct"|"tunnel", "sets": ["youtube", …]}`,
`download` необязателен (умолчание `direct`, при `profile` игнорируется), заголовок
`If-Match`. Ответ `202 {job}` (`kind:"rulesets"`, `arg:""`, `eta_sec:15`).

| Код | HTTP | Когда |
|---|---|---|
| `bad_request` | 400 | политика/`download` вне набора; `profile` с наборами; дубли; тело не JSON |
| `unknown_set` | 400 | имя не найдено в каталоге; имена перечислены |
| `stale_rulesets` | 409 | `If-Match` не совпал |
| `foreign_staged_changes` | 409 | `uci changes nikki` непуст (ADR-0011) |
| `foreign_mixin` | 409 | `/etc/nikki/mixin.yaml` — не наш файл |
| `job_busy` | 409 | идёт другая операция |
| `group_missing` | 503 | движок жив, а группы `BYPASS` в нём нет |
| `catalog_unavailable` | 503 | новых имён нечем проверить: каталог не загружен |
| `uci_unavailable` | 503 | uci не ответил |

Джоб: файл → флаг → перезапуск (только в режиме `nikki`) → сверка. Провал — в
`job.error`; в `last_fail` не пишется (своей таксономии причин нет, как у подписки).

### `GET /api/nikki/rulesets/catalog`

```json
{"commit": "abc123…", "fetched_at": "2026-09-07T10:00:00Z", "stale": false,
 "names": ["0x0", "115", …], "ip": ["cloudflare", "cn", …],
 "packs": [{"id": "social", "sets": ["facebook", "instagram", "twitter", "tiktok", "reddit"]}, …]}
```

`ETag: W/"<commit[:16]>-<unix fetched_at>"`, `Cache-Control: private, max-age=3600`,
gzip по `Accept-Encoding`, `304` на `If-None-Match`. `503 catalog_unavailable`
при пустом кэше. Паки — константы в Go, на выдаче фильтруются по наличию имени.

---

## 5. Панель

Файл `web/panel/src/ui/Rulesets.tsx`; `Engine.tsx` получает вкладки «Узлы» /
«Что в туннель · наборы» внутри карточки Nikki (вкладки рисуются **до** проверок
`starting/down` узлов: наборы доступны и при лежащем движке). Вкладка и черновик —
состояние `App` (сводка заголовка раздела на узком экране обязана знать про
«не применено»).

Состояния: `applied` (ответ `GET`, три значения `undefined/null/объект`), `draft`
(`{policy, download, sets} | null`, null = совпадает с применённым), `catalog`
(побочный список, грузится при первом фокусе поиска или открытии вкладки; при
`503 catalog_unavailable` — строка причины и кнопка «Повторить»; при `stale:true` —
подпись «список от <дата>»), `query`.

Порядок на вкладке (`docs/panel.md` §3, диагноз → действие → параметры → опасное):
1. вводный абзац; строка «Nikki не отвечает: загрузились ли наборы — неизвестно»
   при `live:false`; строка «Файл mixin.yaml не принадлежит панели…» при `foreign`;
2. при `profile`: «Правил панели нет — маршрут решает профиль mihomo…» и
   переключатель политики без положения;
3. переключатель политики («Только выбранное / Всё, кроме выбранного») + подсказка;
4. переключатель «Откуда качать наборы» («Напрямую / Через туннель») + подсказка;
5. подпись «В туннель · N наборов» / «Напрямую, в обход туннеля · N»; чипы выбранного
   (`+ip` меткой, красная рамка у не загрузившихся, ✕); пустое состояние словами макета;
6. «Наборы пакетом» (пять паков); поиск; до восьми строк: выбранное + частые, при
   запросе — совпадения; теги `в туннеле / напрямую / добавлен, не применён / убран,
   не применён / не загрузился`; хвост `geosite:<имя>`; подсказка «Найдено по «…»: N»;
7. блок «Изменений: N» → «Применить» (замок `rulesets`, `poll.sow(202)`) / «Сбросить»;
8. подпись про источник;
9. «Вернуть профилю» — тихая кнопка с встроенным подтверждением (перезапуск Nikki).

После `done` джоба `rulesets` панель перечитывает список и показывает тост:
«Применено · наборов: N» либо warn «Не загрузились M из N: …». `jobText`:
`rulesets` → «Применяю наборы», `failText` → «Наборы не применены».
Черновик живёт в памяти вкладки и теряется при F5 — сознательно.

Мок (`web/mock-server.mjs`): сценарии `rulesets-profile` (начальное),
`rulesets-only` (три набора, `telegram` с `ip:true`, `openai` не загрузился),
`rulesets-foreign`, `rulesets-down` (`live:false`), `rulesets-nocatalog`
(каталог отвечает 503). `PUT` меняет overlay после джоба. Каталог — из golden-примера
`docs/api/examples/nikki-rulesets-catalog.json` с полным списком имён (снят один раз
при написании примера).

---

## 6. Файлы

| Файл | Ответственность |
|---|---|
| `internal/geosite/catalog.go`, `fetch.go` | клиент GitHub Trees, кэш в памяти, `Catalog.Has/HasIP`, `FileURL`, паки |
| `internal/atomicfile/atomicfile.go` | `Write(path, data, perm)` — вынос `writeAtomic` из `internal/subs/update.go:285` |
| `internal/rulesets/rulesets.go` | `Config`, `Render`, `Parse`, `Fingerprint`, `Validate`, `Verify` |
| `internal/nikki/nikki.go` | `RuleProvider`, `Client.RuleProviders` |
| `internal/httpapi/rulesetshandlers.go` | три обработчика, джоб, ожидание Clash API, кэш тела каталога |
| `internal/httpapi/panelfs.go` | вынос `servePrepared`, `preparedFile` |
| `internal/httpapi/server.go` | поля `catalog`, `Config.MixinPath`, `Config.CatalogBaseURL`, маршруты |
| `cmd/netmoded-dev/{main,fakes}.go` | `devNikki.RuleProviders`, проводка имён |
| `web/panel/src/ui/{Rulesets,Engine,App}.tsx`, `api/{types,describe}.ts`, `i18n/*.json`, `styles.css` | панель |
| `web/mock-server.mjs`, `web/README.md` | мок и чек-лист |
| `docs/api/openapi.yaml`, `docs/api/examples/nikki-rulesets-*.json` | контракт, golden-примеры |
| `docs/adr/0039-geosite-rulesets-mixin-file.md`, `docs/contracts/nikki-mixin-rulesets.md`, `docs/contracts/errors.md`, `docs/recon/nikki-mixin.md`, `docs/recon/evidence.json`, `docs/adr/README.md`, `docs/panel.md`, `README.md` | решения и документы |

Новых глаголов `Executor` нет: файл пишет демон сам (как провайдер подписки),
флаг — `UCIGet/UCISet/UCICommit/UCIRevert`, перезапуск — `ApplyMode`.

Ограничения проекта: комментарии и тексты по-русски (почему, а не что); `make verify`
без node (ADR-0035); зависимостей Go нет (`check-stdlib.sh`); откатов нет
(ADR-0006), автопочинки нет (ADR-0010); `UCIRevert` только своё, до `commit`, на
пути отказа (ADR-0028); коммит на задачу с трейлерами сессии.

---

## 8. Не делаем

Свои URL и зеркала; выбор узла-цели по набору; чистые geoip-наборы (страны) без
geosite-пары; правку `main.yml` демоном; импорт чужих UCI-секций; токен GitHub;
кнопку «повторить скачивание набора» (`PUT /providers/rules/{name}`) — пока железо не
покажет, что нужна; подтверждение перед «Применить» (та же цена, что у смены режима).

---

## 11. Снять с роутера до выкладки

```
uci show nikki | grep -v api_secret
grep -n 'nikki-rules\|MIXIN_FILE_PATH' /etc/init.d/nikki /etc/nikki/scripts/include.sh
cat /etc/nikki/mixin.yaml
grep -n -A3 '^rules:' /etc/nikki/run/config.yaml | head; ls -la /etc/nikki/run/rules/
S=$(uci get nikki.mixin.api_secret); curl -s -H "Authorization: Bearer $S" http://127.0.0.1:9090/providers/rules
mihomo -v; apk info nikki | head -3; df -h /overlay
curl -sI https://api.github.com/repos/MetaCubeX/meta-rules-dat/git/trees/meta | head -5
```

Затем: `deploy.sh`; убрать наборы из `main.yml`; в панели `only` + youtube, telegram →
«Применить»; `cat /etc/nikki/mixin.yaml`; `grep -n nm-geosite /etc/nikki/run/config.yaml`;
`/providers/rules` — `nm-geosite-*` и `nm-geoip-telegram` с ненулевым `updatedAt`;
youtube через туннель, ya.ru напрямую; «Вернуть профилю» → флаг 0, файл из шапки,
Nikki работает как до.
