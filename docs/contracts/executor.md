# Контракт: `Executor`

Единственная дверь демона к системе. Всё, что демон умеет сделать с
роутером, перечислено ниже — если действия нет в интерфейсе, демон его не
совершает.

Обоснование формы — [ADR-0015](../adr/0015-executor-shape.md).
Отличия от SPEC §10 — [ADR-0018](../adr/0018-spec-superseded.md).

## Интерфейс

```go
// Package executor — единственная граница между демоном и системой.
//
// Правило: через интерфейс проходят СЫРЫЕ байты. Разбор — чистые функции
// в internal/uci и internal/wireless, они тестируются на записанном выводе
// роутера (docs/recon/raw/) без моков.
package executor

// ErrNotFound возвращается UCIGet, когда опции или секции не существует.
// Отличать её от ошибки выполнения обязательно: отсутствие опции `disabled`
// означает «сеть включена» (ADR-0004), а не сбой.
var ErrNotFound = errors.New("uci: entry not found")

type Executor interface {
	// --- UCI: чтение ---

	// UCIShow возвращает сырой вывод `uci show <pkg>`.
	// Единственный способ перечислить секции. Разбирается uci.ParseShow.
	UCIShow(pkg string) ([]byte, error)

	// UCIGet возвращает одно значение (`uci get pkg.section.option`).
	// Если записи нет — ErrNotFound.
	UCIGet(pkg, section, option string) (string, error)

	// UCIChanges возвращает сырой вывод `uci changes <pkg>`.
	// Пустой результат = стейджинг чист. Непустой = кто-то (LuCI, ssh)
	// держит незакоммиченные правки; писать нельзя (ADR-0011).
	UCIChanges(pkg string) ([]byte, error)

	// --- UCI: запись ---

	// UCIAddNamed создаёт именованную секцию: `uci set pkg.name=sectionType`.
	// Именованную, потому что индексы наружу не выходят (ADR-0005).
	UCIAddNamed(pkg, name, sectionType string) error

	// UCISet записывает одну опцию: `uci set pkg.section.option=value`.
	UCISet(pkg, section, option, value string) error

	// UCIDelete удаляет опцию, а при option == "" — секцию целиком:
	// `uci delete pkg.section[.option]`.
	UCIDelete(pkg, section, option string) error

	// UCICommit публикует стейджинг пакета: `uci commit <pkg>`.
	// Коммитит ВСЁ, что в стейджинге, включая чужое, — поэтому вызову
	// обязана предшествовать проверка UCIChanges (ADR-0011).
	UCICommit(pkg string) error

	// --- Живая система ---

	// UbusCall выполняет `ubus call <object> <method> '<args>'` и возвращает
	// сырой JSON ответа. args == nil эквивалентен `{}`.
	// Имя объекта обязано быть процитировано в docs/recon/evidence.json —
	// проверяет scripts/check-evidence.sh.
	UbusCall(object, method string, args map[string]any) ([]byte, error)

	// --- Именованные внешние действия ---

	// ApplyMode запускает /usr/local/bin/netmode-apply <mode>.
	// Единственное место старта и останова nikki/b4 (SPEC §4, Слой 2).
	// mode ∈ {"nikki","b4","off"}; проверяется до вызова.
	ApplyMode(mode string) ([]byte, error)

	// UpdateSubscription запускает /usr/local/bin/happ2clash.
	// Скрипт держит собственный flock (SPEC §9) — пересечения с ручным
	// запуском из ssh безопасны.
	UpdateSubscription() ([]byte, error)
}
```

Десять методов. Реализация — `execCmd` поверх `os/exec`; фейк —
`map[string][]byte`, заполненная фикстурами из `docs/recon/raw/`.

## Почему каждый метод здесь

| Метод | Кто вызывает | Без него нельзя |
|---|---|---|
| `UCIShow` | классификация выбора, список сетей, отпечаток | перечислить `wifi-iface` — `UCIGet` адресует точечно |
| `UCIGet` | чтение `netmode.main.mode`, `network.lan.ipaddr` | читать одну опцию, отличая «нет записи» от сбоя |
| `UCIChanges` | перед каждой записью в `wireless` | заметить чужой черновик до того, как мы его опубликуем |
| `UCIAddNamed` | `POST /api/wifi/networks` (создание) | создать сеть |
| `UCISet` | создание и правка сети, запись `mode` | что-либо изменить |
| `UCIDelete` | `DELETE /api/wifi/networks/{id}`, очистка опции | удалить сеть |
| `UCICommit` | завершение записи | опубликовать изменения |
| `UbusCall` | статус радио, статус `wwan`, скан, `iwinfo info` | узнать что-либо о живой системе |
| `ApplyMode` | `POST /api/mode` | сменить режим, не обходя `netmode-apply` |
| `UpdateSubscription` | `POST /api/subscription/update`, планировщик | обновить подписку |

## Чего в интерфейсе нет — и почему

| Отсутствует | Причина |
|---|---|
| `Run(cmd, args...)` | универсальный глагол возвращает всё удалённое одной строкой: `tar` для снапшота, `/etc/init.d/b4 restart` в обход `netmode-apply`. Ни один гейт этого не поймает |
| `Service(name, action)` | сервисы стартует только `netmode-apply` (SPEC §4, Слой 2). Метод позволял бы обойти это правило, не нарушив ни одной проверки |
| `SnapshotConfigs` / `RestoreConfigs` | отката нет ([ADR-0006](../adr/0006-no-rollback.md)). Удалены, а не помечены устаревшими: удаление и есть механизм принуждения. Проверяет `scripts/check-no-rollback.sh` |
| `UCIRevert` | откат чужого стейджинга уничтожает чужую работу так же необратимо, как коммит её опубликовал бы ([ADR-0011](../adr/0011-optimistic-concurrency.md)) |
| `WifiScan(device)` | возвращал разобранные структуры — парсер оказывался за моком и переставал тестироваться. Стал `wireless.ParseScan` над `UbusCall` |
| `UCIAddAnonymous` | безымянные секции мы не создаём никогда ([ADR-0005](../adr/0005-section-name-identity.md)) |
| Работа с LED | отдельный интерфейс `led.LED`: пути в sysfs, политика best-effort, ошибки глотаются и не валят джоб. Смешивать с UCI незачем |
| Клиенты b4 и Nikki | отдельные интерфейсы поверх `net/http`. Они не системные вызовы, а сетевые |
| Чтение/запись произвольных файлов | токен читается в `cmd/netmoded` при старте обычным `os.ReadFile`; лог подписки — в `internal/logs`. Общий файловый глагол открыл бы `/etc/b4/b4.json` |

## Чистые функции над результатами

Не входят в интерфейс. Вход — `[]byte`, выход — структура, ошибок ввода-вывода
нет. Тестируются на записанном выводе роутера.

| Функция | Вход | Фикстура |
|---|---|---|
| `uci.ParseShow([]byte) (Config, error)` | `uci show <pkg>` | `raw/10-uci-show-wireless.txt`, `raw/11-uci-show-network.txt` |
| `uci.HasStagedChanges([]byte) bool` | `uci changes <pkg>` | — (пусто/непусто) |
| `wireless.Fingerprint([]byte) string` | `uci show wireless` | `raw/10-…txt` |
| `wireless.Classify(Config) Selection` | результат `ParseShow` | `raw/10-…txt` |
| `wireless.ParseStatus([]byte) (map[string]Radio, error)` | `network.wireless status` | `raw/21-…json` |
| `wireless.ParseScan([]byte) ([]ScanResult, error)` | `iwinfo scan` | **нет, см. ниже** |
| `wireless.ParseInfo([]byte) (Info, error)` | `iwinfo info` | **нет, см. ниже** |
| `netif.ParseInterface([]byte) (IfStatus, error)` | `network.interface.wwan status` | **нет, см. ниже** |

Особая ценность `raw/10-uci-show-wireless.txt` — строка 41:
`wireless.wifinet2.key='REDACTED_PSK!!SPECIAL@@'`. Плейсхолдер сохраняет форму
значения со спецсимволами и проверяет экранирование в парсере `uci show`.
Рукописный мок такой ошибки не поймает, потому что повторяет нашу же догадку.

> **NEEDS RECON — три фикстуры отсутствуют.** Известны только сигнатуры
> вызовов (`raw/22-ubus-v-list-iwinfo.txt`, `raw/20-ubus-list.txt`), но не
> имена полей ответов. `ParseScan`, `ParseInfo` и `ParseInterface` писать
> нельзя до получения вывода. Разрешается тремя чтениями:
>
> ```
> ubus call iwinfo scan '{"device":"phy0.0-sta0"}'
> ubus call iwinfo info '{"device":"phy0.0-sta0"}'
> ubus call network.interface.wwan status
> ```
>
> Имя устройства подставить то, что вернул `network.wireless status`, а не
> литерал (`scripts/check-no-hardcoded-if.sh`).

## Правила вызова

1. **Имя интерфейса не хардкодится.** `phy0.0-sta0` берётся из
   `network.wireless status` → `radio0.interfaces[].ifname`, где в том же
   объекте лежит `section`. Связь секция↔интерфейс дана напрямую
   (`raw/21-ubus-network-wireless-status.json`).
2. **Запись в `wireless` идёт одной последовательностью без внешних вызовов
   между шагами:** `UCIChanges` (пусто) → `UCIAddNamed`/`UCISet`/`UCIDelete` →
   `UCICommit` → `UCIShow` (новый отпечаток).
3. **`disabled` записывается ровно в одном месте** — при создании секции, со
   значением `"1"` ([ADR-0009](../adr/0009-phase1-write-invariant.md)).
4. **Значение `key` не попадает в логи и в тексты ошибок**
   ([ADR-0012](../adr/0012-write-only-credentials.md)). Ошибка `UCISet` для
   опции `key` логируется без значения.
5. **Таймауты.** Все вызовы через `context.Context` с дедлайном: UCI и `ubus`
   status — 3 с, `iwinfo scan` — 15 с (уточнить после RQ-04), `ApplyMode` —
   60 с, `UpdateSubscription` — 120 с. Превышение = ошибка, а не зависший
   обработчик.

## Что обязано быть проверено в тестах

Список из SPEC §10, уточнённый принятыми решениями:

| Сценарий | Как проверяется |
|---|---|
| Параллельные запросы на смену режима | второй получает `409 job_busy`, первый доводится до конца |
| Недоступность API b4 | `/api/b4/*` → `503`, `/api/status` отдаёт `b4.available: false` и **не падает** |
| Недоступность Clash API | то же для `/api/nikki/*` |
| Пустой результат конвертера подписки | джоб `failed`, файл провайдера не перезаписан, строка в логе со `status: "fail"` |
| Все четыре состояния `Selection` | четыре фикстуры `uci show wireless` |
| `409` на правку включённой сети | фикстура `Single` + `POST` по её `id` |
| `409` при несовпадении отпечатка | подменить фикстуру между чтением и записью |
| `409` при чужом стейджинге | непустой вывод `UCIChanges` |
| Экранирование спецсимволов в `key` | `raw/10-uci-show-wireless.txt:41` |
| Отсутствие отката | `scripts/check-no-rollback.sh` в `make checks` |
