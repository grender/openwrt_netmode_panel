package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
