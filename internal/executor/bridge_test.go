package executor

// Тесты глаголов моста (ADR-0030). Проверяется разбор КОДОВ возврата
// netmode-bridge, а не условия их возникновения — условия гоняет
// scripts/check-netmode-bridge.sh живым запуском скрипта в песочнице.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bridgeStub(t *testing.T, code int, stderrText string) *Exec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "netmode-bridge")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s' >&2\nexit %d\n", stderrText, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New()
	e.bridgeBin = path
	return e
}

// Классы отказа различаются кодом, а не текстом — текст пишется для
// человека и меняется свободно (тот же принцип, что ADR-0027).
func TestApplyBridgeExitCodeBecomesOutcome(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{3, ErrBridgeBusy},
		{4, ErrBridgeApply},
		{5, ErrBridgeService},
		{7, ErrBridgePrereq},
		{9, ErrRelaydInstall},
	}
	for _, tt := range tests {
		const detail = "ОШИБКА: подробность из stderr"
		err := bridgeStub(t, tt.code, detail).ApplyBridge(context.Background(), "enable")
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

// Сентинелы моста не пересекаются с соседними таблицами: код 4 у wifi и
// у моста означает похожее, но обработчик моста не имеет права реагировать
// на отказ переключения сети — и наоборот.
func TestBridgeSentinelsDoNotCross(t *testing.T) {
	pairs := [][2]error{
		{ErrBridgeBusy, ErrUpstreamBusy},
		{ErrBridgeApply, ErrUpstreamApply},
		{ErrBridgePrereq, ErrUpstreamPrereq},
		{ErrBridgeApply, ErrApplyFirewall},
		{ErrBridgeService, ErrApplyStart},
	}
	for _, p := range pairs {
		if errors.Is(p[0], p[1]) || errors.Is(p[1], p[0]) {
			t.Errorf("исходы %v и %v опознаются друг через друга", p[0], p[1])
		}
	}
	// Кода 0 в таблице нет: «мост работает» скрипт не утверждает (ADR-0030).
	if _, ok := bridgeSentinels[0]; ok {
		t.Error("у кода 0 появился сентинел: скрипт не имеет права утверждать успех")
	}
	// Код 8 — факт probe, а не исход применения; в таблицу он не входит,
	// его разбирает BridgeProbe отдельно. Появление здесь означало бы, что
	// «адрес свободен» стало ошибкой — ровно та потеря различия, на которой
	// строился живой IP-конфликт.
	if _, ok := bridgeSentinels[8]; ok {
		t.Error("код 8 попал в таблицу исходов: «не ответил» — факт, а не отказ")
	}
}

// probe: три исхода — ответил, тишина, отказ — тремя разными формами.
func TestBridgeProbeThreeOutcomes(t *testing.T) {
	ctx := context.Background()

	answered, err := bridgeStub(t, 0, "").BridgeProbe(ctx, "192.0.2.9", "wdev")
	if err != nil || !answered {
		t.Errorf("код 0: (%v, %v), ожидалось (true, nil)", answered, err)
	}

	answered, err = bridgeStub(t, 8, "").BridgeProbe(ctx, "192.0.2.9", "wdev")
	if err != nil || answered {
		t.Errorf("код 8: (%v, %v), ожидалось (false, nil) — тишина не ошибка", answered, err)
	}

	answered, err = bridgeStub(t, 7, "нет ping").BridgeProbe(ctx, "192.0.2.9", "wdev")
	if !errors.Is(err, ErrBridgePrereq) || answered {
		t.Errorf("код 7: (%v, %v), ожидался исход ErrBridgePrereq", answered, err)
	}
}

// Мусор отвергается ДО запуска процесса: скрипт ответил бы кодом 1
// «баг вызывающего», и владелец увидел бы его в двух процессах от причины.
func TestBridgeRejectsBadArgsBeforeExec(t *testing.T) {
	ctx := context.Background()
	var got []string
	e := New()
	e.commandRunner = captureRunner(&got, []byte("{}"), nil)

	badNames := []string{"", "Eth1", "-eth1", "a..b", "eth 1", "порт"}
	for _, name := range badNames {
		if _, err := e.BridgeStatus(ctx, name); err == nil {
			t.Errorf("имя порта %q принято", name)
		}
		if _, err := e.BridgeProbe(ctx, "192.0.2.9", name); err == nil {
			t.Errorf("имя интерфейса %q принято", name)
		}
	}
	for _, ip := range []string{"", "мусор", "999.1.1.1", "fe80::1", "192.0.2.9:22"} {
		if _, err := e.BridgeProbe(ctx, ip, "wdev"); err == nil {
			t.Errorf("адрес %q принят", ip)
		}
	}
	if err := e.ApplyBridge(ctx, "reboot"); err == nil {
		t.Error("действие reboot принято")
	}
	if len(got) != 0 {
		t.Errorf("мусор дошёл до запуска процесса: %v", got)
	}

	// Точка в имени легитимна: живые интерфейсы называются br-lan.2 и
	// phy0.0-sta0 (raw/82).
	for _, name := range []string{"br-lan.2", "phy0.0-sta0", "eth1"} {
		if _, err := e.BridgeStatus(ctx, name); err != nil {
			t.Errorf("имя %q отвергнуто: %v", name, err)
		}
	}
}

// Списочные операции строят команды uci add_list/del_list и принимают
// значения, которые validateName отверг бы: 'eth1:u*', '!192.168.0.0/24'.
func TestExecBuildsListCommands(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(e *Exec) error
		want []string
	}{
		{
			"add_list ports",
			func(e *Exec) error { return e.UCIAddList(ctx, "network", "vlan2", "ports", "eth1:t") },
			[]string{"/sbin/uci", "add_list", "network.vlan2.ports=eth1:t"},
		},
		{
			"add_list masq_src с отрицанием",
			func(e *Exec) error { return e.UCIAddList(ctx, "firewall", "wanz", "masq_src", "!192.168.0.0/24") },
			[]string{"/sbin/uci", "add_list", "firewall.wanz.masq_src=!192.168.0.0/24"},
		},
		{
			"del_list network",
			func(e *Exec) error { return e.UCIDelList(ctx, "network", "relay", "network", "homelan") },
			[]string{"/sbin/uci", "del_list", "network.relay.network=homelan"},
		},
	}
	for _, tt := range tests {
		var got []string
		e := New()
		e.commandRunner = captureRunner(&got, []byte(""), nil)
		if err := tt.call(e); err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if strings.Join(got, " ") != strings.Join(tt.want, " ") {
			t.Errorf("%s: команда %v, ожидалась %v", tt.name, got, tt.want)
		}
	}

	// Пустой элемент списка отвергается: uci молча создал бы пустое
	// значение, а его потом не адресовать del_list'ом.
	e := New()
	var got []string
	e.commandRunner = captureRunner(&got, []byte(""), nil)
	if err := e.UCIAddList(ctx, "network", "relay", "network", ""); err == nil {
		t.Error("пустой элемент списка принят")
	}
	if len(got) != 0 {
		t.Error("пустой элемент дошёл до uci")
	}
}

// Фейк ходит той же таблицей кодов, что и роутер, — второй копии разбора нет.
func TestFakeBridgeUsesSameTable(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.BridgeExitCodes["apply enable"] = 4
	if err := f.ApplyBridge(ctx, "enable"); !errors.Is(err, ErrBridgeApply) {
		t.Errorf("фейк: код 4 → %v, ожидался ErrBridgeApply", err)
	}
	f.BridgeExitCodes["install"] = 9
	if err := f.InstallRelayd(ctx); !errors.Is(err, ErrRelaydInstall) {
		t.Errorf("фейк: код 9 → %v, ожидался ErrRelaydInstall", err)
	}

	// Проба фейка отвечает картой и не путает тишину с отказом.
	f2 := NewFake()
	f2.BridgeProbeAnswers["192.0.2.7"] = true
	if ans, err := f2.BridgeProbe(ctx, "192.0.2.7", "wdev"); err != nil || !ans {
		t.Errorf("фейк-проба занятого: (%v, %v)", ans, err)
	}
	if ans, err := f2.BridgeProbe(ctx, "192.0.2.8", "wdev"); err != nil || ans {
		t.Errorf("фейк-проба свободного: (%v, %v)", ans, err)
	}

	// Отсутствующий скрипт — ErrNoExecutor через ту же воронку, что на
	// роутере, а не выдуманный текст.
	f3 := NewFake()
	f3.MissingBins = []string{BridgeBinPath}
	if err := f3.ApplyBridge(ctx, "disable"); !errors.Is(err, ErrNoExecutor) {
		t.Errorf("нет скрипта → %v, ожидался ErrNoExecutor", err)
	}
	if got := f3.MissingExecutors(); len(got) != 1 || got[0] != BridgeBinPath {
		t.Errorf("MissingExecutors: %v", got)
	}
}

// Записи списков видны следующему чтению — без этого тест «зона получила
// сеть homelan» проверял бы пустоту.
func TestFakeListOpsReflectInShow(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Fixtures["uci show network"] = []byte("network.relay=interface\n")

	if err := f.UCIAddList(ctx, "network", "relay", "network", "homelan"); err != nil {
		t.Fatal(err)
	}
	if err := f.UCIAddList(ctx, "network", "relay", "network", "wwan"); err != nil {
		t.Fatal(err)
	}
	out, _ := f.UCIShow(ctx, "network")
	if !strings.Contains(string(out), "network.relay.network='homelan' 'wwan'") {
		t.Errorf("список не отражён в uci show:\n%s", out)
	}

	if err := f.UCIDelList(ctx, "network", "relay", "network", "homelan"); err != nil {
		t.Fatal(err)
	}
	out, _ = f.UCIShow(ctx, "network")
	if strings.Contains(string(out), "homelan") || !strings.Contains(string(out), "'wwan'") {
		t.Errorf("del_list убрал не то:\n%s", out)
	}
}
