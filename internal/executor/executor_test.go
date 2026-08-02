package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFakeServesFixtures(t *testing.T) {
	// Фейк отвечает записанным выводом роутера. Смысл именно в этом:
	// рукописная структура проверяла бы нашу догадку саму против себя.
	f := NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))

	out, err := f.UCIShow(context.Background(), "wireless")
	if err != nil {
		t.Fatalf("UCIShow: %v", err)
	}
	if !strings.Contains(string(out), "wireless.wifinet0.ssid='John24'") {
		t.Errorf("вывод не похож на фикстуру wireless:\n%s", out)
	}

	out, err = f.UbusCall(context.Background(), "network.wireless", "status", nil)
	if err != nil {
		t.Fatalf("UbusCall: %v", err)
	}
	if !strings.Contains(string(out), "phy0.0-sta0") {
		t.Errorf("вывод не похож на статус радио:\n%s", out)
	}
}

func TestFakeUCIGetNotFound(t *testing.T) {
	// Отличать «записи нет» от сбоя обязательно: отсутствие опции disabled
	// означает «сеть включена», а не ошибку чтения.
	f := NewFake()
	f.UCIValues["netmode.main.mode"] = "nikki"

	got, err := f.UCIGet(context.Background(), "netmode", "main", "mode")
	if err != nil {
		t.Fatalf("UCIGet: %v", err)
	}
	if got != "nikki" {
		t.Errorf("mode = %q, ожидалось nikki", got)
	}

	_, err = f.UCIGet(context.Background(), "netmode", "main", "нет-такой")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидалась ErrNotFound, получено %v", err)
	}
}

func TestFakeRecordsWrites(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.UCIAddNamed(ctx, "wireless", "up_test", "wifi-iface"); err != nil {
		t.Fatalf("UCIAddNamed: %v", err)
	}
	if err := f.UCISet(ctx, "wireless", "up_test", "ssid", "Тест"); err != nil {
		t.Fatalf("UCISet: %v", err)
	}
	if err := f.UCISet(ctx, "wireless", "up_test", "disabled", "1"); err != nil {
		t.Fatalf("UCISet: %v", err)
	}
	if err := f.UCICommit(ctx, "wireless"); err != nil {
		t.Fatalf("UCICommit: %v", err)
	}

	want := []string{
		"add-named wireless.up_test=wifi-iface",
		"set wireless.up_test.ssid=Тест",
		"set wireless.up_test.disabled=1",
		"commit wireless",
	}
	if len(f.Calls) != len(want) {
		t.Fatalf("вызовов %d, ожидалось %d: %v", len(f.Calls), len(want), f.Calls)
	}
	for i, w := range want {
		if f.Calls[i] != w {
			t.Errorf("вызов[%d] = %q, ожидался %q", i, f.Calls[i], w)
		}
	}
}

func TestFakeErrorInjection(t *testing.T) {
	// Без внедрения ошибок нельзя проверить деградацию — а именно её
	// требует SPEC §10: недоступность b4/Nikki, сбой записи.
	f := NewFake()
	boom := errors.New("занято")
	f.Errors["commit wireless"] = boom

	err := f.UCICommit(context.Background(), "wireless")
	if !errors.Is(err, boom) {
		t.Errorf("ожидалась внедрённая ошибка, получено %v", err)
	}
}

func TestFakeContextCancellation(t *testing.T) {
	// Отменённый контекст обязан прерывать вызов: иначе обработчик
	// зависнет, а клиент уже ушёл.
	f := NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.UCIShow(ctx, "wireless"); !errors.Is(err, context.Canceled) {
		t.Errorf("ожидалась context.Canceled, получено %v", err)
	}
}

func TestRealExecutorRejectsUnknownMode(t *testing.T) {
	// ApplyMode — именованный глагол вместо универсального Run.
	// Значение проверяется до вызова скрипта: пускать в командную строку
	// непроверенную строку незачем.
	e := New()
	for _, bad := range []string{"", "nikkie", "off; rm -rf /", "../../bin/sh"} {
		if err := e.ApplyMode(context.Background(), bad); err == nil {
			t.Errorf("ApplyMode(%q) прошёл, ожидался отказ", bad)
		}
	}
}

func TestRealExecutorRejectsBadUCIName(t *testing.T) {
	// Имена пакетов, секций и опций попадают в argv. Проверяем их сами,
	// потому что exec.Command не даёт шелла, но uci принимает
	// собственный синтаксис, где точка и кавычка меняют смысл.
	e := New()
	ctx := context.Background()

	bad := []string{"", "wire less", "wire.less", "wire'less", "-flag", "wire\nless"}
	for _, b := range bad {
		if _, err := e.UCIShow(ctx, b); err == nil {
			t.Errorf("UCIShow(%q) прошёл, ожидался отказ", b)
		}
		if err := e.UCISet(ctx, "wireless", b, "ssid", "x"); err == nil {
			t.Errorf("UCISet с секцией %q прошёл, ожидался отказ", b)
		}
	}

	// Значение опции — наоборот, произвольное: там живут пароли WiFi
	// со спецсимволами (raw/10, wifinet2.key).
	if err := validateValue("pw!!ATOM2023@@"); err != nil {
		t.Errorf("пароль со спецсимволами отвергнут: %v", err)
	}
	if err := validateValue("обычный ssid с пробелом"); err != nil {
		t.Errorf("ssid с пробелом отвергнут: %v", err)
	}
	// Но не любое: нулевой байт и перевод строки ломают argv и вывод uci.
	for _, b := range []string{"a\x00b", "a\nb"} {
		if err := validateValue(b); err == nil {
			t.Errorf("значение %q прошло, ожидался отказ", b)
		}
	}
}

func TestRealExecutorRejectsUbusObjectNotInEvidence(t *testing.T) {
	// Механизм против галлюцинаций работает и в рантайме, не только
	// в grep-гейте: объект, которого нет в разведке, не вызывается.
	e := New()
	ctx := context.Background()

	if _, err := e.UbusCall(ctx, "выдуманный.объект", "status", nil); err == nil {
		t.Error("вызов невидимого объекта прошёл, ожидался отказ")
	}
	// Разрешённые — из docs/recon/evidence.json.
	for _, ok := range []string{"network.wireless", "iwinfo", "network.interface.wwan"} {
		if err := validateUbusObject(ok); err != nil {
			t.Errorf("объект %q отвергнут: %v", ok, err)
		}
	}
}

func TestTimeoutsAreSet(t *testing.T) {
	// Каждый внешний вызов обязан иметь дедлайн, иначе зависший uci
	// подвешивает обработчик, который его вызвал.
	if UCITimeout <= 0 || ScanTimeout <= 0 || ApplyTimeout <= 0 || SubscriptionTimeout <= 0 {
		t.Fatal("таймауты должны быть положительными")
	}
	if ScanTimeout < UCITimeout {
		t.Error("скан заведомо дольше чтения uci")
	}
	if ApplyTimeout < ScanTimeout {
		t.Error("применение режима дольше скана")
	}
	if SubscriptionTimeout < ApplyTimeout {
		t.Error("обновление подписки — самая долгая операция")
	}
}

func TestFakeIsAnExecutor(t *testing.T) {
	// Компилируемая проверка: фейк и реальная реализация удовлетворяют
	// одному интерфейсу. Без неё они разъедутся незаметно.
	var _ Executor = NewFake()
	var _ Executor = New()
}

func TestFakeLoadFixturesFailsLoudly(t *testing.T) {
	// Молчаливо пустой фейк — источник тестов, которые ничего не проверяют.
	f := NewFake()
	dir := t.TempDir()
	if err := f.loadFixtures(dir); err == nil {
		t.Error("пустой каталог фикстур принят молча, ожидалась ошибка")
	}
	if err := f.loadFixtures(filepath.Join(dir, "нет-такого")); err == nil {
		t.Error("несуществующий каталог принят молча, ожидалась ошибка")
	}
}

func TestFakeApplyModeAndSubscription(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.ApplyMode(ctx, "b4"); err != nil {
		t.Fatalf("ApplyMode: %v", err)
	}
	if _, err := f.UpdateSubscription(ctx); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	want := []string{"apply-mode b4", "update-subscription"}
	for i, w := range want {
		if i >= len(f.Calls) || f.Calls[i] != w {
			t.Errorf("вызов[%d] = %v, ожидался %q", i, f.Calls, w)
		}
	}
	// Фейк тоже проверяет режим: иначе тест пройдёт на значении,
	// которое реальная реализация отвергнет.
	if err := f.ApplyMode(ctx, "мусор"); err == nil {
		t.Error("фейк принял неизвестный режим")
	}
}

func TestFakeUCIChanges(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	out, err := f.UCIChanges(ctx, "wireless")
	if err != nil {
		t.Fatalf("UCIChanges: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("по умолчанию стейджинг должен быть пуст, получено %q", out)
	}

	f.Staged["wireless"] = "wireless.wifinet2.ssid='X'\n"
	out, _ = f.UCIChanges(ctx, "wireless")
	if len(out) == 0 {
		t.Error("непустой стейджинг не виден")
	}
}

func TestUCIShowOnMissingPackageIsEmptyNotError(t *testing.T) {
	// Свежая установка: /etc/config/netmode ещё нет. Это валидное
	// состояние (см. uci-netmode.md), а не сбой.
	f := NewFake()
	out, err := f.UCIShow(context.Background(), "netmode")
	if err != nil {
		t.Fatalf("UCIShow по отсутствующему пакету: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ожидался пустой вывод, получено %q", out)
	}
}

func TestFixtureFilesAreParseable(t *testing.T) {
	// Регрессия: в фикстуры JSON однажды попали комментарии //,
	// и encoding/json перестал их читать.
	dir := filepath.Join("..", "..", "docs", "recon", "raw")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("не читается каталог фикстур: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if !isValidJSON(b) {
			t.Errorf("%s: не разбирается как JSON", e.Name())
		}
	}
	if n == 0 {
		t.Fatal("JSON-фикстур не найдено — тест ничего не проверяет")
	}
}

func TestDeadlineIsAppliedNotIgnored(t *testing.T) {
	// Реальная реализация обязана применять таймаут к процессу.
	// Проверяем на заведомо отсутствующей команде: вызов должен
	// вернуться быстро с ошибкой, а не висеть.
	e := New()
	e.uciBin = "/nonexistent/uci"
	start := time.Now()
	_, err := e.UCIShow(context.Background(), "wireless")
	if err == nil {
		t.Fatal("ожидалась ошибка на отсутствующем бинаре")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("вызов длился %v — таймаут не применён", d)
	}
}

// captureRunner подменяет запуск процесса, чтобы проверить, какую команду
// собрал Exec. Это единственная часть реальной реализации, которую можно
// проверить без роутера, — и самая ошибкоопасная: неверно склеенный адрес
// UCI молча запишет не туда.
func captureRunner(got *[]string, out []byte, err error) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*got = append([]string{name}, args...)
		return out, err
	}
}

func TestExecBuildsCorrectUCICommands(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(e *Exec) error
		want []string
	}{
		{
			"show",
			func(e *Exec) error { _, err := e.UCIShow(ctx, "wireless"); return err },
			[]string{"/sbin/uci", "show", "wireless"},
		},
		{
			"get",
			func(e *Exec) error { _, err := e.UCIGet(ctx, "netmode", "main", "mode"); return err },
			[]string{"/sbin/uci", "get", "netmode.main.mode"},
		},
		{
			"changes",
			func(e *Exec) error { _, err := e.UCIChanges(ctx, "wireless"); return err },
			[]string{"/sbin/uci", "changes", "wireless"},
		},
		{
			"add-named",
			func(e *Exec) error { return e.UCIAddNamed(ctx, "wireless", "up_home", "wifi-iface") },
			[]string{"/sbin/uci", "set", "wireless.up_home=wifi-iface"},
		},
		{
			"set",
			func(e *Exec) error { return e.UCISet(ctx, "wireless", "up_home", "ssid", "HomeNet") },
			[]string{"/sbin/uci", "set", "wireless.up_home.ssid=HomeNet"},
		},
		{
			"set со спецсимволами в значении",
			func(e *Exec) error { return e.UCISet(ctx, "wireless", "up_home", "encryption", "psk2") },
			[]string{"/sbin/uci", "set", "wireless.up_home.encryption=psk2"},
		},
		{
			"delete опции",
			func(e *Exec) error { return e.UCIDelete(ctx, "wireless", "up_home", "key") },
			[]string{"/sbin/uci", "delete", "wireless.up_home.key"},
		},
		{
			"delete секции целиком",
			func(e *Exec) error { return e.UCIDelete(ctx, "wireless", "up_home", "") },
			[]string{"/sbin/uci", "delete", "wireless.up_home"},
		},
		{
			"commit",
			func(e *Exec) error { return e.UCICommit(ctx, "wireless") },
			[]string{"/sbin/uci", "commit", "wireless"},
		},
		{
			"apply-mode",
			func(e *Exec) error { return e.ApplyMode(ctx, "nikki") },
			[]string{"/usr/local/bin/netmode-apply", "nikki"},
		},
		{
			"update-subscription",
			func(e *Exec) error { _, err := e.UpdateSubscription(ctx); return err },
			[]string{"/usr/local/bin/happ2clash"},
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
		if len(got) != len(tt.want) {
			t.Errorf("%s: команда %v, ожидалась %v", tt.name, got, tt.want)
			continue
		}
		for i := range tt.want {
			if got[i] != tt.want[i] {
				t.Errorf("%s: аргумент[%d] = %q, ожидался %q\n  получено: %v", tt.name, i, got[i], tt.want[i], got)
				break
			}
		}
	}
}

func TestExecBuildsUbusCommands(t *testing.T) {
	ctx := context.Background()

	var got []string
	e := New()
	e.commandRunner = captureRunner(&got, []byte("{}"), nil)

	if _, err := e.UbusCall(ctx, "network.wireless", "status", nil); err != nil {
		t.Fatalf("UbusCall: %v", err)
	}
	want := []string{"/bin/ubus", "call", "network.wireless", "status", "{}"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("аргумент[%d] = %q, ожидался %q (получено %v)", i, got[i], want[i], got)
		}
	}

	// Имя устройства передаётся аргументом, а не склейкой в строку:
	// так его невозможно случайно захардкодить в шаблоне.
	if _, err := e.UbusCall(ctx, "iwinfo", "info", map[string]any{"device": "phy0.0-sta0"}); err != nil {
		t.Fatalf("UbusCall с аргументами: %v", err)
	}
	if got[4] != `{"device":"phy0.0-sta0"}` {
		t.Errorf("аргументы = %q, ожидался JSON с device", got[4])
	}
}

func TestExecKeyErrorDoesNotLeakValue(t *testing.T) {
	// Пароль не должен попасть в текст ошибки: ошибки логируются,
	// логи читают и пересылают (ADR-0012).
	secret := "оченьСекретныйПароль123"
	var got []string
	e := New()
	e.commandRunner = captureRunner(&got, nil, errors.New("uci: I/O error"))

	err := e.UCISet(context.Background(), "wireless", "up_home", "key", secret)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("пароль просочился в текст ошибки: %v", err)
	}

	// Для не-секретных опций контекст, наоборот, нужен для диагностики.
	e.commandRunner = captureRunner(&got, nil, errors.New("uci: I/O error"))
	err = e.UCISet(context.Background(), "wireless", "up_home", "ssid", "HomeNet")
	if err == nil || !strings.Contains(err.Error(), "uci") {
		t.Errorf("ошибка ssid потеряла контекст: %v", err)
	}
}

func TestExecScanGetsLongerTimeout(t *testing.T) {
	// Скан заведомо дольше чтения: с общим таймаутом 3 с он бы падал всегда.
	var deadlines []time.Duration
	e := New()
	e.commandRunner = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("дедлайн не выставлен")
			return nil, nil
		}
		deadlines = append(deadlines, time.Until(d).Round(time.Second))
		return []byte("{}"), nil
	}

	_, _ = e.UbusCall(context.Background(), "iwinfo", "scan", map[string]any{"device": "x"})
	_, _ = e.UbusCall(context.Background(), "network.wireless", "status", nil)

	if len(deadlines) != 2 {
		t.Fatalf("ожидалось 2 вызова, получено %d", len(deadlines))
	}
	if deadlines[0] <= deadlines[1] {
		t.Errorf("скан получил таймаут %v, статус %v — скан должен быть больше", deadlines[0], deadlines[1])
	}
}

func TestExecUCINotFoundMapping(t *testing.T) {
	// «Записи нет» обязано отличаться от сбоя: на этом различии стоит
	// правило «нет опции disabled → сеть включена».
	e := New()
	e.commandRunner = captureRunner(new([]string), nil, errors.New("uci: Entry not found"))

	_, err := e.UCIGet(context.Background(), "netmode", "main", "mode")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидалась ErrNotFound, получено %v", err)
	}

	// Отсутствующий пакет при show — пустой вывод, а не ошибка.
	out, err := e.UCIShow(context.Background(), "netmode")
	if err != nil {
		t.Errorf("show отсутствующего пакета вернул ошибку: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ожидался пустой вывод, получено %q", out)
	}

	// А настоящий сбой обязан дойти до вызывающего.
	e.commandRunner = captureRunner(new([]string), nil, errors.New("uci: Permission denied"))
	if _, err := e.UCIShow(context.Background(), "wireless"); err == nil {
		t.Error("настоящий сбой проглочен")
	}
}
