package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wifiStub — копия netmode-wifi с заданным кодом возврата.
//
// Настоящий скрипт, а не подменённый commandRunner: разбор кода возврата
// живёт в *exec.ExitError, и стаб, возвращающий готовую ошибку, проверял бы
// нашу догадку о форме ошибки саму против себя.
func wifiStub(t *testing.T, code int, stderrText string) *Exec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "netmode-wifi")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s' >&2\nexit %d\n", stderrText, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New()
	e.wifiBin = path
	return e
}

// Классы отказа netmode-wifi различаются кодом возврата, а не текстом:
// текст пишется для человека и меняется свободно (ADR-0027).
func TestApplyUpstreamExitCodeBecomesOutcome(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{3, ErrUpstreamBusy},
		{4, ErrUpstreamApply},
		{7, ErrUpstreamPrereq},
	}
	for _, tt := range tests {
		const detail = "ОШИБКА: подробность из stderr"
		e := wifiStub(t, tt.code, detail)

		err := e.ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
		if !errors.Is(err, tt.want) {
			t.Errorf("код %d → %v, ожидался исход %v", tt.code, err, tt.want)
			continue
		}
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("код %d: stderr скрипта не дошёл до текста ошибки: %v", tt.code, err)
		}
		for _, other := range tests {
			if other.want != tt.want && errors.Is(err, other.want) {
				t.Errorf("код %d опознан и как %v", tt.code, other.want)
			}
		}
	}
}

// Две таблицы кодов соседствуют, и числа 3 и 7 в них совпадают намеренно —
// смысл у них один и тот же. Но сентинелы обязаны остаться РАЗНЫМИ: слив их,
// мы получили бы обработчик режима, реагирующий на отказ переключения сети,
// и наоборот.
func TestUpstreamAndApplySentinelsDoNotCross(t *testing.T) {
	pairs := [][2]error{
		{ErrUpstreamBusy, ErrApplyBusy},
		{ErrUpstreamPrereq, ErrApplyPrereq},
		{ErrUpstreamApply, ErrApplyFirewall},
	}
	for _, p := range pairs {
		if errors.Is(p[0], p[1]) || errors.Is(p[1], p[0]) {
			t.Errorf("исходы %v и %v опознаются друг через друга", p[0], p[1])
		}
	}
	// Код 0 сентинела не имеет, и это часть контракта: кода «переключилось»
	// у скрипта нет. Появление здесь чего угодно означало бы, что скрипт
	// начал судить об исходе, а материала для суждения у него нет.
	if _, ok := upstreamSentinels[0]; ok {
		t.Error("у кода 0 появился сентинел: скрипт не имеет права утверждать успех")
	}
	// Кодов 5 и 6 в этой таблице нет: сервисов netmode-wifi не трогает,
	// верификации в нём нет. Дырка в нумерации честнее числа, означающего
	// в двух соседних контрактах разное.
	for _, code := range []int{2, 5, 6} {
		if _, ok := upstreamSentinels[code]; ok {
			t.Errorf("код %d занят, хотя ADR-0027 его не заводит", code)
		}
	}
}

// nil от ApplyUpstream означает «применение выполнено», и НЕ означает
// «переключилось». Здесь проверяется только первая половина; вторая —
// в internal/httpapi (вердикт выносится по ассоциации).
func TestApplyUpstreamSuccessAndUnknownCode(t *testing.T) {
	if err := wifiStub(t, 0, "").ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"}); err != nil {
		t.Errorf("код 0 обязан быть успехом запуска: %v", err)
	}

	// Неизвестному коду смысл не выдумывается: «неизвестный отказ» честнее,
	// чем отнесённый не к тому классу.
	err := wifiStub(t, 9, "что-то новое").ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
	if err == nil {
		t.Fatal("неизвестный код проглочен")
	}
	for _, sentinel := range []error{ErrUpstreamBusy, ErrUpstreamApply, ErrUpstreamPrereq} {
		if errors.Is(err, sentinel) {
			t.Errorf("код 9 отнесён к классу %v", sentinel)
		}
	}
}

// Форма имени радио проверяется ТОЙ ЖЕ, что у скрипта: он принимает только
// `^[a-z][a-z0-9_-]*$`, а на прочее отвечает кодом 1 «баг вызывающего».
// Пропусти мы «Radio0» дальше, владелец увидел бы «баг вызывающего» вместо
// внятного отказа, причём в двух процессах от места, где причина видна.
func TestApplyUpstreamRejectsRadioNameScriptWouldReject(t *testing.T) {
	bad := []string{"", "Radio0", "0radio", "radio.0", "radio 0", "-radio", "@radio", "radio[0]", "радио"}
	for _, name := range bad {
		var got []string
		e := New()
		e.commandRunner = captureRunner(&got, []byte("ok"), nil)
		if err := e.ApplyUpstream(context.Background(), UpstreamTarget{Radio: name}); err == nil {
			t.Errorf("имя радио %q принято", name)
		}
		if len(got) != 0 {
			t.Errorf("имя радио %q дошло до запуска процесса: %v", name, got)
		}
		// Отказ до запуска — обычная ошибка, а не класс исхода скрипта:
		// система не тронута, и говорить «применить не удалось» было бы
		// ложью о состоянии роутера.
		err := e.ApplyUpstream(context.Background(), UpstreamTarget{Radio: name})
		for _, sentinel := range []error{ErrUpstreamBusy, ErrUpstreamApply, ErrUpstreamPrereq} {
			if errors.Is(err, sentinel) {
				t.Errorf("отказ до запуска выдан за исход скрипта %v", sentinel)
			}
		}
	}
	for _, name := range []string{"wlanx", "wlan-x", "wlan_x", "r0"} {
		var got []string
		e := New()
		e.commandRunner = captureRunner(&got, []byte("ok"), nil)
		if err := e.ApplyUpstream(context.Background(), UpstreamTarget{Radio: name}); err != nil {
			t.Errorf("имя радио %q отвергнуто: %v", name, err)
		}
	}
}

func TestExecBuildsUpstreamCommands(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(e *Exec) error
		want []string
	}{
		{
			"apply-upstream",
			func(e *Exec) error { return e.ApplyUpstream(ctx, UpstreamTarget{Radio: "wlanx"}) },
			[]string{"/usr/local/bin/netmode-wifi", "wlanx"},
		},
		{
			"revert секции",
			func(e *Exec) error { return e.UCIRevert(ctx, "wireless", "up_home") },
			[]string{"/sbin/uci", "revert", "wireless.up_home"},
		},
	}
	for _, tt := range tests {
		var got []string
		e := New()
		e.commandRunner = captureRunner(&got, []byte("ok"), nil)
		if err := tt.call(e); err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if strings.Join(got, " ") != strings.Join(tt.want, " ") {
			t.Errorf("%s: команда %v, ожидалась %v", tt.name, got, tt.want)
		}
	}
}

// Граница ADR-0028: revert только ПО СВОЕМУ АДРЕСУ. Пустое имя секции не
// «подразумевает весь пакет» — оно отвергается, иначе снос по пакету
// однажды получат по невнимательности, и он унесёт чужой черновик.
func TestUCIRevertRefusesPackageWideForm(t *testing.T) {
	var got []string
	e := New()
	e.commandRunner = captureRunner(&got, []byte("ok"), nil)

	if err := e.UCIRevert(context.Background(), "wireless", ""); err == nil {
		t.Error("revert без имени секции принят: это снос по пакету")
	}
	if len(got) != 0 {
		t.Errorf("до запуска uci дело дошло: %v", got)
	}

	f := NewFake()
	if err := f.UCIRevert(context.Background(), "wireless", ""); err == nil {
		t.Error("фейк принял revert без имени секции — тесты доказывали бы границу на фейке, который её не держит")
	}
}

// Отмена у фейка настоящая: она вычёркивает наш черновик из `uci show` и из
// стейджинга, но не трогает ни правки других секций, ни чужой черновик.
//
// Без этого тест «отказ записи не оставил мусора» проверял бы только то, что
// в журнал вызовов легла строка со словом revert.
func TestFakeRevertUndoesOnlyOwnSection(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.Staged["wireless"] = "wireless.wifinet0.ssid='чужая правка'\n"

	if err := f.UCISet(ctx, "wireless", "wifinet2", "disabled", "0"); err != nil {
		t.Fatal(err)
	}
	if err := f.UCISet(ctx, "wireless", "up_other", "ssid", "Останется"); err != nil {
		t.Fatal(err)
	}
	if err := f.UCIRevert(ctx, "wireless", "wifinet2"); err != nil {
		t.Fatal(err)
	}

	show, err := f.UCIShow(ctx, "wireless")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(show), "wireless.wifinet2.disabled='0'") {
		t.Error("отменённая правка всё ещё видна в uci show")
	}
	if !strings.Contains(string(show), "wireless.wifinet2.disabled='1'") {
		t.Error("исходное значение не восстановилось")
	}
	if !strings.Contains(string(show), "wireless.up_other.ssid='Останется'") {
		t.Error("revert одной секции унёс правку другой")
	}

	changes, err := f.UCIChanges(ctx, "wireless")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changes), "чужая правка") {
		t.Errorf("revert по своему адресу снёс чужой черновик: %q", changes)
	}
}

// Фейк и реальная реализация обязаны разбирать код возврата ОДНОЙ таблицей:
// будь у фейка своя копия, тест проверял бы её саму против себя.
func TestFakeApplyUpstreamUsesSameTable(t *testing.T) {
	ctx := context.Background()
	for code, want := range map[int]error{3: ErrUpstreamBusy, 4: ErrUpstreamApply, 7: ErrUpstreamPrereq} {
		f := NewFake()
		f.UpstreamExitCode = code
		err := f.ApplyUpstream(ctx, UpstreamTarget{Radio: "wlanx"})
		if !errors.Is(err, want) {
			t.Errorf("код %d → %v, ожидался %v", code, err, want)
		}
		// Попытка обязана быть видна в журнале и при отказе: на роутере
		// скрипт тоже запускается.
		if len(f.CallsContaining("apply-upstream")) != 1 {
			t.Errorf("код %d: запуск скрипта не попал в журнал: %v", code, f.Calls)
		}
	}

	f := NewFake()
	if err := f.ApplyUpstream(ctx, UpstreamTarget{Radio: "wlanx"}); err != nil {
		t.Errorf("код по умолчанию обязан быть успехом запуска: %v", err)
	}
}

// errorForCode обобщён на две таблицы, и обобщение не должно было изменить
// поведение старой: сентинел плюс исходный текст, неизвестный код — как есть.
func TestErrorForCodeKeepsBothLayers(t *testing.T) {
	cause := errors.New("текст скрипта")

	err := errorForCode(applySentinels, 3, cause)
	if !errors.Is(err, ErrApplyBusy) || !errors.Is(err, cause) {
		t.Errorf("потерян слой: %v", err)
	}
	err = errorForCode(upstreamSentinels, 3, cause)
	if !errors.Is(err, ErrUpstreamBusy) || !errors.Is(err, cause) {
		t.Errorf("потерян слой: %v", err)
	}
	if got := errorForCode(upstreamSentinels, 42, cause); got != cause {
		t.Errorf("неизвестный код обёрнут: %v", got)
	}
}

// wifiKilled — скрипт, снимающий сам себя сигналом.
//
// Настоящий сигнал, а не подставленная ошибка: только так *exec.ExitError
// отдаёт ExitCode() == -1, и только этот путь пройдёт демон, когда скрипт
// снимут по дедлайну UpstreamApplyTimeout или OOM-killer'ом. Стаб с готовой
// ошибкой проверял бы нашу догадку о форме исхода саму против себя.
func wifiKilled(t *testing.T) *Exec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "netmode-wifi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New()
	e.wifiBin = path
	return e
}

// Снятый скрипт кода возврата не имеет, и это НЕ «оба глагола отказали».
//
// Отнести его к ErrUpstreamApply значило бы сказать владельцу «конфигурация
// не применялась», хотя `reconf` мог отработать за сотые доли секунды и
// станция уже на целевой сети. Вызывающий обязан пойти и посмотреть на
// ассоциацию — на этом построен весь ADR-0025.
func TestApplyUpstreamKilledScriptHasNoOutcome(t *testing.T) {
	err := wifiKilled(t).ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
	if err == nil {
		t.Fatal("снятый скрипт объявлен успехом")
	}
	if !errors.Is(err, ErrUpstreamUnknown) {
		t.Errorf("исход %v, ожидался ErrUpstreamUnknown", err)
	}
	for _, sentinel := range []error{ErrUpstreamBusy, ErrUpstreamApply, ErrUpstreamPrereq} {
		if errors.Is(err, sentinel) {
			t.Errorf("отсутствие кода возврата выдано за класс отказа %v", sentinel)
		}
	}
}

// Фейк обязан разбирать «кода не было» ТОЙ ЖЕ воронкой: иначе тесты демона
// проверяли бы вторую копию разбора саму против себя.
func TestFakeUnknownOutcomeUsesSameFunnel(t *testing.T) {
	f := NewFake()
	f.UpstreamExitCode = -1
	err := f.ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
	if !errors.Is(err, ErrUpstreamUnknown) {
		t.Errorf("исход %v, ожидался ErrUpstreamUnknown", err)
	}
	if errors.Is(err, ErrUpstreamApply) {
		t.Errorf("фейк отнёс отсутствие кода к отказу применения: %v", err)
	}
	// Попытка видна в журнале и здесь: на роутере скрипт запускался.
	if len(f.CallsContaining("apply-upstream")) != 1 {
		t.Errorf("запуск скрипта не попал в журнал: %v", f.Calls)
	}
}

// Паника фейка не оставляет его заблокированным: внедряется она под замком,
// а снимает его defer вызывающего. Без этого свойства первый же тест на
// панику в теле джоба вешал бы весь пакет по общему таймауту, ничего не
// объясняя.
func TestFakePanicLeavesFakeUsable(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Panics["commit wireless"] = "внезапно"

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("внедрённая паника не сработала")
			}
		}()
		_ = f.UCICommit(ctx, "wireless")
	}()

	// Замок свободен: следующий вызов проходит, а не висит навсегда.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.UCISet(ctx, "wireless", "up_x", "ssid", "После паники")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("фейк остался заблокированным после паники")
	}
	if len(f.CallsContaining("commit wireless")) != 1 {
		t.Errorf("вызов, на котором рвануло, не попал в журнал: %v", f.Calls)
	}
}

// ─────────── исполнитель, которого нет ───────────

// Незапустившийся скрипт — НЕ «оба глагола отказали», и цена этой разницы
// измерена на живом роутере.
//
// Демон фазы 2 приехал без /usr/local/bin/netmode-wifi. Отсутствие файла
// проваливалось в default ветку applyReason и объявлялось apply_failed, чей
// текст обещает владельцу «оба способа применения отказали» — уверенный
// диагноз про механизм, который ни разу не запускался. Роутер при этом
// оставался с ОПУБЛИКОВАННОЙ, но не применённой конфигурацией: uci commit
// прошёл, применить его было нечем, станция висела не подключённой.
//
// Настоящий несуществующий путь, а не подменённый commandRunner: форма
// ошибки (*fs.PathError под *exec.Error) — это то, что отдаёт os/exec, и
// стаб с готовой ошибкой проверял бы нашу догадку о ней саму против себя.
func TestApplyUpstreamMissingScriptIsNoExecutor(t *testing.T) {
	e := New()
	e.wifiBin = filepath.Join(t.TempDir(), "нет-такого-файла")

	err := e.ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
	if err == nil {
		t.Fatal("отсутствующий скрипт объявлен успехом")
	}
	if !errors.Is(err, ErrNoExecutor) {
		t.Errorf("исход %v, ожидался ErrNoExecutor", err)
	}
	// Каждый из трёх утверждал бы про роутер то, чего мы не знаем или что
	// прямо неверно: Apply — что глаголы звали, Prereq — что скрипт доложил
	// о нехватке flock, Unknown — что процесс мог успеть сделать всё.
	for _, sentinel := range []error{ErrUpstreamApply, ErrUpstreamPrereq, ErrUpstreamUnknown} {
		if errors.Is(err, sentinel) {
			t.Errorf("отсутствие исполнителя выдано за класс отказа %v", sentinel)
		}
	}
	// Путь обязан быть в тексте: владелец унесёт эту строку в ssh, и
	// «исполнителя нет» без имени файла ему там не поможет.
	if !strings.Contains(err.Error(), e.wifiBin) {
		t.Errorf("в тексте нет пути к скрипту: %v", err)
	}
}

// Потерянный бит исполнения читается так же, как отсутствие файла: для
// нажатия это одно и то же, и совет владельцу тот же — переустановить пакет.
//
// Проверка идёт под пропуском для root: под ним режим 0644 не мешает
// запуску, и тест доказывал бы обратное тому, что написано.
func TestApplyUpstreamNonExecutableScriptIsNoExecutor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root бит исполнения не проверяется ядром")
	}
	path := filepath.Join(t.TempDir(), "netmode-wifi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	e.wifiBin = path

	err := e.ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
	if !errors.Is(err, ErrNoExecutor) {
		t.Errorf("исход %v, ожидался ErrNoExecutor", err)
	}
}

// Та же воронка закрывает и netmode-apply: проверка стоит в общей
// classifyByExitCode, а не в одном из двух вызывающих.
func TestApplyModeMissingScriptIsNoExecutor(t *testing.T) {
	e := New()
	e.applyBin = filepath.Join(t.TempDir(), "нет-такого-файла")

	err := e.ApplyMode(context.Background(), "nikki")
	if !errors.Is(err, ErrNoExecutor) {
		t.Errorf("исход %v, ожидался ErrNoExecutor", err)
	}
	if errors.Is(err, ErrApplyFirewall) || errors.Is(err, ErrApplyPrereq) {
		t.Errorf("отсутствие исполнителя выдано за класс отказа: %v", err)
	}
}

// Фейк обязан изображать отсутствие исполнителя ТОЙ ЖЕ воронкой: иначе
// тесты демона проверяли бы вторую копию разбора саму против себя.
func TestFakeMissingBinsUseSameFunnel(t *testing.T) {
	f := NewFake()
	f.MissingBins = []string{WifiBinPath}

	err := f.ApplyUpstream(context.Background(), UpstreamTarget{Radio: "wlanx"})
	if !errors.Is(err, ErrNoExecutor) {
		t.Errorf("исход %v, ожидался ErrNoExecutor", err)
	}
	if errors.Is(err, ErrUpstreamApply) {
		t.Errorf("фейк отнёс отсутствие исполнителя к отказу применения: %v", err)
	}
	// Попытка видна в журнале: демон её сделал, и тест обязан это видеть.
	if len(f.CallsContaining("apply-upstream")) != 1 {
		t.Errorf("попытка запуска не попала в журнал: %v", f.Calls)
	}
	// Отсутствие netmode-wifi не делает недоступным netmode-apply: скрипты
	// ставятся вместе, но пропасть может один.
	if err := f.ApplyMode(context.Background(), "nikki"); err != nil {
		t.Errorf("смена режима отказала из-за чужого отсутствующего скрипта: %v", err)
	}
}

// MissingExecutors отвечает на тот же вопрос ДО нажатия.
//
// Три состояния проверяются вместе, потому что важен не только факт, но и
// состав: панель печатает владельцу путь, и «чего-то не хватает» ему нечего
// делать.
func TestMissingExecutorsListsWhatWontRun(t *testing.T) {
	dir := t.TempDir()
	apply := filepath.Join(dir, "netmode-apply")
	wifi := filepath.Join(dir, "netmode-wifi")
	write := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
			t.Fatal(err)
		}
	}

	e := New()
	e.applyBin, e.wifiBin = apply, wifi

	if got := e.MissingExecutors(); len(got) != 2 {
		t.Errorf("оба скрипта отсутствуют, доложено %v", got)
	}

	write(apply, 0o755)
	got := e.MissingExecutors()
	if len(got) != 1 || got[0] != wifi {
		t.Errorf("доложено %v, ожидался ровно %s", got, wifi)
	}

	write(wifi, 0o755)
	if got := e.MissingExecutors(); len(got) != 0 {
		t.Errorf("оба скрипта на месте, а доложено %v", got)
	}

	// Файл без бита исполнения числится отсутствующим наравне с
	// несуществующим: нажатие от этой разницы не выигрывает ничего.
	if os.Geteuid() != 0 {
		if err := os.Chmod(wifi, 0o644); err != nil {
			t.Fatal(err)
		}
		got := e.MissingExecutors()
		if len(got) != 1 || got[0] != wifi {
			t.Errorf("неисполняемый файл не доложен: %v", got)
		}
	}
}

// Порядок фиксирован: строка в журнале и поле в /api/status не имеют права
// переставляться от запуска к запуску, иначе владелец, сверяющий два
// доклада, решит, что состояние изменилось.
func TestMissingExecutorsOrderIsStable(t *testing.T) {
	f := NewFake()
	f.MissingBins = []string{WifiBinPath, ApplyBinPath}
	got := f.MissingExecutors()
	want := []string{ApplyBinPath, WifiBinPath}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("порядок %v, ожидался %v", got, want)
	}
}
