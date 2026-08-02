package led

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSysfs повторяет раскладку /sys/class/leds: каталог на светодиод,
// внутри файлы-атрибуты. Именно так устроен настоящий интерфейс ядра
// (docs/recon/raw/30-leds-and-phy.txt).
func fakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{Blue, White} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []struct{ name, val string }{
			{"trigger", "[none] timer heartbeat default-on netdev pattern"},
			{"brightness", "0"},
			{"max_brightness", "255"},
			{"delay_on", "0"},
			{"delay_off", "0"},
		} {
			if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.val), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func read(t *testing.T, root, name, attr string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name, attr))
	if err != nil {
		t.Fatalf("чтение %s/%s: %v", name, attr, err)
	}
	return strings.TrimSpace(string(b))
}

func TestSteadyStates(t *testing.T) {
	root := fakeSysfs(t)
	c := New(root, nil)
	defer c.Close()

	tests := []struct {
		state                  State
		blueTrig, blueBright   string
		whiteTrig, whiteBright string
	}{
		{Nikki, "none", "255", "none", "0"},
		{B4, "none", "0", "none", "255"},
		{Off, "none", "0", "none", "0"},
	}
	for _, tt := range tests {
		if err := c.Set(tt.state); err != nil {
			t.Fatalf("%s: %v", tt.state, err)
		}
		if got := read(t, root, Blue, "trigger"); got != tt.blueTrig {
			t.Errorf("%s: синий trigger=%q, ожидался %q", tt.state, got, tt.blueTrig)
		}
		if got := read(t, root, Blue, "brightness"); got != tt.blueBright {
			t.Errorf("%s: синий brightness=%q, ожидался %q", tt.state, got, tt.blueBright)
		}
		if got := read(t, root, White, "brightness"); got != tt.whiteBright {
			t.Errorf("%s: белый brightness=%q, ожидался %q", tt.state, got, tt.whiteBright)
		}
	}
}

// Мигание — штатным триггером ядра, а не горутиной: горутина мигала бы
// только пока жив процесс, и после падения демона светодиод замер бы
// в случайной фазе, продолжая врать.
func TestBlinkUsesKernelTimerTrigger(t *testing.T) {
	root := fakeSysfs(t)
	c := New(root, nil)
	defer c.Close()

	if err := c.Set(ApplyingNikki); err != nil {
		t.Fatalf("ApplyingNikki: %v", err)
	}
	if got := read(t, root, Blue, "trigger"); got != "timer" {
		t.Errorf("trigger=%q, ожидался timer", got)
	}
	if got := read(t, root, Blue, "delay_on"); got != "300" {
		t.Errorf("delay_on=%q", got)
	}
	if got := read(t, root, Blue, "delay_off"); got != "300" {
		t.Errorf("delay_off=%q", got)
	}
	// Мигает ЦЕЛЕВОЙ светодиод, второй погашен (SPEC §8).
	if got := read(t, root, White, "trigger"); got != "none" {
		t.Errorf("белый trigger=%q, ожидался none", got)
	}
}

func TestApplyingOffBlinksBlue(t *testing.T) {
	// Цель «выключено» тоже надо чем-то показать; берём синий.
	root := fakeSysfs(t)
	c := New(root, nil)
	defer c.Close()

	_ = c.Set(ApplyingOff)
	if got := read(t, root, Blue, "trigger"); got != "timer" {
		t.Errorf("синий trigger=%q", got)
	}
}

// Ошибка: оба быстро мигают три секунды, затем состояние по факту.
func TestFlashThenRevert(t *testing.T) {
	root := fakeSysfs(t)
	c := New(root, nil)
	defer c.Close()

	c.Flash(B4)

	for _, name := range []string{Blue, White} {
		if got := read(t, root, name, "trigger"); got != "timer" {
			t.Errorf("%s: trigger=%q, ожидался timer", name, got)
		}
		if got := read(t, root, name, "delay_on"); got != "100" {
			t.Errorf("%s: delay_on=%q — ошибка мигает быстрее применения", name, got)
		}
	}

	// Ждём возврата.
	deadline := time.Now().Add(errorHold + 2*time.Second)
	for time.Now().Before(deadline) {
		if read(t, root, White, "trigger") == "none" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := read(t, root, White, "brightness"); got != "255" {
		t.Errorf("после возврата белый brightness=%q, ожидался 255 (режим b4)", got)
	}
	if got := read(t, root, Blue, "brightness"); got != "0" {
		t.Errorf("после возврата синий brightness=%q, ожидался 0", got)
	}
}

// Новое состояние обязано отменять отложенный возврат: иначе через три
// секунды светодиод перескочит на устаревшую картину.
func TestSetCancelsPendingRevert(t *testing.T) {
	root := fakeSysfs(t)
	c := New(root, nil)
	defer c.Close()

	c.Flash(B4)
	if err := c.Set(Nikki); err != nil {
		t.Fatal(err)
	}

	time.Sleep(errorHold + 300*time.Millisecond)

	if got := read(t, root, Blue, "brightness"); got != "255" {
		t.Errorf("синий brightness=%q — отложенный возврат перебил новое состояние", got)
	}
	if got := read(t, root, White, "brightness"); got != "0" {
		t.Errorf("белый brightness=%q", got)
	}
}

// Таймер не должен пережить остановку демона: иначе он попробует писать
// в sysfs после закрытия — тихая утечка, заметная только по логу.
func TestCloseStopsPendingRevert(t *testing.T) {
	root := fakeSysfs(t)
	c := New(root, nil)

	c.Flash(Nikki)
	c.Close()

	// Загрубляем: делаем каталог недоступным. Если таймер всё же
	// сработает, тест это не свалит, но и записи не будет.
	before := read(t, root, Blue, "trigger")
	time.Sleep(errorHold + 300*time.Millisecond)
	if got := read(t, root, Blue, "trigger"); got != before {
		t.Errorf("состояние изменилось после Close: %q → %q", before, got)
	}
}

// Индикация — best-effort: ошибка записи НИКОГДА не валит операцию.
// Погасший светодиод — неудобство; сорванное переключение режима — потеря
// связи (ADR-0013).
func TestMissingSysfsDegradesQuietly(t *testing.T) {
	var logged []string
	c := New(filepath.Join(t.TempDir(), "нет-такого"), func(f string, a ...any) {
		logged = append(logged, f)
	})
	defer c.Close()

	// Set — основной путь: он зовётся при каждом переключении режима.
	// Ошибку он возвращает вызывающему, но деградацию помечает сам, иначе
	// /api/status уверял бы, что с индикацией всё в порядке.
	if err := c.Set(Nikki); err == nil {
		t.Error("ожидалась ошибка записи")
	}
	if !c.Degraded() {
		t.Error("после неудачного Set контроллер должен уйти в деградацию")
	}
	if len(logged) != 1 {
		t.Errorf("Set должен был залогировать ровно раз, а записей %d: %v", len(logged), logged)
	}

	// Flash не паникует и тоже не добавляет строк — деградация уже взведена.
	c.Flash(Nikki)
	if !c.Degraded() {
		t.Error("после неудачи контроллер должен уйти в деградацию")
	}

	// Повторные вызовы молчат: одинаковые строки в журнале прячут важное.
	n := len(logged)
	c.Flash(Nikki)
	c.Flash(B4)
	_ = c.Set(B4)
	_ = c.Set(Off)
	if len(logged) != n {
		t.Errorf("повторные неудачи снова залогированы: %v", logged)
	}
}

// Отсутствие sysfs — отказ железа, а неизвестное состояние — баг вызывающего.
// Второе не должно уводить контроллер в деградацию: noteLocked идемпотентен,
// и после такой пометки он навсегда замолчал бы про настоящий отказ записи.
func TestUnknownStateDoesNotDegrade(t *testing.T) {
	var logged []string
	c := New(fakeSysfs(t), func(f string, a ...any) {
		logged = append(logged, f)
	})
	defer c.Close()

	if err := c.Set(State("выдумка")); err == nil {
		t.Fatal("неизвестное состояние принято молча")
	}
	if c.Degraded() {
		t.Error("баг вызывающего не должен выключать индикацию целиком")
	}
	if len(logged) != 0 {
		t.Errorf("неизвестное состояние не повод объявлять деградацию: %v", logged)
	}

	// Железо на месте — следующее корректное состояние обязано примениться.
	if err := c.Set(Nikki); err != nil {
		t.Fatalf("Set после неизвестного состояния: %v", err)
	}
	if got := read(t, c.root, Blue, "brightness"); got != "255" {
		t.Errorf("синий brightness=%q — контроллер замолчал после чужого бага", got)
	}
}

func TestMaxBrightnessFallback(t *testing.T) {
	root := t.TempDir()
	// Каталог есть, max_brightness нет — берём единицу: «тускло» лучше,
	// чем «не горит».
	for _, name := range []string{Blue, White} {
		dir := filepath.Join(root, name)
		_ = os.MkdirAll(dir, 0o755)
		for _, f := range []string{"trigger", "brightness"} {
			_ = os.WriteFile(filepath.Join(dir, f), []byte("0"), 0o644)
		}
	}
	c := New(root, nil)
	defer c.Close()

	if err := c.Set(Nikki); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := read(t, root, Blue, "brightness"); got != "1" {
		t.Errorf("brightness=%q, ожидалась 1", got)
	}
}

func TestStateMapping(t *testing.T) {
	tests := []struct {
		mode          string
		steady, apply State
	}{
		{"nikki", Nikki, ApplyingNikki},
		{"b4", B4, ApplyingB4},
		{"off", Off, ApplyingOff},
		{"мусор", Off, ApplyingOff},
	}
	for _, tt := range tests {
		if got := StateForMode(tt.mode); got != tt.steady {
			t.Errorf("StateForMode(%q)=%q, ожидалось %q", tt.mode, got, tt.steady)
		}
		if got := ApplyingForMode(tt.mode); got != tt.apply {
			t.Errorf("ApplyingForMode(%q)=%q, ожидалось %q", tt.mode, got, tt.apply)
		}
	}
}

func TestUnknownStateIsError(t *testing.T) {
	c := New(fakeSysfs(t), nil)
	defer c.Close()
	err := c.Set(State("выдумка"))
	if err == nil {
		t.Fatal("неизвестное состояние принято молча")
	}
	if !errors.Is(err, ErrUnknownState) {
		t.Errorf("ошибка %v не опознаётся как ErrUnknownState", err)
	}
	if c.Degraded() {
		t.Error("неизвестное состояние не должно помечать контроллер деградировавшим")
	}
}
