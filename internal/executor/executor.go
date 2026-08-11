// Package executor — единственная дверь демона к системе.
//
// Если действия нет в интерфейсе, демон его не совершает. Универсальных
// глаголов вроде Run(cmd, args...) здесь намеренно нет: один такой метод
// возвращает всё, что было выброшено осознанно — tar для снапшота конфигов,
// перезапуск сервисов в обход netmode-apply, чтение b4.json. Ни один
// grep-гейт этого не поймает, потому что состав аргументов виден только
// в рантайме.
//
// Через интерфейс проходят СЫРЫЕ байты. Разбор — чистые функции в
// internal/uci и internal/wireless: они тестируются на записанном выводе
// живого роутера (docs/recon/raw/) вообще без моков.
//
// Обоснование формы — docs/contracts/executor.md и ADR-0015.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound — записи UCI не существует.
//
// Отличать её от ошибки выполнения обязательно: отсутствие опции `disabled`
// означает «сеть включена» (ADR-0004), а не сбой чтения. Слить их в одну
// ошибку — значит однажды принять живую сеть за поломку.
var ErrNotFound = errors.New("uci: записи нет")

// ErrOutputTooLarge — внешняя команда вылила больше отведённого ей лимита.
//
// Роутер имеет порядка 512 МБ ОЗУ и не имеет свопа: когда память кончается,
// OOM-killer убивает демон целиком, и владелец теряет управление роутером,
// причём без единой записи о причине. Таймауты от этого не спасают —
// зациклившийся happ2clash за отведённые ему 120 секунд успевает налить
// в stdout столько, сколько успеет.
//
// Усечение обязано быть видимым, поэтому оно возвращается ошибкой, а не
// молчаливо обрезанными байтами. Тихое усечение хуже отказа: разбор
// обрезанного JSON даёт загадочную ошибку парсера, и владелец ищет проблему
// в нашем разборе вместо болтливой команды. По той же причине усечённый
// stdout вызывающему не отдаётся вовсе — разбирать нечего.
var ErrOutputTooLarge = errors.New("вывод команды превысил лимит")

// ErrNoExecutor — скрипта слоя 2 нет на роутере или он не исполняем.
//
// Общий на оба скрипта, а не по сентинелу на каждый: различает их путь в
// тексте ошибки, а решение вызывающего от того, какой именно скрипт не
// приехал, не зависит — оба чинятся одной установкой пакета.
//
// Своё имя нужно потому, что это единственный исход, у которого НЕТ кода
// возврата и при этом ЕСТЬ полная определённость. Три соседних случая
// путались с ним ровно до этой строки:
//
//   - ErrUpstreamApply (код 4) — глаголы звали, оба отказали. Здесь их не
//     звал никто: процесс не стартовал;
//   - ErrUpstreamPrereq (код 7) — о нехватке flock или ubus докладывает сам
//     скрипт. Скрипт, которого нет, доложить о себе не может: чтобы вернуть
//     семёрку, надо сначала запуститься;
//   - ErrUpstreamUnknown (код -1) — процесс сняли сигналом, и он мог успеть
//     всё. Незапущенный не успел ничего.
//
// Цена путаницы измерена на живом роутере: демон фазы 2 приехал без
// netmode-wifi, отсутствие файла попадало в default ветку applyReason, и
// панель показывала «оба способа применения отказали» — уверенный диагноз
// про механизм, который ни разу не запускался. Владелец при этом оставался
// с ОПУБЛИКОВАННОЙ, но не применённой конфигурацией: uci commit прошёл,
// применить его было нечем, и станция висела не подключённой, пока он не
// передёрнул радио руками.
//
// Права чинить это демон не получает (ADR-0010) — только право назвать.
var ErrNoExecutor = errors.New("исполнителя нет на роутере")

// Исходы netmode-apply.
//
// Скрипт — единственное место, где режим применяется на живом роутере
// (SPEC §4, слой 2), и его отказы неравноценны. «Занято, повторите» ничего
// не сломало; «firewall не перезапустился» означает, что в nftables могли
// остаться чужие цепочки; «верификация не сошлась» — что система пришла не
// туда, куда просили. Свести их к одной ошибке значит заставить вызывающего
// разбирать текст, а текст скрипта меняется свободно.
//
// Тексты сентинелов короткие намеренно: они всегда печатаются вместе с
// причиной (текстом скрипта) и с пояснением вызывающего, а три развёрнутые
// формулировки одного и того же события в одной строке читать невозможно.
// Развёрнутое объяснение — здесь, в комментарии, и в контракте.
//
// Сентинелы — ДОБАВЛЕНИЕ к интерфейсу, а не изменение его формы: сигнатура
// ApplyMode прежняя (ADR-0015). Демон различает исходы через errors.Is и
// НЕ получает от этого права чинить систему сам (ADR-0010) — только права
// сказать владельцу, что именно случилось.
var (
	// ErrApplyBusy — переключение уже идёт: flock держит другой процесс
	// (вторая копия демона или человек из ssh). Система не тронута.
	ErrApplyBusy = errors.New("netmode-apply: занято")

	// ErrApplyFirewall — firewall не перезапустился ни с первой, ни со
	// второй попытки. Целевой сервис намеренно НЕ запускался: туннель
	// поверх невычищенных цепочек молча пропускал бы трафик мимо себя.
	// Оба сервиса остановлены, трафик идёт напрямую.
	ErrApplyFirewall = errors.New("netmode-apply: firewall не перезапустился")

	// ErrApplyStart — сервис не перешёл в нужное состояние: не поднялся
	// либо не удалось включить его автозапуск. Что именно — в тексте.
	ErrApplyStart = errors.New("netmode-apply: сервис не перешёл в нужное состояние")

	// ErrApplyVerify — проверка после переключения не сошлась: работает
	// не то, что должно.
	ErrApplyVerify = errors.New("netmode-apply: состояние не сошлось")

	// ErrApplyPrereq — на роутере нет предусловия для безопасного
	// переключения (flock). Чинится доставкой пакета, а не повтором.
	ErrApplyPrereq = errors.New("netmode-apply: нет предусловия на роутере")
)

// applySentinels — таблица «код возврата netmode-apply → исход».
//
// Копия таблицы из шапки files/usr/local/bin/netmode-apply; они обязаны
// меняться вместе, поэтому обе описаны в docs/contracts/executor.md.
// Кода 2 в таблице нет: он занят шеллом под misuse встроенных команд, и
// принять опечатку в скрипте за осмысленный отказ было бы хуже, чем не
// узнать причину.
var applySentinels = map[int]error{
	3: ErrApplyBusy,
	4: ErrApplyFirewall,
	5: ErrApplyStart,
	6: ErrApplyVerify,
	7: ErrApplyPrereq,
}

// Исходы netmode-wifi — применение конфигурации беспроводной сети после
// смены upstream (ADR-0027).
//
// Кода «переключилось» здесь НЕТ и не появится. Скрипт видит только код
// глагола, а тот лжёт: `ubus call network.wireless reconf` без
// предварительного `network reload` возвращает 0, не изменив ассоциации
// (замер в ADR-0025). Поэтому nil от ApplyUpstream означает ровно
// «конфигурация передана netifd», а вердикт о переключении выносит демон
// по ассоциации — в internal/httpapi/upstreamhandler.go.
//
// Совпадение чисел 3 и 7 с netmode-apply намеренное: в обоих скриптах они
// означают одно и то же, и человек, читающий logread, не обязан помнить,
// какой скрипт что кодирует. Число 4 значит разное, и потому у него свой
// сентинел, а не переиспользованный ErrApplyFirewall. Кодов 5 и 6 нет:
// сервисов netmode-wifi не трогает, верификации в нём нет.
var (
	// ErrUpstreamBusy — flock держит другой процесс (ssh, cron, применение
	// при загрузке). Система НЕ тронута, повтор осмыслен через секунды.
	ErrUpstreamBusy = errors.New("netmode-wifi: занято")

	// ErrUpstreamApply — оба глагола отказали: конфигурация не применялась
	// вовсе. Записанное лежит в /etc/config/wireless и уедет в живую
	// систему при ближайшем чужом применении.
	ErrUpstreamApply = errors.New("netmode-wifi: применить конфигурацию не удалось")

	// ErrUpstreamPrereq — нет предусловия (flock, ubus). Чинится доставкой
	// пакета, а не повтором.
	ErrUpstreamPrereq = errors.New("netmode-wifi: нет предусловия на роутере")

	// ErrUpstreamUnknown — чем кончился скрипт, мы НЕ УЗНАЛИ.
	//
	// Это не класс отказа, а его отсутствие, и разница здесь дороже, чем
	// кажется. Процесс, снятый сигналом (дедлайн UpstreamApplyTimeout,
	// OOM-killer, kill по ssh), кода возврата не имеет вовсе:
	// ExitError.ExitCode() отдаёт -1, и это не «неизвестный код скрипта», а
	// «кода не было». Отнести такой исход к ErrUpstreamApply значило бы
	// сказать «оба глагола отказали, конфигурация не применялась», хотя
	// `network reload` и `reconf` могли отработать за первые же
	// сотые секунды (применение измерено в 0.01–0.04 с, ADR-0025/raw/27) —
	// то есть станция уже могла уйти на целевую сеть.
	//
	// Вызывающий обязан обращаться с этим исходом ровно так, как со всей
	// фазой: не судить по коду возврата, а посмотреть на ассоциацию. Это
	// единственное место, где код возврата иначе закрывал бы дорогу к
	// верификации, — а на обратном построен весь ADR-0025.
	ErrUpstreamUnknown = errors.New("netmode-wifi: исход применения неизвестен")
)

// upstreamSentinels — таблица «код возврата netmode-wifi → исход».
//
// Мест ПЯТЬ, и они обязаны меняться вместе (ADR-0027):
//
//	files/usr/local/bin/netmode-wifi      шапка скрипта
//	internal/executor/executor.go         этот срез
//	docs/contracts/executor.md            таблица
//	docs/adr/0027-netmode-wifi-exit-codes.md  таблица
//	scripts/check-netmode-wifi.sh         сценарии S1–S13
//
// Синхронность НЕ проверяет никто. У таксономии причин провала сторож есть —
// scripts/check-fail-reasons.sh сверяет пять её источников и валит сборку. У
// ЭТОЙ таблицы такого гварда нет: check-netmode-wifi.sh проверяет коды живым
// запуском скрипта, то есть стережёт пару «скрипт ↔ сценарии», а Go, контракт
// и ADR остаются на дисциплине. Добавляете код — правьте все пять руками.
var upstreamSentinels = map[int]error{
	3: ErrUpstreamBusy,
	4: ErrUpstreamApply,
	7: ErrUpstreamPrereq,
}

// errorForCode заворачивает причину в сентинел по коду возврата.
//
// Таблица параметром, а не зашитая: скриптов с таблицей исходов уже два
// (netmode-apply, netmode-wifi), и вторая копия этой функции разъехалась бы
// с первой в первый же раз, когда правку внесли в одну.
//
// Одна и та же функция работает на реальную реализацию и на фейк: будь у
// фейка своя копия, тест проверял бы её саму против себя, а не тот разбор,
// который сработает на роутере.
//
// Неизвестный код возвращается как есть: выдумывать ему смысл нельзя —
// «неизвестный отказ» честнее, чем отнесённый не к тому классу.
func errorForCode(table map[int]error, code int, cause error) error {
	sentinel, ok := table[code]
	if !ok {
		return cause
	}
	// Оба слоя оборачиваются: сентинел нужен вызывающему для решения,
	// исходный текст (stderr скрипта) — владельцу для диагностики.
	return fmt.Errorf("%w: %w", sentinel, cause)
}

func applyErrorForCode(code int, cause error) error {
	return errorForCode(applySentinels, code, cause)
}

// upstreamErrorForCode — исход netmode-wifi по коду возврата.
//
// Отрицательный код (у *exec.ExitError он означает «процесс снят сигналом»)
// разбирается ОТДЕЛЬНО и до таблицы: кода возврата в этом случае нет вовсе,
// и искать его в таблице значило бы обещать ответ там, где вопрос не был
// задан. См. ErrUpstreamUnknown — там же и цена ошибки.
//
// Живёт рядом с таблицей, а не в classifyUpstreamError, чтобы через ту же
// воронку проходил и фейк: разбор «-1 → исход неизвестен» обязан быть один на
// роутер и на тесты, иначе тест проверял бы вторую копию саму против себя.
func upstreamErrorForCode(code int, cause error) error {
	if code < 0 {
		return fmt.Errorf("%w: %w", ErrUpstreamUnknown, cause)
	}
	return errorForCode(upstreamSentinels, code, cause)
}

// Таймауты. Каждый внешний вызов обязан иметь дедлайн: зависший uci
// подвешивает обработчик, который его вызвал, а за ним и опрос статуса.
const (
	UCITimeout          = 3 * time.Second
	ScanTimeout         = 15 * time.Second
	ApplyTimeout        = 60 * time.Second
	SubscriptionTimeout = 120 * time.Second

	// UpstreamApplyTimeout — потолок на netmode-wifi.
	//
	// Пятнадцать секунд, а не шестьдесят, как у ApplyTimeout, и это не
	// экономия: применение ИЗМЕРЕНО и укладывается в 0.01–0.04 с
	// (ADR-0025, raw/27, шесть применений подряд). Скрипт не ждёт
	// ассоциации внутри себя (ADR-0027) — ждать её будет демон, уже без
	// замка. Значит всё, что длится дольше секунд, — это зависший ubus, и
	// минутный дедлайн лишь прятал бы его на 45 лишних секунд, держа
	// джоб занятым и кнопку заблокированной.
	//
	// Запас против измеренного — примерно 400-кратный, так что срабатывание
	// этого дедлайна означает поломку, а не медленный роутер.
	UpstreamApplyTimeout = 15 * time.Second
)

// Лимиты на вывод внешних команд. Дедлайн ограничивает ВРЕМЯ, эти два
// числа — ПАМЯТЬ; без них таймаут лишь задаёт, сколько секунд у команды есть
// на то, чтобы съесть роутер (см. ErrOutputTooLarge).
const (
	// MaxStdout — 1 МиБ, столько же, сколько у буфера сканера в
	// internal/logs (вызов sc.Buffer при чтении журнала): один и тот же
	// порядок «больше этого на роутере не бывает», и заводить второе число
	// незачем. Номер строки здесь не указан намеренно: он протухает от
	// любой правки выше по файлу, причём молча.
	//
	// Запас против самого объёмного легитимного вывода — стократный.
	// Снятые размеры: iwinfo scan на 14 видимых сетей — 8 989 байт
	// (raw/23-ubus-iwinfo-scan.json, ~640 байт на сеть), ubus call
	// network.wireless status — 1 435 байт (raw/21-…json), uci show
	// wireless — 1 583 байта (raw/10-…txt). Даже в эфире, где видно
	// полторы тысячи точек, скан в мегабайт укладывается.
	MaxStdout = 1024 * 1024

	// MaxStderr — 64 КиБ. Там живут только аварийные сообщения (err() в
	// netmode-apply — одна строка), и это ровно тот объём, который
	// удерживал cmd.Output() в поле ExitError.Stderr: буфер stderr у него
	// был ограничен и раньше, дырой был именно stdout.
	MaxStderr = 64 * 1024
)

// waitDelay — сколько ждать закрытия труб после снятия процесса.
//
// Без него Wait висит, пока трубу держит хоть кто-то: netmode-apply
// запускает init-скрипты, и внук, унаследовавший stdout, пережил бы и
// таймаут, и убитого родителя. Обработчик, ради которого выставлялся
// дедлайн, висел бы вместе с ним.
const waitDelay = 2 * time.Second

// limitedBuffer накапливает не больше limit байт и запоминает факт
// переполнения.
//
// Растёт по мере надобности, а не аллоцируется на лимит сразу: типичный
// вывод uci — десятки байт, и мегабайт под каждый из них был бы ровно той
// тратой памяти, от которой лимит защищает.
//
// Write никогда не возвращает ошибку и всегда отчитывается о полной записи.
// Короткая запись означала бы io.ErrShortWrite в копирующей горутине
// os/exec, та закрыла бы трубу, и процесс умер бы от SIGPIPE — с потерей
// настоящего кода возврата, по которому различаются исходы netmode-apply.
// Лишнее поэтому отбрасывается молча, а факт переполнения читается через
// truncated — уже после Wait, который дожидается копирующих горутин.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
	// onLimit снимает процесс, как только лимит достигнут: дочитывать
	// мусор до конца таймаута незачем, а на роутере это ещё и до двух
	// минут CPU, отнятых у всего остального.
	onLimit   func()
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	switch {
	case len(p) <= room:
		b.buf.Write(p)
	default:
		if room > 0 {
			b.buf.Write(p[:room])
		}
		if !b.truncated {
			b.truncated = true
			if b.onLimit != nil {
				b.onLimit()
			}
		}
	}
	return len(p), nil
}

// UpstreamTarget — чему адресовано применение беспроводной конфигурации.
//
// Структура, а не позиционная строка, и это не украшение. Рядом с именем
// радио (`radio0`) в этом коде всё время ходит имя интерфейса
// (`phy0.0-sta0`) и имя секции (`wifinet2`) — три разные строки, каждая
// подходит по типу. Перепутанные аргументы у ApplyUpstream(ctx, radio) не
// поймал бы ни компилятор, ни ревью, а на роутере это значит `reconf`,
// адресованный несуществующему устройству, — то есть код 4 вместо
// переключения. Именованное поле делает подмену видимой в точке вызова.
//
// Поле одно намеренно: скрипту нужно ровно имя радио (ADR-0025, узкий
// глагол). Второе поле «на будущее» здесь означало бы полномочие, которого
// у скрипта нет.
type UpstreamTarget struct {
	// Radio — имя станционного радио, ВЫВЕДЕННОЕ из системы
	// (wireless.ResolveRadios, ADR-0019), никогда не литерал.
	Radio string
}

// Executor — контракт из docs/contracts/executor.md.
type Executor interface {
	UCIShow(ctx context.Context, pkg string) ([]byte, error)
	UCIGet(ctx context.Context, pkg, section, option string) (string, error)
	UCIChanges(ctx context.Context, pkg string) ([]byte, error)

	UCIAddNamed(ctx context.Context, pkg, name, sectionType string) error
	UCISet(ctx context.Context, pkg, section, option, value string) error
	UCIDelete(ctx context.Context, pkg, section, option string) error
	UCICommit(ctx context.Context, pkg string) error
	UCIRevert(ctx context.Context, pkg, section string) error

	UbusCall(ctx context.Context, object, method string, args map[string]any) ([]byte, error)

	ApplyMode(ctx context.Context, mode string) error
	ApplyUpstream(ctx context.Context, t UpstreamTarget) error
	UpdateSubscription(ctx context.Context) ([]byte, error)

	MissingExecutors() []string
}

// validModes — режимы из SPEC §4. Проверяются до вызова скрипта.
var validModes = map[string]bool{"nikki": true, "b4": true, "off": true}

// allowedUbusObjects — объекты, подтверждённые разведкой
// (docs/recon/evidence.json). Список закрытый: это тот же механизм против
// галлюцинаций, что и check-evidence.sh, только в рантайме. Скрипт ловит
// литерал в исходнике, здесь ловится значение, собранное на лету.
var allowedUbusObjects = map[string]bool{
	"network.wireless":       true,
	"iwinfo":                 true,
	"network.interface.wwan": true,
}

// validateUbusObject проверяет, что объект подтверждён разведкой.
func validateUbusObject(object string) error {
	if !allowedUbusObjects[object] {
		return fmt.Errorf("объект ubus %q не подтверждён разведкой (docs/recon/evidence.json)", object)
	}
	return nil
}

// ErrDeleteDisabled — попытка удалить опцию `disabled` в пакете wireless.
//
// Запрет абсолютный и без исключений (ADR-0026, правило 2 инварианта записи):
// секция БЕЗ `disabled` считается ВКЛЮЧЁННОЙ (ADR-0004), то есть удаление
// опции — это включение станционной сети способом, который в диффе не
// выглядит как включение. Включать разрешено только записью литерала "0" на
// пути переключения upstream.
//
// Отдельный сентинел, а не текст на месте: вызывающему может понадобиться
// отличить нарушение инварианта от отказа uci — первое чинится правкой кода,
// второе не чинится вовсе.
var ErrDeleteDisabled = errors.New(
	`uci delete wireless.*.disabled запрещён (ADR-0026, правило 2): секция без ` +
		`disabled считается включённой (ADR-0004) — включайте записью литерала "0"`)

// forbidDeleteDisabled — рантайм-половина запрета из ADR-0026.
//
// Статическая половина — правило R1 в scripts/check-wireless-write.sh — ловит
// ЛИТЕРАЛ "disabled" в строке вызова. Вычисленное имя опции проходит мимо неё
// в принципе: греп не исполняет код, и `opt := "dis" + "abled"` для него
// обычная строка. Здесь же видно ЗНАЧЕНИЕ, откуда бы оно ни взялось.
//
// Тот же приём, что с allowedUbusObjects: скрипт стережёт написанное,
// рантайм — сделанное. Ни одна из половин не покрывает область другой.
//
// Функция общая для Exec и Fake намеренно: фейк, разрешающий запрещённое,
// учит тесты неправде — они доказывали бы поведение, которого на роутере не
// будет.
func forbidDeleteDisabled(pkg, option string) error {
	if pkg == "wireless" && option == "disabled" {
		return ErrDeleteDisabled
	}
	return nil
}

// ErrDisabledValue — попытка записать в wireless.*.disabled что-то кроме "0"
// и "1" (ADR-0026, правило 1; область значений — ADR-0004).
var ErrDisabledValue = errors.New(
	`uci set wireless.*.disabled принимает только "0" и "1" (ADR-0026, правило 1)`)

// forbidDisabledValue — рантайм-половина правила R2.
//
// Держит ОБЛАСТЬ ЗНАЧЕНИЙ, а не «литеральность»: откуда взялась строка, в
// рантайме не видно в принципе, и притворяться, что видно, было бы хуже
// молчания. Зато `strconv.FormatBool` даёт "true"/"false", а `Sprintf("%d")`
// — что угодно, и оба до uci не доходят: `disabled=true` секцию не выключает,
// её выключает "1" (ADR-0004), а "true" тихо читается как ВКЛЮЧЕНО.
//
// Литеральность записи — предмет грепа (R2, R3, R4) и только его. Это ровно
// тот случай, когда две половины закрывают разное и ни одна не лишняя.
func forbidDisabledValue(pkg, option, value string) error {
	if pkg == "wireless" && option == "disabled" && value != "0" && value != "1" {
		return fmt.Errorf("%w: получено %q", ErrDisabledValue, value)
	}
	return nil
}

// validateName проверяет имя пакета, секции, типа секции или опции UCI.
//
// exec.Command не запускает шелл, поэтому инъекции команд тут нет. Но у uci
// собственный синтаксис: точка разделяет уровни адреса, ведущий дефис
// читается как флаг, а перевод строки ломает разбор нашего же вывода.
//
// Дефис внутри имени разрешён: типы секций так и называются — `wifi-iface`,
// `wifi-device` (raw/10-uci-show-wireless.txt). Запрещён только ведущий,
// потому что именно он превращает аргумент во флаг.
func validateName(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s: пустое имя", kind)
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("%s %q: ведущий дефис читается как флаг", kind, s)
	}
	for _, r := range s {
		ok := r == '_' || r == '-' || r == '@' || r == '[' || r == ']' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("%s %q: недопустимый символ %q", kind, s, r)
		}
	}
	return nil
}

// validateRadioName проверяет имя радио ФОРМОЙ СКРИПТА, а не формой UCI.
//
// netmode-wifi принимает только `^[a-z][a-z0-9_-]*$` и на всё прочее отвечает
// кодом 1 — «баг вызывающего» (ADR-0027). validateName здесь не годится: он
// писался под имена секций UCI и потому шире — пропускает заглавные, ведущую
// цифру и `@`, `[`, `]`. Имя вида `Radio0` прошло бы Go, дошло бы до скрипта
// и вернулось кодом 1, а владелец увидел бы «баг вызывающего» вместо
// внятного отказа — причём причина оказалась бы в двух процессах от места,
// где её видно.
//
// На реальных именах OpenWrt расхождение не стреляет: они этой форме
// удовлетворяют. Это защита в глубину, и её цена — одна функция.
//
// Отказ возвращается ОБЫЧНОЙ ошибкой, а не сентинелом из upstreamSentinels:
// сентинелы описывают исходы запущенного скрипта, а здесь до запуска дело не
// дошло и система не тронута. Наделить этот отказ кодом 4 значило бы сказать
// владельцу «применить не удалось, записанное ждёт чужого применения» —
// утверждение, ложное дважды.
func validateRadioName(s string) error {
	if s == "" {
		return errors.New("имя радио: пустое имя")
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(i > 0 && (r == '_' || r == '-' || (r >= '0' && r <= '9')))
		if !ok {
			return fmt.Errorf("имя радио %q: netmode-wifi принимает только строчные латинские "+
				"буквы, цифры, дефис и подчёркивание, первым символом — букву", s)
		}
	}
	return nil
}

// validateValue проверяет значение опции.
//
// Здесь наоборот — почти всё разрешено: в значениях живут пароли WiFi со
// спецсимволами (raw/10-uci-show-wireless.txt, wifinet2.key) и SSID с
// пробелами и кириллицей. Запрещены только нулевой байт и перевод строки:
// первый обрывает argv, второй ломает построчный разбор `uci show`.
func validateValue(v string) error {
	if strings.ContainsAny(v, "\x00\n\r") {
		return errors.New("значение содержит нулевой байт или перевод строки")
	}
	return nil
}

// --- Реальная реализация ---

// Exec выполняет команды на роутере.
type Exec struct {
	uciBin        string
	ubusBin       string
	applyBin      string
	wifiBin       string
	subscribeBin  string
	commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Пути скриптов слоя 2 — ЭКСПОРТИРОВАННЫЕ константы, а не литералы в New().
//
// Их называет теперь не только запуск: MissingExecutors докладывает их в
// журнал и в /api/status, фейк по ним изображает неустановленный пакет, а
// панель печатает владельцу, что именно доставить. Четыре копии одной
// строки разъехались бы в первой же правке пути, и разъехались бы молча —
// проверять их синхронность нечем.
const (
	ApplyBinPath = "/usr/local/bin/netmode-apply"
	WifiBinPath  = "/usr/local/bin/netmode-wifi"
)

// New возвращает исполнителя с путями по умолчанию.
func New() *Exec {
	return &Exec{
		uciBin:       "/sbin/uci",
		ubusBin:      "/bin/ubus",
		applyBin:     ApplyBinPath,
		wifiBin:      WifiBinPath,
		subscribeBin: "/usr/local/bin/happ2clash",
	}
}

func (e *Exec) run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if e.commandRunner != nil {
		return e.commandRunner(ctx, name, args...)
	}

	// Свои буферы вместо cmd.Output(): тот буферизует stdout неограниченно.
	stdout := &limitedBuffer{limit: MaxStdout, onLimit: cancel}
	stderr := &limitedBuffer{limit: MaxStderr}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	err := cmd.Run()

	// Читать truncated можно только здесь: Run дождался копирующих горутин,
	// и запись флага произошла раньше этого чтения.
	what := filepath.Base(name) + " " + strings.Join(args, " ")

	// Переполнение stdout — отказ, а не предупреждение. Код возврата при
	// нём уже не значит ничего (процесс сняли на полуслове), а отдать
	// обрезанный вывод наверх значит подсунуть парсеру огрызок JSON.
	if stdout.truncated {
		return nil, fmt.Errorf("%s: %w: stdout превысил %d Б, процесс снят, вывод неполон и не разбирается",
			strings.TrimSpace(what), ErrOutputTooLarge, MaxStdout)
	}

	if err != nil {
		// cmd.Output() клал stderr в поле ExitError.Stderr, и текст ошибки
		// строился по нему. Со своими буферами это поле пустое навсегда,
		// поэтому причину берём из своего буфера — иначе диагностика
		// netmode-apply (die_code пишет в stderr) молча опустеет, а тесты
		// на errors.Is продолжали бы проходить.
		//
		// stderr uci и netmode-apply короткий и по делу; значений опций
		// (паролей WiFi) в нём не бывает, поэтому включать его безопасно.
		detail := strings.TrimSpace(stderr.buf.String())
		if stderr.truncated {
			// Обрыв на середине фразы обязан быть виден: иначе усечённое
			// сообщение читается как полное и уводит не туда.
			detail += fmt.Sprintf(" […stderr усечён на %d Б]", MaxStderr)
		}
		if detail != "" {
			return nil, fmt.Errorf("%s: %w: %s", strings.TrimSpace(what), err, detail)
		}
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(what), err)
	}

	// Успех при переполненном stderr отказом не считается: stdout полон и
	// пригоден, а выбрасывать годные данные из-за болтливости в другой
	// поток — это отказ там, где отказывать не за что.
	return stdout.buf.Bytes(), nil
}

func (e *Exec) UCIShow(ctx context.Context, pkg string) ([]byte, error) {
	if err := validateName("пакет", pkg); err != nil {
		return nil, err
	}
	out, err := e.run(ctx, UCITimeout, e.uciBin, "show", pkg)
	if err != nil {
		// Отсутствующий пакет — валидное состояние свежей установки
		// (docs/contracts/uci-netmode.md), а не сбой.
		if isUCINotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (e *Exec) UCIGet(ctx context.Context, pkg, section, option string) (string, error) {
	for kind, s := range map[string]string{"пакет": pkg, "секция": section, "опция": option} {
		if err := validateName(kind, s); err != nil {
			return "", err
		}
	}
	out, err := e.run(ctx, UCITimeout, e.uciBin, "get", pkg+"."+section+"."+option)
	if err != nil {
		if isUCINotFound(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (e *Exec) UCIChanges(ctx context.Context, pkg string) ([]byte, error) {
	if err := validateName("пакет", pkg); err != nil {
		return nil, err
	}
	out, err := e.run(ctx, UCITimeout, e.uciBin, "changes", pkg)
	if err != nil {
		if isUCINotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (e *Exec) UCIAddNamed(ctx context.Context, pkg, name, sectionType string) error {
	for kind, s := range map[string]string{"пакет": pkg, "секция": name, "тип": sectionType} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	_, err := e.run(ctx, UCITimeout, e.uciBin, "set", pkg+"."+name+"="+sectionType)
	return err
}

func (e *Exec) UCISet(ctx context.Context, pkg, section, option, value string) error {
	if err := forbidDisabledValue(pkg, option, value); err != nil {
		return err
	}
	for kind, s := range map[string]string{"пакет": pkg, "секция": section, "опция": option} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	if err := validateValue(value); err != nil {
		return err
	}
	_, err := e.run(ctx, UCITimeout, e.uciBin, "set", pkg+"."+section+"."+option+"="+value)
	if err != nil && option == "key" {
		// Значение пароля не должно попасть в лог через текст ошибки
		// (ADR-0012). Само значение в сообщении uci не появляется, но
		// подстраховываемся: возвращаем ошибку без контекста аргументов.
		return fmt.Errorf("uci set %s.%s.key: запись не удалась", pkg, section)
	}
	return err
}

func (e *Exec) UCIDelete(ctx context.Context, pkg, section, option string) error {
	// Инвариант ДО валидации имён: он не про синтаксис адреса, а про смысл
	// операции, и на невалидном имени секции нарушение осталось бы
	// нарушением.
	if err := forbidDeleteDisabled(pkg, option); err != nil {
		return err
	}
	for kind, s := range map[string]string{"пакет": pkg, "секция": section} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	target := pkg + "." + section
	if option != "" {
		if err := validateName("опция", option); err != nil {
			return err
		}
		target += "." + option
	}
	_, err := e.run(ctx, UCITimeout, e.uciBin, "delete", target)
	return err
}

func (e *Exec) UCICommit(ctx context.Context, pkg string) error {
	if err := validateName("пакет", pkg); err != nil {
		return err
	}
	_, err := e.run(ctx, UCITimeout, e.uciBin, "commit", pkg)
	return err
}

// UCIRevert отменяет НАШ собственный черновик одной секции (ADR-0028).
//
// Это не откат (ADR-0006) и не автопочинка (ADR-0010). Отменяется
// НЕОПУБЛИКОВАННОЕ намерение: черновик по определению не был закоммичен,
// живая конфигурация его не видела, netifd о нём не знает, восстанавливать
// нечего — `uci revert` не возвращает состояние, он вычёркивает то, чего
// ещё не было.
//
// Границы вызова, за пределами которых он запрещён (ADR-0028, «Решение»):
//
//  1. только на пути отказа записи — не при старте, не в фоне, не «заодно»;
//  2. только ДО UCICommit — после коммита это уже возврат опубликованного,
//     то есть откат;
//  3. только по адресу секции, которую записывал этот же запрос.
//
// Отсюда обязательный аргумент section: `uci revert wireless` по пакету
// снёс бы и чужой черновик, попавший в стейджинг в окне между проверкой
// шага 1 openWriteCommon и отказом. Наш адрес известен точно — мы его либо
// сгенерировали, либо получили из разбора `uci show`, — и расширять снос до
// пакета не за чем. Пустое имя секции поэтому отвергается здесь, а не
// «подразумевает весь пакет»: подразумеваемое полномочие однажды получат
// по невнимательности.
func (e *Exec) UCIRevert(ctx context.Context, pkg, section string) error {
	for kind, s := range map[string]string{"пакет": pkg, "секция": section} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	_, err := e.run(ctx, UCITimeout, e.uciBin, "revert", pkg+"."+section)
	return err
}

func (e *Exec) UbusCall(ctx context.Context, object, method string, args map[string]any) ([]byte, error) {
	if err := validateUbusObject(object); err != nil {
		return nil, err
	}
	if err := validateName("метод", method); err != nil {
		return nil, err
	}

	payload := "{}"
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("аргументы ubus: %w", err)
		}
		payload = string(b)
	}

	timeout := UCITimeout
	if object == "iwinfo" && method == "scan" {
		timeout = ScanTimeout
	}
	return e.run(ctx, timeout, e.ubusBin, "call", object, method, payload)
}

func (e *Exec) ApplyMode(ctx context.Context, mode string) error {
	if !validModes[mode] {
		return fmt.Errorf("неизвестный режим %q (допустимы nikki, b4, off)", mode)
	}
	_, err := e.run(ctx, ApplyTimeout, e.applyBin, mode)
	return classifyApplyError(err)
}

// ApplyUpstream просит netifd перечитать беспроводную конфигурацию для
// станционного радио: /usr/local/bin/netmode-wifi <radio>.
//
// nil означает РОВНО «применение выполнено» — глагол принят netifd. Он НЕ
// означает, что станция переключилась: код возврата глагола измеренно лжёт
// (ADR-0025, «мина»), и кода «переключилось» у скрипта нет вовсе
// (ADR-0027). Вердикт выносит вызывающий, читая ассоциацию.
//
// Имя радио проверяется здесь, а не только у вызывающего: пустая строка
// дошла бы до скрипта как отсутствующий аргумент, тот вернул бы код 1
// («баг вызывающего»), и отличить его от настоящего отказа применения
// пришлось бы по тексту.
func (e *Exec) ApplyUpstream(ctx context.Context, t UpstreamTarget) error {
	if err := validateRadioName(t.Radio); err != nil {
		return err
	}
	_, err := e.run(ctx, UpstreamApplyTimeout, e.wifiBin, t.Radio)
	return classifyUpstreamError(err)
}

// MissingExecutors — какие скрипты слоя 2 не приедут в ответ на нажатие.
//
// Существует потому, что пути известны конструктору, а проверить их до сих
// пор было негде: New() прописывал две строки и не смотрел на диск ни разу.
// Демон отдавал панель с кнопками, которые заведомо не могли сработать, и
// узнавал об этом вместе с владельцем — после uci commit, когда намерение
// уже опубликовано и ждёт применения.
//
// Возвращает ПУТИ, а не булево: «чего-то не хватает» владельцу нечего
// делать, а «нет /usr/local/bin/netmode-wifi» он унесёт в ssh как есть.
// Порядок фиксированный (сначала netmode-apply, потом netmode-wifi), чтобы
// строка в журнале и поле в статусе не переставлялись между запусками.
//
// Проверка вызывается на старте и в сборке статуса, то есть раз в
// полсекунды в худшем случае. Это два stat по локальному пути — дешевле,
// чем любой из уже идущих там вызовов ubus.
//
// Неисполняемый файл числится отсутствующим наравне с несуществующим: для
// нажатия это одно и то же, и разделять их значило бы просить владельца
// понять разницу между «нет файла» и «нет бита». Причина при этом не
// теряется — она приедет в тексте ErrNoExecutor при первой же попытке.
func (e *Exec) MissingExecutors() []string {
	var missing []string
	for _, path := range []string{e.applyBin, e.wifiBin} {
		st, err := os.Stat(path)
		if err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			missing = append(missing, path)
		}
	}
	return missing
}

// classifyByExitCode переводит код возврата скрипта в исход.
//
// run уже заворачивает *exec.ExitError через %w — код достаётся errors.As
// без переделки run, и в тексте уже лежит stderr скрипта (die_code пишет
// аварийные сообщения именно туда, а run собирает stderr в собственный
// ограниченный буфер и подставляет его в текст ошибки).
//
// Параметром идёт ФУНКЦИЯ разбора, а не таблица: у netmode-wifi разбор шире
// таблицы — отрицательный код он читает как «исход неизвестен»
// (upstreamErrorForCode), и через ту же функцию обязан проходить фейк.
// Передай сюда таблицу — и у фейка появилась бы вторая, более бедная копия
// разбора, то есть тест, проверяющий не то, что случится на роутере.
//
// Не ExitError — процесс не запустился вовсе: нет файла, нет прав, контекст
// умер до старта. Кода возврата не существует и в этом случае, но разница с
// «сняли на полпути» принципиальная: незапущенный скрипт системы не касался,
// а снятый — мог успеть всё.
//
// Две причины незапуска из трёх называются своим именем (ErrNoExecutor), и
// проверка стоит ПЕРЕД разбором кода намеренно: у *exec.Error кода нет, он
// не *exec.ExitError, и раньше такая ошибка молча уезжала в общий выход
// «как есть» — а там её подбирал default вызывающего и объявлял отказом
// глаголов. Третья причина (умерший контекст) своего имени не получает: она
// про нас, а не про роутер, и лечится не установкой пакета.
func classifyByExitCode(outcome func(code int, cause error) error, err error) error {
	if err == nil {
		return nil
	}
	// Права проверяются наравне с существованием: файл с потерянным битом
	// 0111 не запускается точно так же, и совет владельцу тот же —
	// переустановить пакет, а не повторять нажатие.
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", ErrNoExecutor, err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	return outcome(ee.ExitCode(), err)
}

func classifyApplyError(err error) error {
	return classifyByExitCode(applyErrorForCode, err)
}

func classifyUpstreamError(err error) error {
	return classifyByExitCode(upstreamErrorForCode, err)
}

func (e *Exec) UpdateSubscription(ctx context.Context) ([]byte, error) {
	return e.run(ctx, SubscriptionTimeout, e.subscribeBin)
}

// isUCINotFound отличает «нет такой записи» от настоящего сбоя.
//
// uci возвращает ненулевой код и в том, и в другом случае, различая их
// только текстом на stderr. Строка «Entry not found» — часть его
// пользовательского интерфейса, не приватная деталь.
func isUCINotFound(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Entry not found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "No such file")
}

// isValidJSON — вспомогательная проверка для тестов фикстур.
func isValidJSON(b []byte) bool {
	return json.Valid(b)
}
