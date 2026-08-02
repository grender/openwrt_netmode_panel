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

// applyErrorForCode заворачивает причину в сентинел по коду возврата.
//
// Одна таблица на реальную реализацию и на фейк: будь у фейка своя копия,
// тест проверял бы её саму против себя, а не тот разбор, который сработает
// на роутере.
//
// Неизвестный код возвращается как есть: выдумывать ему смысл нельзя —
// «неизвестный отказ» честнее, чем отнесённый не к тому классу.
func applyErrorForCode(code int, cause error) error {
	sentinel, ok := applySentinels[code]
	if !ok {
		return cause
	}
	// Оба слоя оборачиваются: сентинел нужен вызывающему для решения,
	// исходный текст (stderr скрипта) — владельцу для диагностики.
	return fmt.Errorf("%w: %w", sentinel, cause)
}

// Таймауты. Каждый внешний вызов обязан иметь дедлайн: зависший uci
// подвешивает обработчик, который его вызвал, а за ним и опрос статуса.
const (
	UCITimeout          = 3 * time.Second
	ScanTimeout         = 15 * time.Second
	ApplyTimeout        = 60 * time.Second
	SubscriptionTimeout = 120 * time.Second
)

// Лимиты на вывод внешних команд. Дедлайн ограничивает ВРЕМЯ, эти два
// числа — ПАМЯТЬ; без них таймаут лишь задаёт, сколько секунд у команды есть
// на то, чтобы съесть роутер (см. ErrOutputTooLarge).
const (
	// MaxStdout — 1 МиБ, столько же, сколько у буфера сканера в
	// internal/logs (logs.go:143): один и тот же порядок «больше этого
	// на роутере не бывает», и заводить второе число незачем.
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

// Executor — контракт из docs/contracts/executor.md.
type Executor interface {
	UCIShow(ctx context.Context, pkg string) ([]byte, error)
	UCIGet(ctx context.Context, pkg, section, option string) (string, error)
	UCIChanges(ctx context.Context, pkg string) ([]byte, error)

	UCIAddNamed(ctx context.Context, pkg, name, sectionType string) error
	UCISet(ctx context.Context, pkg, section, option, value string) error
	UCIDelete(ctx context.Context, pkg, section, option string) error
	UCICommit(ctx context.Context, pkg string) error

	UbusCall(ctx context.Context, object, method string, args map[string]any) ([]byte, error)

	ApplyMode(ctx context.Context, mode string) error
	UpdateSubscription(ctx context.Context) ([]byte, error)
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
	subscribeBin  string
	commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// New возвращает исполнителя с путями по умолчанию.
func New() *Exec {
	return &Exec{
		uciBin:       "/sbin/uci",
		ubusBin:      "/bin/ubus",
		applyBin:     "/usr/local/bin/netmode-apply",
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

// classifyApplyError переводит код возврата скрипта в исход.
//
// run уже заворачивает *exec.ExitError через %w — код достаётся errors.As
// без переделки run, и в тексте уже лежит stderr скрипта (die_code пишет
// аварийные сообщения именно туда, а run собирает stderr в собственный
// ограниченный буфер и подставляет его в текст ошибки).
//
// Не ExitError — процесс не запустился вовсе (нет файла, права, таймаут
// контекста). Кода возврата не существует, классифицировать нечего.
func classifyApplyError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	return applyErrorForCode(ee.ExitCode(), err)
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
