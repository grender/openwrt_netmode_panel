# Nikki: склейка `mixin.yaml` и каталог наборов geosite

**Снято с исходников, не с роутера.** 192.168.9.1 во время работы был
недоступен: механика nikki прочитана в upstream
`nikkinikki-org/OpenWrt-nikki` версии **2026.04.08**, механика провайдеров
правил — в mihomo (ветка Alpha, срез 2026-09-07), числа каталога сняты `curl`
**с ноутбука**. Живой пробы не было ни одной.

Читать это как «форма известна», а не «поведение измерено» — статус
`verified_by_source` в
[`evidence.json`](evidence.json) (`geosite_catalog`). Что снять с железа до
выкладки — RQ-07 внизу.

## Как nikki собирает рабочий конфиг

`nikki/files/nikki.init:155-197`. `/etc/nikki/run/config.yaml` собирается **на
старте службы** из трёх документов: профиль (`/etc/nikki/profiles/<имя>.yml`)
⊕ `/etc/nikki/mixin.yaml` — **только если `nikki.mixin.mixin_file_content=1`** —
⊕ вывод `ucode/mixin.uc` из UCI-секций. Слияние делает `yq`:

```
yq eval-all '... | . as $item ireduce ({}; . * $item) | .rules = .nikki-rules + .rules | del(.nikki-rules) | …'
```

Что здесь важно, по пунктам:

- `. * $item` — **глубокое слияние, последний документ побеждает**. Ключи
  `rule-providers` сливаются по имени провайдера: наши `nm-*` встают рядом с
  провайдерами профиля, не затирая их (пока имена не совпадают — отсюда
  приставка `nm-`).
- `.rules = .nikki-rules + .rules` — **наши правила приписываются ПЕРЕД
  правилами профиля**, а не после. Отсюда два следствия: наш `MATCH` закрывает
  правила профиля, не трогая их, и `GEOIP,PRIVATE,DIRECT,no-resolve` обязан
  стоять в нашем блоке перед `MATCH`.
- Сборка происходит только при старте, значит **применить без перезапуска
  нельзя**. Перезапуск у нас один — `netmode-apply nikki`
  ([ADR-0015](../adr/0015-executor-shape.md)).

`ucode/mixin.uc` — альтернативный вход в тот же конфиг: он порождает
`rule-providers` и `rules` из UCI-секций `rule_provider`/`rule` пакета `nikki`.
Мы им **не пользуемся** — доводы в
[ADR-0039](../adr/0039-geosite-rulesets-mixin-file.md) («Рассмотренные
альтернативы»).

`nikki/files/scripts/include.sh:5-12`: `MIXIN_FILE_PATH=/etc/nikki/mixin.yaml`,
`RUN_DIR=/etc/nikki/run`. Оба литерала мы используем: первый — как путь файла
выбора, второй — как корень для `path: ./rules/<имя>.mrs` у провайдеров.

### Пустой `rule-providers:` затирает провайдеров профиля

Прямое следствие `. * $item`: ключ `rule-providers` со значением `null`
(а именно так `yq` читает `rule-providers:` без содержимого) при слиянии
побеждает и **обнуляет** соответствующий ключ предыдущего документа, то есть
семь ручных провайдеров владельца в `main.yml`.

Поэтому при нуле выбранных наборов `Render` рубрику **не пишет вовсе**
(`internal/rulesets/rulesets.go`, ветка `len(c.Sets) > 0`); стережёт
`TestRenderEmptySetsHasNoProviders`.

**Это вывод из формы команды `yq`, а не наблюдение.** Проверить на роутере
обязательно — строка добавлена в список RQ-07.

## mihomo: провайдеры правил

Срез Alpha 2026-09-07.

- `rules/provider/parse.go:51-59` — http-провайдер: без `path` файл кладётся в
  `<home>/rules/<md5(url)>`; с `path: ./rules/x.mrs` — по указанному пути
  относительно рабочего каталога. У владельца провайдеры уже описаны вторым
  способом, наши — тоже.
- `component/resource/fetcher.go:100-111` — **цикл повторного скачивания
  заводится даже тогда, когда первая загрузка не удалась**. То есть провайдер,
  не скачавшийся при старте, докачается сам по своему интервалу (у нас
  `interval: 86400`).
- `hub/executor/executor.go:318-336` — **неудачная начальная загрузка
  провайдера не валит старт движка**. Провайдер остаётся пустым и молча не
  матчит ничего. Отсюда прямое требование к нашей сверке: «mihomo поднялся» не
  значит «наборы работают», и вердикт нужно брать у Clash API, а не у кода
  возврата `netmode-apply`.
- `hub/route/provider.go:113-138` вместе с `rules/provider/provider.go:107-118`
  — `GET /providers/rules` отдаёт
  `{providers: {<имя>: {name, type, vehicleType, behavior, format, ruleCount,
  updatedAt}}}`. **Нулевое `updatedAt` = ни разу не скачан**; это и есть наш
  критерий загруженности. `ruleCount` критерием быть не может: в репозитории
  есть пустые наборы.
- `mihomo -t` провайдеров не качает — проверить конфиг заранее и по результату
  судить о наборах нельзя.

## Каталог `MetaCubeX/meta-rules-dat`, ветка `meta`

Замер 2026-09-07, `curl` **с ноутбука**, без токена.

| | |
|---|---|
| `geo/geosite/` | 5698 файлов, **1899** имён в `.mrs` |
| `geo/geoip/` | **260** `.mrs` |
| Пересечение | **13** имён: `telegram`, `google`, `cloudflare`, `facebook`, `netflix`, `twitter`, `fastly`, `tor`, `cn`, `private`, `hm`, `sb`, `st` |
| Алфавит имён | строчная латиница, цифры, `-`, `@`, `!` (`category-ai-!cn`, `netflix@ads`) |
| Размер | ~800 КБ JSON на четыре дерева (по проводу gzip ~90 КБ); медианный `.mrs` — 115 байт, `cn` — 538 КБ |

Каждое имя лежит тремя файлами (`.mrs`, `.yaml`, `.list`) — отсюда 5698 против
1899. Считаются только `.mrs`: их читает mihomo.

**Рекурсивный обход непригоден.** `GET …/git/trees/meta?recursive=1` отвечает
`truncated: true` примерно на **93 854** записях — из-за каталога `asn/`.
Дерево `geosite` в такой ответ не помещается вовсе, поэтому обход только
пошаговый: корень ветки → запись `geo` → деревья `geosite` и `geoip` по их
`url`. Четыре запроса на холодную загрузку.

**Лимит без токена — 60 запросов в час на IP.** Ответ `304` на запрос с
`If-None-Match` в лимит **не входит** — на этом построено продление свежести
снимка раз в шесть часов. Токен GitHub мы не заводим
([спека](../superpowers/specs/2026-09-07-geosite-rulesets-design.md) §8).

Литералы (`https://api.github.com`,
`/repos/MetaCubeX/meta-rules-dat/git/trees/meta`,
`https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/`) лежат в
[`evidence.json`](evidence.json) → `geosite_catalog` и сторожатся
`scripts/check-evidence.sh` (каталог `internal/geosite` добавлен в `DIRS`
2026-09-07).

**Чего этот замер не доказывает:** доходит ли до `api.github.com` аплинк
роутера и укладывается ли холодная загрузка в обещанные 1–3 с **с роутера**.
Ноутбук стоит в той же сети, но не за тем же маршрутом.

## RQ-07 — снять с роутера до выкладки

Открыт. Команды — дословно из спеки §11:

```
uci show nikki | grep -v api_secret
grep -n 'nikki-rules\|MIXIN_FILE_PATH' /etc/init.d/nikki /etc/nikki/scripts/include.sh
cat /etc/nikki/mixin.yaml
grep -n -A3 '^rules:' /etc/nikki/run/config.yaml | head; ls -la /etc/nikki/run/rules/
S=$(uci get nikki.mixin.api_secret); curl -s -H "Authorization: Bearer $S" http://127.0.0.1:9090/providers/rules
mihomo -v; apk info nikki | head -3; df -h /overlay
curl -sI https://api.github.com/repos/MetaCubeX/meta-rules-dat/git/trees/meta | head -5
```

Затем: `deploy.sh`; убрать наборы из `main.yml`; в панели `only` + youtube,
telegram → «Применить»; `cat /etc/nikki/mixin.yaml`;
`grep -n nm-geosite /etc/nikki/run/config.yaml`; `/providers/rules` —
`nm-geosite-*` и `nm-geoip-telegram` с ненулевым `updatedAt`; youtube через
туннель, ya.ru напрямую; «Вернуть профилю» → флаг 0, файл из шапки, Nikki
работает как до.

Сверх спеки, **отдельной строкой**: проверить, что документ `mixin.yaml`
**без ключа `rule-providers`** действительно оставляет провайдеров `main.yml`
на месте, а документ с пустым `rule-providers:` их обнуляет. Это единственное
утверждение о склейке, выведенное из формы команды `yq`, а не прочитанное как
факт; проверяется применением политики `only` с нулём наборов и последующим
`grep -n -A3 'rule-providers' /etc/nikki/run/config.yaml`.

Что закроет RQ-07: версия mihomo и версия пакета nikki на роутере, реальные
правила профиля, живой ответ `/providers/rules`, время холодной загрузки
каталога с роутера. После первой живой пробы запись `geosite_catalog` в
`evidence.json` обязана подняться до `verified` с приложенным `raw/`.
