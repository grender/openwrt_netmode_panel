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
	if UCITimeout <= 0 || ScanTimeout <= 0 || ApplyTimeout <= 0 {
		t.Fatal("таймауты должны быть положительными")
	}
	if ScanTimeout < UCITimeout {
		t.Error("скан заведомо дольше чтения uci")
	}
	if ApplyTimeout < ScanTimeout {
		t.Error("применение режима дольше скана")
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

func TestFakeApplyMode(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.ApplyMode(ctx, "b4"); err != nil {
		t.Fatalf("ApplyMode: %v", err)
	}
	want := []string{"apply-mode b4"}
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

// applyStub подменяет netmode-apply скриптом, который пишет сообщение
// в stderr и завершается заданным кодом.
//
// Настоящий процесс, а не подставной commandRunner: проверяется весь путь
// целиком — exec.ExitError → код возврата → исход. Подмена runner'а обошла
// бы ровно то место, где ошибиться легче всего.
func applyStub(t *testing.T, code int, stderrText string) *Exec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "netmode-apply")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s' >&2\nexit %d\n", stderrText, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New()
	e.applyBin = path
	return e
}

// Демон обязан отличать «занято, повторите» от «переключение провалилось»
// и от «состояние не сошлось»: реакция на них разная вплоть до индикации.
// Раньше все причины сводились к exit 1, и различать их пришлось бы
// разбором текста — а текст скрипта меняется свободно.
func TestApplyModeExitCodeBecomesOutcome(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{3, ErrApplyBusy},
		{4, ErrApplyFirewall},
		{5, ErrApplyStart},
		{6, ErrApplyVerify},
		{7, ErrApplyPrereq},
	}
	for _, tt := range tests {
		const detail = "ОШИБКА: подробность из stderr"
		e := applyStub(t, tt.code, detail)

		err := e.ApplyMode(context.Background(), "nikki")
		if !errors.Is(err, tt.want) {
			t.Errorf("код %d → %v, ожидался исход %v", tt.code, err, tt.want)
			continue
		}
		// Аварийные сообщения скрипта уходят в stderr именно ради этого:
		// cmd.Output() кладёт stdout в результат, а в текст ошибки отдаёт
		// только stderr. Пиши die_code в stdout — здесь была бы пустота.
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("код %d: stderr скрипта не дошёл до текста ошибки: %v", tt.code, err)
		}
		// Исходы не должны склеиваться между собой.
		for _, other := range tests {
			if other.want != tt.want && errors.Is(err, other.want) {
				t.Errorf("код %d опознан и как %v", tt.code, other.want)
			}
		}
	}
}

func TestApplyModeSuccessAndUnknownExitCodes(t *testing.T) {
	sentinels := []error{ErrApplyBusy, ErrApplyFirewall, ErrApplyStart, ErrApplyVerify, ErrApplyPrereq}

	// Ноль — успех, никаких исходов.
	if err := applyStub(t, 0, "применён").ApplyMode(context.Background(), "off"); err != nil {
		t.Errorf("успешный скрипт вернул ошибку: %v", err)
	}

	// Код вне контракта смыслом не наделяется: «неизвестный отказ» честнее,
	// чем отнесённый не к тому классу. Но ошибкой он остаётся.
	err := applyStub(t, 9, "что-то новое").ApplyMode(context.Background(), "nikki")
	if err == nil {
		t.Fatal("код 9 проглочен, ожидалась ошибка")
	}
	for _, s := range sentinels {
		if errors.Is(err, s) {
			t.Errorf("коду 9 приписан исход %v", s)
		}
	}

	// Скрипта нет вовсе: кода возврата не существует, классифицировать
	// нечего — но и молчать нельзя.
	e := New()
	e.applyBin = filepath.Join(t.TempDir(), "нет-такого")
	err = e.ApplyMode(context.Background(), "b4")
	if err == nil {
		t.Fatal("отсутствие скрипта проглочено")
	}
	for _, s := range sentinels {
		if errors.Is(err, s) {
			t.Errorf("отсутствию скрипта приписан исход %v", s)
		}
	}
}

// Фейк обязан отдавать те же исходы, что и роутер: тест, различающий
// поведение демона, иначе проверял бы выдуманную ошибку вместо кода.
func TestFakeApplyModeExitCodes(t *testing.T) {
	for code, want := range map[int]error{
		3: ErrApplyBusy,
		4: ErrApplyFirewall,
		5: ErrApplyStart,
		6: ErrApplyVerify,
		7: ErrApplyPrereq,
	} {
		f := NewFake()
		f.ApplyExitCodes["nikki"] = code

		err := f.ApplyMode(context.Background(), "nikki")
		if !errors.Is(err, want) {
			t.Errorf("фейк: код %d → %v, ожидался исход %v", code, err, want)
		}
		// Попытка всё равно была: на роутере скрипт запускается и падает
		// уже внутри, а не «не вызывался».
		if len(f.CallsContaining("apply-mode nikki")) != 1 {
			t.Errorf("фейк: код %d не записал попытку: %v", code, f.Calls)
		}
		// Другой режим отказом не задет.
		if err := f.ApplyMode(context.Background(), "off"); err != nil {
			t.Errorf("фейк: отказ nikki задел режим off: %v", err)
		}
	}
}

// stubBin кладёт во временный каталог исполняемый sh-скрипт с заданным телом
// и возвращает путь к нему.
//
// Настоящий процесс, а не подставной commandRunner: лимиты применяются
// только на реальном пути, и проверять их через подмену runner'а значило бы
// проверять пустое место.
func stubBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Болтливая команда не должна уносить с собой демон. На роутере нет свопа:
// неограниченный буфер стоит не деградации, а OOM-kill всего процесса —
// и владелец теряет управление роутером без записи о причине.
func TestRunRefusesOutputOverLimit(t *testing.T) {
	// ~2 МиБ за 2048 итераций встроенного printf: два лимита подряд,
	// без fork'ов на каждую строку.
	//
	// Глагол здесь — ubus со сканом: нужен вызов, который ОТДАЁТ stdout
	// (проверяем, что усечённые байты не уходят наверх) и имеет дедлайн
	// заметно длиннее самого теста (проверяем, что процесс сняли на лимите,
	// а не дождались таймаута). Раньше эту роль играл happ2clash со своими
	// 120 секундами; скрипта нет, ScanTimeout в 15 с даёт тот же запас.
	e := New()
	e.ubusBin = stubBin(t, `
line=$(awk 'BEGIN{s="";while(length(s)<1023)s=s "x";print s}')
i=0
while [ $i -lt 2048 ]; do printf '%s\n' "$line"; i=$((i+1)); done
`)

	start := time.Now()
	out, err := e.UbusCall(context.Background(), "iwinfo", "scan", nil)
	if err == nil {
		t.Fatal("вывод сверх лимита принят молча — это и есть OOM в проде")
	}
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("ожидался ErrOutputTooLarge, получено %v", err)
	}
	// Усечённые байты наверх не уходят: разбор огрызка JSON дал бы
	// загадочную ошибку парсера, и причину искали бы не там.
	if len(out) != 0 {
		t.Errorf("отдано %d Б усечённого вывода, ожидался пустой результат", len(out))
	}
	// Усечение обязано быть видимым в журнале, а не только в типе ошибки.
	if !strings.Contains(err.Error(), "неполон") {
		t.Errorf("в тексте ошибки не сказано, что вывод неполон: %v", err)
	}
	// Процесс снимается на лимите, а не дочитывается до конца таймаута
	// (у скана он ScanTimeout).
	if d := time.Since(start); d > ScanTimeout/2 {
		t.Errorf("вызов длился %v — процесс не сняли на лимите", d)
	}
}

// Ловушка перехода со cmd.Output(): тот сам заводил буфер под stderr и клал
// его в ExitError.Stderr, откуда текст ошибки и брался. Со своими буферами
// это поле пустое навсегда, и диагностика молча опустела бы — при том, что
// errors.As по коду возврата продолжал бы проходить.
func TestRunStderrReachesErrorText(t *testing.T) {
	const detail = "ОШИБКА: нет flock — цепочка «скрипт → stderr → текст ошибки»"

	// Путь uci: stderr нужен ещё и для того, чтобы отличить «записи нет»
	// от настоящего сбоя.
	e := New()
	e.uciBin = stubBin(t, "printf '%s\\n' '"+detail+"' >&2\nexit 1\n")
	_, err := e.UCIChanges(context.Background(), "wireless")
	if err == nil {
		t.Fatal("ненулевой код проглочен")
	}
	if !strings.Contains(err.Error(), detail) {
		t.Errorf("stderr не дошёл до текста ошибки: %v", err)
	}

	// Тот же путь для netmode-apply: die_code пишет причину в stderr
	// (files/usr/local/bin/netmode-apply, err()), и это единственное, что
	// владелец увидит о причине отказа.
	e2 := New()
	e2.applyBin = stubBin(t, "printf '%s\\n' '"+detail+"' >&2\nexit 4\n")
	err = e2.ApplyMode(context.Background(), "nikki")
	if !errors.Is(err, ErrApplyFirewall) {
		t.Fatalf("код 4 не опознан: %v", err)
	}
	if !strings.Contains(err.Error(), detail) {
		t.Errorf("stderr скрипта не дошёл до текста ошибки: %v", err)
	}

	// stdout в текст ошибки не подмешивается: обычные шаги скрипт пишет
	// туда же, и они бы забили причину.
	e3 := New()
	e3.applyBin = stubBin(t, "printf 'обычный шаг\\n'\nprintf '%s\\n' '"+detail+"' >&2\nexit 5\n")
	err = e3.ApplyMode(context.Background(), "b4")
	if strings.Contains(err.Error(), "обычный шаг") {
		t.Errorf("stdout попал в текст ошибки: %v", err)
	}
}

// Усечённый stderr тоже обязан быть помечен: обрыв на середине фразы иначе
// читается как полное сообщение и уводит не туда.
func TestRunMarksTruncatedStderr(t *testing.T) {
	e := New()
	e.applyBin = stubBin(t, `
line=$(awk 'BEGIN{s="";while(length(s)<1023)s=s "x";print s}')
i=0
while [ $i -lt 128 ]; do printf '%s\n' "$line" >&2; i=$((i+1)); done
exit 6
`)
	err := e.ApplyMode(context.Background(), "nikki")
	if !errors.Is(err, ErrApplyVerify) {
		t.Fatalf("код 6 не опознан: %v", err)
	}
	if !strings.Contains(err.Error(), "усечён") {
		t.Errorf("усечение stderr не помечено: длина текста %d", len(err.Error()))
	}
	// Но именно помечен, а не превращён в отказ по лимиту: код возврата
	// тут настоящий и ценнее.
	if errors.Is(err, ErrOutputTooLarge) {
		t.Error("болтливость в stderr выдана за переполнение stdout")
	}
}

// Обычный короткий вывод правкой не задет — включая завершающий перевод
// строки, который UCIGet срезает сам.
func TestRunShortOutputUnchanged(t *testing.T) {
	e := New()
	e.uciBin = stubBin(t, "printf '192.168.1.1\\n'\n")

	out, err := e.UCIShow(context.Background(), "network")
	if err != nil {
		t.Fatalf("UCIShow: %v", err)
	}
	if string(out) != "192.168.1.1\n" {
		t.Errorf("вывод = %q, ожидался неизменным", out)
	}

	got, err := e.UCIGet(context.Background(), "network", "lan", "ipaddr")
	if err != nil {
		t.Fatalf("UCIGet: %v", err)
	}
	if got != "192.168.1.1" {
		t.Errorf("значение = %q, ожидалось 192.168.1.1", got)
	}

	// Пустой вывод (uci commit, uci set) остаётся пустым, а не становится
	// ошибкой.
	e.uciBin = stubBin(t, "exit 0\n")
	if err := e.UCICommit(context.Background(), "wireless"); err != nil {
		t.Errorf("успешный commit вернул ошибку: %v", err)
	}
}

// Лимит не должен сам стать тратой памяти: буфер растёт по мере надобности,
// а не аллоцируется на мегабайт под каждый `uci get`.
func TestLimitedBufferGrowsLazily(t *testing.T) {
	b := &limitedBuffer{limit: MaxStdout}
	if _, err := b.Write([]byte("nikki\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if c := cap(b.buf.Bytes()); c > 4096 {
		t.Errorf("под 6 байт занято %d Б — буфер аллоцируется на лимит", c)
	}
	if b.truncated {
		t.Error("короткая запись помечена как усечённая")
	}
}

func TestLimitedBufferKeepsPrefixAndFlags(t *testing.T) {
	calls := 0
	b := &limitedBuffer{limit: 10, onLimit: func() { calls++ }}

	// Запись через границу: удерживается ровно limit байт.
	n, err := b.Write([]byte("абвгде"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("абвгде") {
		t.Fatalf("Write вернул %d из %d — короткая запись убьёт процесс по SIGPIPE",
			n, len("абвгде"))
	}
	if b.buf.Len() != 10 {
		t.Errorf("удержано %d Б, ожидалось 10", b.buf.Len())
	}
	if !b.truncated {
		t.Fatal("переполнение не отмечено")
	}

	// Запись в уже переполненный буфер: принимается целиком и
	// отбрасывается, процесс снимается один раз, а не на каждой записи.
	if n, err = b.Write([]byte("ещё")); err != nil || n != len("ещё") {
		t.Errorf("Write после переполнения = (%d, %v), ожидалось (%d, nil)", n, err, len("ещё"))
	}
	if b.buf.Len() != 10 {
		t.Errorf("буфер вырос до %d Б после переполнения", b.buf.Len())
	}
	if calls != 1 {
		t.Errorf("onLimit вызван %d раз, ожидался ровно один", calls)
	}
}

// Лимит stdout обязан с запасом покрывать самый объёмный вывод, который
// роутер отдаёт легитимно, — иначе защита от OOM станет отказом на ровном
// месте. Проверяется на снятых фикстурах, а не на догадке.
func TestLimitCoversRealRouterOutput(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "recon", "raw")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("не читается каталог фикстур: %v", err)
	}
	largest, name := 0, ""
	for _, en := range entries {
		// .tsv здесь — не ответ роутера на команду, а наша собственная
		// телеметрия: сэмплер замера RQ-03 пишет строку четыре раза в
		// секунду в течение всего прогона, и её объём говорит о
		// длительности опыта, а не о том, сколько отдаёт ubus. Считать её
		// за фикстуру значит требовать от лимита многомегабайтного запаса
		// против файла, который executor не читает никогда.
		if strings.HasSuffix(en.Name(), ".tsv") {
			continue
		}
		info, err := en.Info()
		if err != nil {
			t.Fatal(err)
		}
		if int(info.Size()) > largest {
			largest, name = int(info.Size()), en.Name()
		}
	}
	if largest == 0 {
		t.Fatal("фикстур не найдено — тест ничего не проверяет")
	}
	// Десятикратный запас минимум: самый большой снимок — скан эфира,
	// а число видимых сетей меняется от места к месту.
	if MaxStdout < largest*10 {
		t.Errorf("лимит %d Б против %d Б (%s) — запаса нет", MaxStdout, largest, name)
	}
	if MaxStderr <= 0 || MaxStderr > MaxStdout {
		t.Errorf("лимит stderr %d Б бессмыслен", MaxStderr)
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

// ─────────── рантайм-половина механических запретов ───────────
//
// У обоих запретов есть статическая половина — правила R1 и R2 в
// scripts/check-wireless-write.sh. Она ловит ЛИТЕРАЛ в исходнике и в принципе
// не видит вычисленного: `opt := "dis" + "abled"` для грепа обычная строка, а
// `strconv.FormatBool(true)` — тем более. Рантайм видит ровно значение, откуда
// бы оно ни взялось, и не видит написанного. Половины закрывают разное, и ни
// одна не лишняя.
//
// Тесты постоянные, а не «проверили руками при добавлении»: рантайм-половина
// без теста гниёт так же тихо, как греп без самотеста, — с той разницей, что
// её пропажу не покажет даже дифф гварда.

// Удаление wireless.*.disabled запрещено всегда: секция БЕЗ disabled
// считается включённой (ADR-0004), то есть удаление — это включение
// станционной сети способом, который в диффе не выглядит как включение
// (ADR-0026, правило 2).
func TestDeleteOfDisabledIsRefusedEverywhere(t *testing.T) {
	ctx := context.Background()

	var got []string
	e := New()
	e.commandRunner = captureRunner(&got, []byte("ok"), nil)
	err := e.UCIDelete(ctx, "wireless", "wifinet2", "disabled")
	if !errors.Is(err, ErrDeleteDisabled) {
		t.Errorf("реальная реализация: ошибка %v, ожидался ErrDeleteDisabled", err)
	}
	if len(got) != 0 {
		t.Errorf("до запуска uci дело дошло: %v", got)
	}

	// Фейк обязан отказывать той же функцией. Разреши он запрещённое —
	// тесты фазы 2, которые почти все идут через него, доказывали бы
	// поведение, которого на роутере не существует.
	f := NewFake()
	err = f.UCIDelete(ctx, "wireless", "wifinet2", "disabled")
	if !errors.Is(err, ErrDeleteDisabled) {
		t.Errorf("фейк: ошибка %v, ожидался ErrDeleteDisabled", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("фейк записал запрещённое удаление: %v", f.Calls)
	}
}

// Запрет УЗКИЙ, и это половина его смысла: расширенный «до кучи» на соседние
// адреса, он сломал бы правку сетей (удаление ключа при переходе на открытую
// сеть) и удаление секции целиком.
func TestDeleteOfNeighbouringAddressesStillWorks(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name             string
		pkg, sec, option string
		want             []string
	}{
		{"пароль той же секции", "wireless", "wifinet2", "key",
			[]string{"/sbin/uci", "delete", "wireless.wifinet2.key"}},
		{"секция целиком", "wireless", "wifinet2", "",
			[]string{"/sbin/uci", "delete", "wireless.wifinet2"}},
		{"disabled в ЧУЖОМ пакете", "network", "lan", "disabled",
			[]string{"/sbin/uci", "delete", "network.lan.disabled"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			e := New()
			e.commandRunner = captureRunner(&got, []byte("ok"), nil)
			if err := e.UCIDelete(ctx, c.pkg, c.sec, c.option); err != nil {
				t.Fatalf("отказ на разрешённом адресе: %v", err)
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("команда %v, ожидалась %v", got, c.want)
			}

			f := NewFake()
			if err := f.UCIDelete(ctx, c.pkg, c.sec, c.option); err != nil {
				t.Errorf("фейк отказал на разрешённом адресе: %v", err)
			}
		})
	}
}

// Область значений disabled — только "0" и "1" (ADR-0004). `disabled=true`
// секцию НЕ выключает: uci прочтёт «true» как непонятное значение, а netifd —
// как включено. Пропущенное сюда значение гасит не то, что просили, и молча.
func TestDisabledValueOutsideDomainIsRefusedEverywhere(t *testing.T) {
	ctx := context.Background()
	// "true"/"false" — то, что даёт strconv.FormatBool; "" — забытая
	// переменная; "01" и " 1" — опечатки, которые uci проглотит.
	for _, value := range []string{"true", "false", "", "01", " 1", "да", "2"} {
		var got []string
		e := New()
		e.commandRunner = captureRunner(&got, []byte("ok"), nil)
		err := e.UCISet(ctx, "wireless", "wifinet2", "disabled", value)
		if !errors.Is(err, ErrDisabledValue) {
			t.Errorf("реальная реализация, значение %q: ошибка %v, ожидался ErrDisabledValue", value, err)
		}
		if len(got) != 0 {
			t.Errorf("значение %q дошло до запуска uci: %v", value, got)
		}
		// Само значение обязано быть в тексте: без него владелец и
		// разработчик видят «запрещено» и не видят, что именно записывали.
		if err != nil && value != "" && !strings.Contains(err.Error(), value) {
			t.Errorf("значение %q не названо в тексте отказа: %v", value, err)
		}

		f := NewFake()
		if err := f.UCISet(ctx, "wireless", "wifinet2", "disabled", value); !errors.Is(err, ErrDisabledValue) {
			t.Errorf("фейк, значение %q: ошибка %v, ожидался ErrDisabledValue", value, err)
		} else if len(f.Calls) != 0 {
			t.Errorf("фейк записал запрещённое значение %q: %v", value, f.Calls)
		}
	}
}

// И обратная сторона: разрешённое обязано проходить. Обе цифры нужны обеим
// реализациям — партия переключения пишет и "1" (погасить прочие), и "0"
// (включить цель), — а соседние опции запрет не касается вовсе.
func TestDisabledDomainDoesNotBlockLegitimateWrites(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                    string
		pkg, sec, option, value string
	}{
		{"погасить", "wireless", "wifinet0", "disabled", "1"},
		{"включить", "wireless", "wifinet2", "disabled", "0"},
		{"ssid с пробелом", "wireless", "wifinet2", "ssid", "Сеть с пробелом"},
		{"пароль со спецсимволами", "wireless", "wifinet2", "key", "p@ss w0rd!!"},
		{"disabled в чужом пакете", "network", "lan", "disabled", "true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			e := New()
			e.commandRunner = captureRunner(&got, []byte("ok"), nil)
			if err := e.UCISet(ctx, c.pkg, c.sec, c.option, c.value); err != nil {
				t.Fatalf("отказ на законной записи: %v", err)
			}
			want := []string{"/sbin/uci", "set", c.pkg + "." + c.sec + "." + c.option + "=" + c.value}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("команда %v, ожидалась %v", got, want)
			}

			f := NewFake()
			if err := f.UCISet(ctx, c.pkg, c.sec, c.option, c.value); err != nil {
				t.Errorf("фейк отказал на законной записи: %v", err)
			}
		})
	}
}

func TestIsUbusNotFound(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("ubus call network.interface.homelan status: exit status 4: Command failed: ubus call network.interface.homelan status {} (Not found)"), true},
		{errors.New("ubus: exit status 7: Command failed: Request timed out"), false},
		{context.DeadlineExceeded, false},
	} {
		if got := IsUbusNotFound(tt.err); got != tt.want {
			t.Errorf("IsUbusNotFound(%v)=%v, ожидалось %v", tt.err, got, tt.want)
		}
	}
}
