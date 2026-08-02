package wireless

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"netmoded/internal/uci"
)

func raw(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "recon", "raw", name))
	if err != nil {
		t.Fatalf("фикстура %s: %v", name, err)
	}
	return b
}

func parse(t *testing.T, name string) *uci.Config {
	t.Helper()
	c, err := uci.ParseShow("wireless", raw(t, name))
	if err != nil {
		t.Fatalf("разбор %s: %v", name, err)
	}
	return c
}

// idOf разворачивает Network.ID для сравнений. nil превращается в «‹null›»,
// а не в пустую строку: иначе тест не заметил бы возврата к `""` вместо
// `null` — то самое расхождение с ADR-0005, ради которого поле стало
// указателем.
func idOf(n Network) string {
	if n.ID == nil {
		return "‹null›"
	}
	return *n.ID
}

// Снимок живого роутера — канонический рабочий случай.
func TestClassifyRealFixtureIsSingle(t *testing.T) {
	sel := Classify(parse(t, "10-uci-show-wireless.txt"), "radio0")

	if sel.State != Single {
		t.Fatalf("состояние %q, ожидалось single", sel.State)
	}
	if sel.Active != "wifinet0" {
		t.Errorf("активная %q, ожидалась wifinet0", sel.Active)
	}
	if got, want := len(sel.Sections), 2; got != want {
		t.Errorf("наших секций %d, ожидалось %d: %v", got, want, sel.Sections)
	}
	if len(sel.Conflict) != 0 {
		t.Errorf("конфликтов быть не должно: %v", sel.Conflict)
	}
	// wifinet1 — точка доступа на radio1, она не наша.
	for _, s := range sel.Sections {
		if s == "wifinet1" {
			t.Error("домашняя точка доступа попала в станционные секции")
		}
	}
}

func TestClassifyStates(t *testing.T) {
	const hdr = "wireless.radio0=wifi-device\nwireless.radio0.band='2g'\n"

	sta := func(name, ssid, disabled string) string {
		s := "wireless." + name + "=wifi-iface\n" +
			"wireless." + name + ".device='radio0'\n" +
			"wireless." + name + ".mode='sta'\n" +
			"wireless." + name + ".ssid='" + ssid + "'\n"
		if disabled != "" {
			s += "wireless." + name + ".disabled='" + disabled + "'\n"
		}
		return s
	}

	tests := []struct {
		name     string
		body     string
		want     State
		active   string
		conflict int
	}{
		{"нет станционных секций", hdr, Empty, "", 0},
		{"только точка доступа", hdr +
			"wireless.ap=wifi-iface\nwireless.ap.device='radio1'\nwireless.ap.mode='ap'\n", Empty, "", 0},
		{"одна включена", hdr + sta("a", "A", ""), Single, "a", 0},
		{"одна включена явным нулём", hdr + sta("a", "A", "0"), Single, "a", 0},
		{"одна включена, одна нет", hdr + sta("a", "A", "") + sta("b", "B", "1"), Single, "a", 0},
		{"все выключены", hdr + sta("a", "A", "1") + sta("b", "B", "1"), AllDisabled, "", 0},
		{"две включены", hdr + sta("a", "A", "") + sta("b", "B", ""), Ambiguous, "", 2},
		{"три включены", hdr + sta("a", "A", "") + sta("b", "B", "0") + sta("c", "C", ""), Ambiguous, "", 3},
	}

	for _, tt := range tests {
		c, err := uci.ParseShow("wireless", []byte(tt.body))
		if err != nil {
			t.Fatalf("%s: разбор: %v", tt.name, err)
		}
		sel := Classify(c, "radio0")
		if sel.State != tt.want {
			t.Errorf("%s: состояние %q, ожидалось %q", tt.name, sel.State, tt.want)
		}
		if sel.Active != tt.active {
			t.Errorf("%s: активная %q, ожидалась %q", tt.name, sel.Active, tt.active)
		}
		if len(sel.Conflict) != tt.conflict {
			t.Errorf("%s: конфликтов %d, ожидалось %d", tt.name, len(sel.Conflict), tt.conflict)
		}
	}
}

// Нераспознанное значение disabled не трактуется по-своему: мы не знаем,
// как его прочтёт netifd, поэтому весь набор объявляется неоднозначным.
func TestUnparseableDisabledPoisonsWholeSelection(t *testing.T) {
	body := "wireless.radio0=wifi-device\n" +
		"wireless.a=wifi-iface\nwireless.a.device='radio0'\nwireless.a.mode='sta'\nwireless.a.disabled='1'\n" +
		"wireless.b=wifi-iface\nwireless.b.device='radio0'\nwireless.b.mode='sta'\nwireless.b.disabled='может быть'\n"

	c, _ := uci.ParseShow("wireless", []byte(body))
	sel := Classify(c, "radio0")

	if sel.State != Ambiguous {
		t.Fatalf("состояние %q, ожидалось ambiguous", sel.State)
	}
	if len(sel.Conflict) != 1 || sel.Conflict[0] != "b" {
		t.Errorf("конфликт %v, ожидалась только секция b", sel.Conflict)
	}
	if sel.Reason == "" {
		t.Error("причина неоднозначности должна быть названа — иначе панели нечего показать")
	}
}

// Анонимную sta-секцию может создать LuCI. Она обязана участвовать в
// классификации: иначе Ambiguous не сработает там, где должен, — секция
// невидима для нас, но продолжает влиять на то, что поднимет netifd.
func TestAnonymousSectionCounts(t *testing.T) {
	body := "wireless.radio0=wifi-device\n" +
		"wireless.a=wifi-iface\nwireless.a.device='radio0'\nwireless.a.mode='sta'\n" +
		"wireless.@wifi-iface[7]=wifi-iface\n" +
		"wireless.@wifi-iface[7].device='radio0'\nwireless.@wifi-iface[7].mode='sta'\n"

	c, _ := uci.ParseShow("wireless", []byte(body))
	sel := Classify(c, "radio0")

	if sel.State != Ambiguous {
		t.Fatalf("состояние %q — анонимная секция не учтена", sel.State)
	}
	if len(sel.Sections) != 2 {
		t.Errorf("секций %d, ожидалось 2", len(sel.Sections))
	}
}

func TestSectionsKeepFileOrder(t *testing.T) {
	sel := Classify(parse(t, "10-uci-show-wireless.txt"), "radio0")
	want := []string{"wifinet0", "wifinet2"}
	for i, w := range want {
		if i >= len(sel.Sections) || sel.Sections[i] != w {
			t.Errorf("секции %v, ожидались %v", sel.Sections, want)
			break
		}
	}
}

// ─────────── список сохранённых сетей ───────────

func TestNetworksFromFixture(t *testing.T) {
	c := parse(t, "10-uci-show-wireless.txt")
	sel := Classify(c, "radio0")
	nets := Networks(c, sel, "radio0")

	if len(nets) != 2 {
		t.Fatalf("сетей %d, ожидалось 2", len(nets))
	}

	active, saved := nets[0], nets[1]
	if idOf(active) != "wifinet0" || active.SSID != "John24" || !active.Enabled {
		t.Errorf("первая сеть: id=%s %+v", idOf(active), active)
	}
	// Активную секцию в фазе 1 править нельзя (ADR-0009).
	if active.Editable {
		t.Error("активная сеть не должна быть редактируемой в фазе 1")
	}
	if idOf(saved) != "wifinet2" || saved.SSID != "ATOM" || saved.Enabled {
		t.Errorf("вторая сеть: id=%s %+v", idOf(saved), saved)
	}
	if !saved.Editable {
		t.Error("выключенная сеть должна быть редактируемой")
	}
	// Пароль не покидает роутер: наружу идёт только факт его наличия.
	if !active.HasKey || !saved.HasKey {
		t.Error("has_key должен быть true — в фикстуре у обеих есть key")
	}
}

// has_key отвечает ровно на один вопрос: «в секции сохранён пароль».
//
// Вопрос «можно ли подключиться без пароля» — другой, и ответ на него у
// открытой сети противоположный. Пока формула их складывала, открытая сеть
// сообщала о сохранённом пароле, которого в секции нет вовсе.
func TestOpenNetworkHasNoKey(t *testing.T) {
	body := "wireless.radio0=wifi-device\n" +
		"wireless.open=wifi-iface\nwireless.open.device='radio0'\n" +
		"wireless.open.mode='sta'\nwireless.open.ssid='FreeWiFi'\n" +
		"wireless.open.encryption='none'\nwireless.open.disabled='1'\n"

	c, err := uci.ParseShow("wireless", []byte(body))
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	nets := Networks(c, Classify(c, "radio0"), "radio0")
	if len(nets) != 1 {
		t.Fatalf("сетей %d, ожидалась 1", len(nets))
	}
	if nets[0].HasKey {
		t.Error("у открытой сети has_key=true — пароля в секции нет")
	}
}

// Пустой key — это тоже «пароля нет»: секция с encryption=psk2 и пустым
// паролем не поднимется, и показывать её как настроенную нельзя.
func TestEmptyKeyIsNoKey(t *testing.T) {
	body := "wireless.radio0=wifi-device\n" +
		"wireless.blank=wifi-iface\nwireless.blank.device='radio0'\n" +
		"wireless.blank.mode='sta'\nwireless.blank.ssid='X'\n" +
		"wireless.blank.encryption='psk2'\nwireless.blank.key=''\n" +
		"wireless.blank.disabled='1'\n"

	c, err := uci.ParseShow("wireless", []byte(body))
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	nets := Networks(c, Classify(c, "radio0"), "radio0")
	if len(nets) != 1 {
		t.Fatalf("сетей %d, ожидалась 1", len(nets))
	}
	if nets[0].HasKey {
		t.Error("has_key=true при пустом key")
	}
}

func TestNetworksNeverExposeKey(t *testing.T) {
	// Регрессия на ADR-0012: структура не должна иметь поля с паролем вовсе,
	// иначе он однажды уедет в JSON вместе с остальным.
	c := parse(t, "10-uci-show-wireless.txt")
	nets := Networks(c, Classify(c, "radio0"), "radio0")
	for _, n := range nets {
		if containsSecret(n) {
			t.Errorf("в сети %s просочился секрет", idOf(n))
		}
	}
}

func TestNetworksAmbiguousMakesNothingEditable(t *testing.T) {
	body := "wireless.radio0=wifi-device\n" +
		"wireless.a=wifi-iface\nwireless.a.device='radio0'\nwireless.a.mode='sta'\n" +
		"wireless.b=wifi-iface\nwireless.b.device='radio0'\nwireless.b.mode='sta'\n" +
		"wireless.c=wifi-iface\nwireless.c.device='radio0'\nwireless.c.mode='sta'\nwireless.c.disabled='1'\n"

	c, _ := uci.ParseShow("wireless", []byte(body))
	sel := Classify(c, "radio0")
	for _, n := range Networks(c, sel, "radio0") {
		if n.Editable {
			t.Errorf("при ambiguous секция %s не может быть редактируемой", idOf(n))
		}
	}
}

func TestAnonymousNetworkIsNotEditable(t *testing.T) {
	// Секцию без имени нельзя адресовать стабильно: индекс съедет при
	// удалении соседней. Показываем, но не даём править.
	body := "wireless.radio0=wifi-device\n" +
		"wireless.@wifi-iface[0]=wifi-iface\n" +
		"wireless.@wifi-iface[0].device='radio0'\nwireless.@wifi-iface[0].mode='sta'\n" +
		"wireless.@wifi-iface[0].disabled='1'\n"

	c, _ := uci.ParseShow("wireless", []byte(body))
	nets := Networks(c, Classify(c, "radio0"), "radio0")
	if len(nets) != 1 {
		t.Fatalf("сетей %d, ожидалась 1", len(nets))
	}
	if nets[0].ID != nil {
		t.Errorf("id анонимной секции = %q, ожидался nil", *nets[0].ID)
	}
	if nets[0].Editable {
		t.Error("анонимная секция не редактируется")
	}
}

// Проверка на уровне JSON, а не структуры: расхождение с ADR-0005 наступает
// именно в сериализованном виде. Пустая строка вместо null в JS ложно-истинна
// и склеивается в валидный путь `/api/wifi/networks/` — то есть «сети без
// адреса» хватило бы, чтобы дойти до DELETE.
func TestAnonymousSectionSerializesIDAsNull(t *testing.T) {
	body := "wireless.radio0=wifi-device\n" +
		"wireless.@wifi-iface[0]=wifi-iface\n" +
		"wireless.@wifi-iface[0].device='radio0'\nwireless.@wifi-iface[0].mode='sta'\n" +
		"wireless.@wifi-iface[0].disabled='1'\n" +
		"wireless.named=wifi-iface\nwireless.named.device='radio0'\n" +
		"wireless.named.mode='sta'\nwireless.named.disabled='1'\n"

	c, _ := uci.ParseShow("wireless", []byte(body))
	nets := Networks(c, Classify(c, "radio0"), "radio0")
	if len(nets) != 2 {
		t.Fatalf("сетей %d, ожидалось 2", len(nets))
	}

	var got []map[string]any
	b, err := json.Marshal(nets)
	if err != nil {
		t.Fatalf("сериализация: %v", err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}

	// Ключ обязан присутствовать — со значением null, а не исчезать.
	v, ok := got[0]["id"]
	if !ok {
		t.Fatalf("в JSON нет ключа id: %s", b)
	}
	if v != nil {
		t.Errorf("id анонимной секции = %#v, ожидался null: %s", v, b)
	}
	if got[1]["id"] != "named" {
		t.Errorf("id именованной секции = %#v, ожидалось \"named\"", got[1]["id"])
	}
}

// ─────────── отпечаток ───────────

func TestFingerprintStableAndSensitive(t *testing.T) {
	a := raw(t, "10-uci-show-wireless.txt")
	f1, f2 := Fingerprint(a), Fingerprint(a)
	if f1 != f2 {
		t.Error("отпечаток одного входа должен совпадать")
	}
	if f1 == "" {
		t.Fatal("пустой отпечаток")
	}
	b := append(append([]byte{}, a...), []byte("wireless.x=wifi-iface\n")...)
	if Fingerprint(b) == f1 {
		t.Error("отпечаток обязан меняться при правке конфига — на нём стоит защита от гонки")
	}
}

// ─────────── статус радио ───────────

func TestParseStatusFixture(t *testing.T) {
	st, err := ParseStatus(raw(t, "21-ubus-network-wireless-status.json"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}

	r0, ok := st["radio0"]
	if !ok {
		t.Fatal("radio0 не найдено")
	}
	if !r0.Up {
		t.Error("radio0 должно быть up")
	}
	if r0.Config.Band != "2g" {
		t.Errorf("band radio0 = %q, ожидался 2g", r0.Config.Band)
	}
	if len(r0.Interfaces) != 1 {
		t.Fatalf("интерфейсов radio0: %d", len(r0.Interfaces))
	}
	// Связь секция↔ifname дана прямо в ответе — сопоставлять по порядку
	// не нужно, а хардкодить имя тем более.
	if r0.Interfaces[0].Section != "wifinet0" || r0.Interfaces[0].Ifname != "phy0.0-sta0" {
		t.Errorf("radio0.interfaces[0] = %+v", r0.Interfaces[0])
	}

	r1 := st["radio1"]
	if r1.Config.Band != "5g" || r1.Interfaces[0].Ifname != "phy0.1-ap0" {
		t.Errorf("radio1 = %+v", r1)
	}
}

func TestIfnameLookup(t *testing.T) {
	st, _ := ParseStatus(raw(t, "21-ubus-network-wireless-status.json"))

	if got := IfnameForSection(st, "wifinet0"); got != "phy0.0-sta0" {
		t.Errorf("ifname wifinet0 = %q", got)
	}
	if got := IfnameForMode(st, "radio1", "ap"); got != "phy0.1-ap0" {
		t.Errorf("ifname точки доступа = %q", got)
	}
	// Несуществующее — пустая строка, а не выдуманное имя.
	if got := IfnameForSection(st, "нет-такой"); got != "" {
		t.Errorf("для неизвестной секции вернулось %q, ожидалась пустая строка", got)
	}
}

func TestPendingApply(t *testing.T) {
	st, _ := ParseStatus(raw(t, "21-ubus-network-wireless-status.json"))
	if PendingApply(st) {
		t.Error("в фикстуре pending=false у обоих радио")
	}
}

// ─────────── привязка «радио ↔ роль ↔ диапазон» ───────────

// Снимок живого роутера: станция действительно на radio0. Разрешение обязано
// дать ровно то, что раньше было константой, — иначе доказать, что новый код
// эквивалентен старому на подтверждённом железе, нечем.
func TestResolveRadiosFromRealFixtures(t *testing.T) {
	st, _ := ParseStatus(raw(t, "21-ubus-network-wireless-status.json"))
	got := ResolveRadios(st, parse(t, "10-uci-show-wireless.txt"))

	want := Radios{Station: "radio0", AP: "radio1", StationBand: "2g", APBand: "5g"}
	if got != want {
		t.Errorf("ResolveRadios = %+v, ожидалось %+v", got, want)
	}
	if !got.Known() {
		t.Error("станционное радио выведено, Known() обязан быть true")
	}
}

// Главный сценарий отказа, ради которого убраны константы: перепрошивка или
// другая ревизия железа переставляют радио местами. Код обязан пойти за
// mode=sta, а не за индексом, иначе инвариант фазы 1 (ADR-0009) молча
// применится к домашней точке.
func TestResolveRadiosFollowsModeNotIndex(t *testing.T) {
	swapped := []byte(`{
		"radio0": {"up": true, "config": {"band": "5g"},
			"interfaces": [{"section": "wifinet1", "ifname": "phy0.0-ap0",
				"config": {"mode": "ap", "ssid": "grenderNet"}}]},
		"radio1": {"up": true, "config": {"band": "2g"},
			"interfaces": [{"section": "wifinet0", "ifname": "phy0.1-sta0",
				"config": {"mode": "sta", "ssid": "John24"}}]}
	}`)
	st, err := ParseStatus(swapped)
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}

	got := ResolveRadios(st, nil)
	want := Radios{Station: "radio1", AP: "radio0", StationBand: "2g", APBand: "5g"}
	if got != want {
		t.Errorf("ResolveRadios = %+v, ожидалось %+v — разрешение пошло за индексом, а не за режимом", got, want)
	}
}

// Фаза 1 создаёт станционную секцию ВЫКЛЮЧЕННОЙ (ADR-0009), поэтому в
// network.wireless status её нет вовсе. Без чтения UCI такой роутер выглядел
// бы как роутер без станции, и панель не дала бы завести первую сеть.
func TestResolveRadiosFallsBackToUCIWhenRadioIsOff(t *testing.T) {
	// В ubus только домашняя точка: станционное радио выключено.
	st, _ := ParseStatus([]byte(`{
		"radio0": {"up": true, "config": {"band": "5g"},
			"interfaces": [{"section": "ap", "ifname": "phy0.0-ap0",
				"config": {"mode": "ap"}}]},
		"radio1": {"up": false, "disabled": true, "config": {"band": "2g"},
			"interfaces": []}
	}`))
	c, err := uci.ParseShow("wireless", []byte(
		"wireless.radio0=wifi-device\nwireless.radio0.band='5g'\n"+
			"wireless.radio1=wifi-device\nwireless.radio1.band='2g'\n"+
			"wireless.ap=wifi-iface\nwireless.ap.device='radio0'\nwireless.ap.mode='ap'\n"+
			"wireless.up=wifi-iface\nwireless.up.device='radio1'\nwireless.up.mode='sta'\nwireless.up.disabled='1'\n"))
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}

	got := ResolveRadios(st, c)
	if got.Station != "radio1" {
		t.Errorf("станция = %q, ожидалось radio1 из секции UCI с mode=sta", got.Station)
	}
	// Диапазон выключенного радио в ubus взять негде — берётся из wifi-device.
	if got.StationBand != "2g" {
		t.Errorf("диапазон станции = %q, ожидалось 2g из секции wifi-device", got.StationBand)
	}
	if got.AP != "radio0" || got.APBand != "5g" {
		t.Errorf("домашняя точка = %q/%q, ожидалось radio0/5g", got.AP, got.APBand)
	}
}

// Живое состояние главнее записанного намерения: если ubus говорит, что
// станция на radio1, устаревшая секция UCI не имеет права это перебить.
func TestResolveRadiosPrefersUbusOverUCI(t *testing.T) {
	st, _ := ParseStatus([]byte(`{
		"radio1": {"up": true, "config": {"band": "5g"},
			"interfaces": [{"section": "s", "ifname": "phy0.1-sta0",
				"config": {"mode": "sta"}}]}
	}`))
	c, _ := uci.ParseShow("wireless", []byte(
		"wireless.radio0=wifi-device\nwireless.radio0.band='2g'\n"+
			"wireless.old=wifi-iface\nwireless.old.device='radio0'\nwireless.old.mode='sta'\n"))

	got := ResolveRadios(st, c)
	if got.Station != "radio1" || got.StationBand != "5g" {
		t.Errorf("разрешение = %+v, ожидалось radio1/5g из живого ubus", got)
	}
}

// Не выяснили — значит не выяснили. Никакого «возьмём радио с меньшим
// индексом»: это тот же хардкод, только без комментария.
func TestResolveRadiosGuessesNothing(t *testing.T) {
	cases := []struct {
		name string
		st   map[string]Radio
		c    *uci.Config
	}{
		{"источников нет вовсе", nil, nil},
		{"радио есть, интерфейсов нет", mustStatus(t, `{"radio0":{"up":true,"config":{"band":"2g"},"interfaces":[]}}`), nil},
		{"в UCI только точка доступа", nil, mustConfig(t,
			"wireless.radio0=wifi-device\nwireless.radio0.band='2g'\n"+
				"wireless.ap=wifi-iface\nwireless.ap.device='radio0'\nwireless.ap.mode='ap'\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveRadios(tc.st, tc.c)
			if got.Station != "" || got.StationBand != "" {
				t.Errorf("выведено %+v, ожидалась пустота: угадывать нечем", got)
			}
			if got.Known() {
				t.Error("Known() == true без единого mode=sta — вызывающий не отказал бы")
			}
		})
	}
}

// Порядок обхода map в Go случаен, а ответ обязан быть одним и тем же:
// иначе демон правил бы то одну секцию, то другую, и воспроизвести это
// было бы нечем.
func TestResolveRadiosIsDeterministic(t *testing.T) {
	st, _ := ParseStatus([]byte(`{
		"radio0": {"config": {"band": "2g"}, "interfaces": [{"config": {"mode": "sta"}}]},
		"radio1": {"config": {"band": "5g"}, "interfaces": [{"config": {"mode": "sta"}}]},
		"radio2": {"config": {"band": "6g"}, "interfaces": [{"config": {"mode": "sta"}}]}
	}`))
	first := ResolveRadios(st, nil)
	for i := 0; i < 50; i++ {
		if got := ResolveRadios(st, nil); got != first {
			t.Fatalf("итерация %d дала %+v, первая — %+v", i, got, first)
		}
	}
}

func mustStatus(t *testing.T, s string) map[string]Radio {
	t.Helper()
	st, err := ParseStatus([]byte(s))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	return st
}

func mustConfig(t *testing.T, s string) *uci.Config {
	t.Helper()
	c, err := uci.ParseShow("wireless", []byte(s))
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	return c
}

// ─────────── скан ───────────

func TestParseScanFixture(t *testing.T) {
	nets, err := ParseScan(raw(t, "23-ubus-iwinfo-scan.json"))
	if err != nil {
		t.Fatalf("ParseScan: %v", err)
	}
	if len(nets) != 14 {
		t.Fatalf("сетей %d, ожидалось 14", len(nets))
	}

	first := nets[0]
	if first.SSID != "John24" || first.SignalDBm != -48 || first.Channel != 1 {
		t.Errorf("первая сеть: %+v", first)
	}
	if first.Encryption != "psk2" {
		t.Errorf("шифрование %q, ожидалось psk2 (wpa [2] + psk)", first.Encryption)
	}
}

// Три сети из четырнадцати пришли БЕЗ поля ssid — это скрытые сети.
// Такую форму не придумывают, её только наблюдают; рукописный мок её бы
// не поймал, а структура с ssid string молча превратила бы их в сети
// с пустым именем.
func TestParseScanHandlesHiddenNetworks(t *testing.T) {
	nets, err := ParseScan(raw(t, "23-ubus-iwinfo-scan.json"))
	if err != nil {
		t.Fatalf("ParseScan: %v", err)
	}
	hidden := 0
	for _, n := range nets {
		if n.Hidden {
			hidden++
			if n.SSID != "" {
				t.Errorf("скрытая сеть с непустым ssid: %+v", n)
			}
			if n.BSSID == "" {
				t.Error("у скрытой сети должен остаться bssid — иначе её нечем показать")
			}
		}
	}
	if hidden != 3 {
		t.Errorf("скрытых сетей %d, ожидалось 3", hidden)
	}
}

func TestParseScanEncryptionVariants(t *testing.T) {
	nets, _ := ParseScan(raw(t, "23-ubus-iwinfo-scan.json"))
	seen := map[string]bool{}
	for _, n := range nets {
		seen[n.Encryption] = true
	}
	// В фикстуре есть wpa [2], [2,3]+sae и [1,2].
	for _, want := range []string{"psk2", "sae-mixed", "psk-mixed"} {
		if !seen[want] {
			t.Errorf("вариант шифрования %q не распознан; получены: %v", want, keys(seen))
		}
	}
}

func TestParseScanEmpty(t *testing.T) {
	nets, err := ParseScan([]byte(`{"results":[]}`))
	if err != nil {
		t.Fatalf("пустой скан — не ошибка: %v", err)
	}
	if len(nets) != 0 {
		t.Errorf("сетей %d, ожидалось 0", len(nets))
	}
}

func TestParseScanRejectsGarbage(t *testing.T) {
	if _, err := ParseScan([]byte("не json")); err == nil {
		t.Error("мусор принят молча")
	}
}

// ─────────── ассоциация ───────────

func TestParseInfoFixture(t *testing.T) {
	info, err := ParseInfo(raw(t, "24-ubus-iwinfo-info.json"))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	// Главное: поле с ассоциированным SSID называется ssid, и оно
	// сходится с включённой секцией wifinet0 из raw/10.
	if info.SSID != "John24" {
		t.Errorf("ssid = %q, ожидался John24", info.SSID)
	}
	if info.SignalDBm != -45 || info.NoiseDBm != -79 {
		t.Errorf("сигнал/шум = %d/%d, ожидалось -45/-79", info.SignalDBm, info.NoiseDBm)
	}
	if info.Mode != "Client" {
		t.Errorf("режим %q, ожидался Client", info.Mode)
	}
}

func TestParseInfoNotAssociated(t *testing.T) {
	// Когда ассоциации нет, iwinfo отдаёт объект без ssid.
	info, err := ParseInfo([]byte(`{"phy":"phy0","mode":"Client","signal":0}`))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if info.SSID != "" {
		t.Errorf("ssid = %q, ожидался пустой", info.SSID)
	}
	if info.Associated {
		t.Error("без ssid ассоциации нет")
	}
}
