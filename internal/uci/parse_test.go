package uci

import (
	"os"
	"path/filepath"
	"testing"
)

// Фикстуры — дословный вывод живого роутера (см. docs/recon/README.md).
// Рукописный мок проверял бы нашу догадку саму против себя; записанный
// вывод ловит ошибки формы.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "recon", "raw", name))
	if err != nil {
		t.Fatalf("не читается фикстура %s: %v", name, err)
	}
	return string(b)
}

func TestParseRealWirelessFixture(t *testing.T) {
	c, err := ParseShow("wireless", []byte(fixture(t, "10-uci-show-wireless.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got, want := len(c.Sections), 5; got != want {
		var names []string
		for _, s := range c.Sections {
			names = append(names, s.Name)
		}
		t.Fatalf("секций %d, ожидалось %d: %v", got, want, names)
	}

	tests := []struct {
		name    string
		typ     string
		options map[string]string
	}{
		{"radio0", "wifi-device", map[string]string{
			"band": "2g", "country": "RU", "radio": "0", "channel": "auto",
			"path": "soc/11280000.pcie/pci0000:00/0000:00:00.0/0000:01:00.0",
		}},
		{"radio1", "wifi-device", map[string]string{
			"band": "5g", "htmode": "HT20", "radio": "1",
		}},
		{"wifinet0", "wifi-iface", map[string]string{
			"device": "radio0", "mode": "sta", "network": "wwan",
			"ssid": "John24", "encryption": "psk2",
		}},
		{"wifinet1", "wifi-iface", map[string]string{
			"device": "radio1", "mode": "ap", "ssid": "grenderNet", "network": "lan",
		}},
		{"wifinet2", "wifi-iface", map[string]string{
			"device": "radio0", "mode": "sta", "ssid": "ATOM", "disabled": "1",
		}},
	}

	for _, tt := range tests {
		s, ok := c.Section(tt.name)
		if !ok {
			t.Errorf("секция %s не найдена", tt.name)
			continue
		}
		if s.Type != tt.typ {
			t.Errorf("%s: тип %q, ожидался %q", tt.name, s.Type, tt.typ)
		}
		for k, want := range tt.options {
			if got := s.Options[k]; got != want {
				t.Errorf("%s.%s = %q, ожидалось %q", tt.name, k, got, want)
			}
		}
	}
}

// Ключевой факт, подтверждённый на живом роутере: у wifinet0 нет опции
// disabled, и именно она в ассоциации. Значит отсутствие опции означает
// «включено», а не «выключено» — и демон обязан всегда писать её явно.
func TestMissingDisabledMeansEnabled(t *testing.T) {
	c, err := ParseShow("wireless", []byte(fixture(t, "10-uci-show-wireless.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	active, ok := c.Section("wifinet0")
	if !ok {
		t.Fatal("wifinet0 не найдена")
	}
	if _, present := active.Options["disabled"]; present {
		t.Error("у wifinet0 не должно быть опции disabled — она активна")
	}
	if active.Disabled() {
		t.Error("wifinet0 без опции disabled должна считаться включённой")
	}

	saved, ok := c.Section("wifinet2")
	if !ok {
		t.Fatal("wifinet2 не найдена")
	}
	if !saved.Disabled() {
		t.Error("wifinet2 с disabled='1' должна считаться выключенной")
	}
}

func TestDisabledTruthiness(t *testing.T) {
	// UCI считает истинными несколько написаний; всё остальное — ложь.
	// Неразбираемое значение НЕ должно молча трактоваться как «включено»:
	// вызывающий обязан увидеть неоднозначность.
	tests := []struct {
		value    string
		disabled bool
		valid    bool
	}{
		{"1", true, true},
		{"on", true, true},
		{"true", true, true},
		{"yes", true, true},
		{"enabled", true, true},
		{"0", false, true},
		{"off", false, true},
		{"false", false, true},
		{"no", false, true},
		{"", false, true},
		{"maybe", false, false},
		{"2", false, false},
	}

	for _, tt := range tests {
		s := Section{Options: map[string]string{"disabled": tt.value}}
		if tt.value == "" {
			s = Section{Options: map[string]string{}}
		}
		if got := s.Disabled(); got != tt.disabled {
			t.Errorf("disabled=%q: Disabled()=%v, ожидалось %v", tt.value, got, tt.disabled)
		}
		if got := s.DisabledValid(); got != tt.valid {
			t.Errorf("disabled=%q: DisabledValid()=%v, ожидалось %v", tt.value, got, tt.valid)
		}
	}
}

// Пароль со спецсимволами — самая ценная из трёх фикстур-плейсхолдеров:
// она проверяет, что парсер не спотыкается о ! @ и кавычки.
func TestParseValueWithSpecialChars(t *testing.T) {
	c, err := ParseShow("wireless", []byte(fixture(t, "10-uci-show-wireless.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, ok := c.Section("wifinet2")
	if !ok {
		t.Fatal("wifinet2 не найдена")
	}
	if got, want := s.Options["key"], "REDACTED_PSK!!SPECIAL@@"; got != want {
		t.Errorf("key = %q, ожидалось %q", got, want)
	}
}

func TestParseAnonymousSection(t *testing.T) {
	c, err := ParseShow("network", []byte(fixture(t, "11-uci-show-network.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	s, ok := c.Section("@device[0]")
	if !ok {
		t.Fatal("@device[0] не найдена")
	}
	if s.Type != "device" {
		t.Errorf("тип %q, ожидался device", s.Type)
	}
	if got := s.Options["name"]; got != "br-lan" {
		t.Errorf("name = %q, ожидалось br-lan", got)
	}
	if !s.Anonymous {
		t.Error("@device[0] должна быть помечена анонимной")
	}

	// Именованная секция — не анонимная.
	if w, ok := c.Section("wwan"); !ok {
		t.Error("wwan не найдена")
	} else if w.Anonymous {
		t.Error("wwan не анонимная")
	}
}

// Upstream-интерфейс из спеки оказался фактом, а не догадкой.
func TestNetworkFixtureHasWwan(t *testing.T) {
	c, err := ParseShow("network", []byte(fixture(t, "11-uci-show-network.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, ok := c.Section("wwan")
	if !ok {
		t.Fatal("network.wwan не найдена")
	}
	if got := s.Options["proto"]; got != "dhcp" {
		t.Errorf("wwan.proto = %q, ожидалось dhcp", got)
	}
	if lan, ok := c.Section("lan"); !ok {
		t.Error("lan не найдена")
	} else if got := lan.Options["ipaddr"]; got != "192.168.9.1/24" {
		t.Errorf("lan.ipaddr = %q, ожидалось 192.168.9.1/24", got)
	}
}

func TestParseList(t *testing.T) {
	raw := "p.s=t\np.s.single='one'\np.s.many='a' 'b' 'c'\n"
	c, err := ParseShow("p", []byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, ok := c.Section("s")
	if !ok {
		t.Fatal("секция s не найдена")
	}
	if got := s.Options["single"]; got != "one" {
		t.Errorf("single = %q", got)
	}
	got := s.Lists["many"]
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("many = %v, ожидалось %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("many[%d] = %q, ожидалось %q", i, got[i], want[i])
		}
	}
	// Список не должен дублироваться в Options — иначе вызывающий
	// незаметно прочитает только первый элемент.
	if _, dup := s.Options["many"]; dup {
		t.Error("список many не должен попадать в Options")
	}
}

func TestParseEscapedQuote(t *testing.T) {
	// uci show экранирует одинарную кавычку последовательностью '\''
	raw := `p.s=t` + "\n" + `p.s.k='it'\''s'` + "\n"
	c, err := ParseShow("p", []byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, _ := c.Section("s")
	if got, want := s.Options["k"], "it's"; got != want {
		t.Errorf("k = %q, ожидалось %q", got, want)
	}
}

func TestParsePreservesFileOrder(t *testing.T) {
	c, err := ParseShow("wireless", []byte(fixture(t, "10-uci-show-wireless.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"radio0", "radio1", "wifinet0", "wifinet1", "wifinet2"}
	if len(c.Sections) != len(want) {
		t.Fatalf("секций %d, ожидалось %d", len(c.Sections), len(want))
	}
	for i, w := range want {
		if c.Sections[i].Name != w {
			t.Errorf("секция[%d] = %q, ожидалась %q", i, c.Sections[i].Name, w)
		}
	}
}

func TestByType(t *testing.T) {
	c, err := ParseShow("wireless", []byte(fixture(t, "10-uci-show-wireless.txt")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ifaces := c.ByType("wifi-iface")
	if got, want := len(ifaces), 3; got != want {
		t.Fatalf("wifi-iface: %d, ожидалось %d", got, want)
	}
	devices := c.ByType("wifi-device")
	if got, want := len(devices), 2; got != want {
		t.Fatalf("wifi-device: %d, ожидалось %d", got, want)
	}
	if got := c.ByType("нет-такого"); len(got) != 0 {
		t.Errorf("несуществующий тип вернул %d секций", len(got))
	}
}

func TestParseSkipsCommentsAndBlanks(t *testing.T) {
	raw := "# комментарий\n\np.s=t\n   \np.s.k='v'\n# ещё один\n"
	c, err := ParseShow("p", []byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Sections) != 1 {
		t.Fatalf("секций %d, ожидалась 1", len(c.Sections))
	}
	if got := c.Sections[0].Options["k"]; got != "v" {
		t.Errorf("k = %q", got)
	}
}

func TestParseRejectsForeignPackage(t *testing.T) {
	// Опечатка в имени пакета не должна молча дать пустой результат:
	// это выглядело бы как «на роутере ничего нет».
	_, err := ParseShow("wireless", []byte("network.lan=interface\n"))
	if err == nil {
		t.Fatal("ожидалась ошибка на строке из чужого пакета")
	}
}

func TestParseMalformedLine(t *testing.T) {
	// Каждый случай изолирован: секция объявлена там, где проверяется
	// разбор значения, иначе тест прошёл бы по посторонней причине.
	tests := []struct {
		name string
		raw  string
	}{
		{"нет знака равенства", "мусор без всякой структуры\n"},
		{"незакрытая кавычка", "p.s=t\np.s.k='незакрытая\n"},
		{"мусор вне кавычек", "p.s=t\np.s.k='a' мусор\n"},
		{"пустой тип секции", "p.s=\n"},
	}
	for _, tt := range tests {
		if _, err := ParseShow("p", []byte(tt.raw)); err == nil {
			t.Errorf("%s: ожидалась ошибка на %q", tt.name, tt.raw)
		}
	}
}

func TestParseOptionBeforeSection(t *testing.T) {
	// Опция без объявленной секции — повреждённый вывод, а не «создай секцию».
	if _, err := ParseShow("p", []byte("p.s.k='v'\n")); err == nil {
		t.Fatal("ожидалась ошибка: опция раньше объявления секции")
	}
}

func TestParseEmpty(t *testing.T) {
	c, err := ParseShow("wireless", []byte(""))
	if err != nil {
		t.Fatalf("пустой ввод — не ошибка: %v", err)
	}
	if len(c.Sections) != 0 {
		t.Errorf("секций %d, ожидалось 0", len(c.Sections))
	}
}

func TestHasStagedChanges(t *testing.T) {
	// Пустой вывод `uci changes` — стейджинг чист, писать можно.
	// Непустой — кто-то держит черновик; наш commit опубликовал бы его.
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"пусто", "", false},
		{"только пробелы", "  \n\t\n", false},
		{"одна правка", "wireless.wifinet2.ssid='X'\n", true},
		{"несколько", "a.b.c='1'\nd.e.f='2'\n", true},
		{"комментарий не считается", "# заметка\n", false},
	}
	for _, tt := range tests {
		if got := HasStagedChanges([]byte(tt.raw)); got != tt.want {
			t.Errorf("%s: HasStagedChanges(%q) = %v, ожидалось %v", tt.name, tt.raw, got, tt.want)
		}
	}
}
