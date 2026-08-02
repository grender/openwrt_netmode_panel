package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"netmoded/internal/executor"
)

// logSink — журнал демона в тестах.
//
// Свой мьютекс обязателен: статус пишет в журнал под своим замком и из
// своей горутины, а тест читает из другой.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// matching — строки журнала, содержащие подстроку.
func (l *logSink) matching(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

func newReader(t *testing.T) (*StatusReader, *executor.Fake) {
	t.Helper()
	r, f, _ := newReaderWithLog(t)
	return r, f
}

func newReaderWithLog(t *testing.T) (*StatusReader, *executor.Fake, *logSink) {
	t.Helper()
	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.UCIValues["netmode.main.mode"] = "nikki"
	f.UCIValues["system.@system[0].hostname"] = "grenderRouter"
	sink := &logSink{}
	r := NewStatusReader(f, sink.logf)
	r.b4 = newFakeB4Client()
	r.nikki = newFakeNikkiClient()
	return r, f, sink
}

// ─────────── перестановка радио ───────────
//
// Фикстуры разведки сняты с роутера, где станция действительно на radio0, —
// поэтому на них невозможно отличить выведенную привязку от угаданной по
// индексу. Эти фикстуры переставляют радио местами И меняют диапазоны:
// станция уезжает на radio1 (5 ГГц), домашняя точка — на radio0 (2.4 ГГц).
// Ровно это делает перепрошивка или другая ревизия железа, и ровно на этом
// старый хардкод начинал бы править домашнюю сеть вместо внешней (ADR-0019).

const swappedStatusJSON = `{
	"radio0": {
		"up": true, "pending": false, "disabled": false,
		"config": {"band": "2g", "channel": "auto", "country": "RU"},
		"interfaces": [{"section": "wifinet1", "ifname": "phy0.0-ap0",
			"config": {"mode": "ap", "ssid": "grenderNet"}}]
	},
	"radio1": {
		"up": true, "pending": false, "disabled": false,
		"config": {"band": "5g", "channel": "auto", "country": "RU"},
		"interfaces": [{"section": "wifinet0", "ifname": "phy0.1-sta0",
			"config": {"mode": "sta", "ssid": "John24"}}]
	}
}`

const swappedWirelessUCI = "wireless.radio0=wifi-device\n" +
	"wireless.radio0.band='2g'\n" +
	"wireless.radio1=wifi-device\n" +
	"wireless.radio1.band='5g'\n" +
	"wireless.wifinet0=wifi-iface\n" +
	"wireless.wifinet0.device='radio1'\n" +
	"wireless.wifinet0.mode='sta'\n" +
	"wireless.wifinet0.network='wwan'\n" +
	"wireless.wifinet0.ssid='John24'\n" +
	"wireless.wifinet0.encryption='psk2'\n" +
	"wireless.wifinet1=wifi-iface\n" +
	"wireless.wifinet1.device='radio0'\n" +
	"wireless.wifinet1.mode='ap'\n" +
	"wireless.wifinet1.ssid='grenderNet'\n" +
	"wireless.wifinet1.network='lan'\n" +
	"wireless.wifinet2=wifi-iface\n" +
	"wireless.wifinet2.device='radio1'\n" +
	"wireless.wifinet2.mode='sta'\n" +
	"wireless.wifinet2.ssid='ATOM'\n" +
	"wireless.wifinet2.encryption='psk2'\n" +
	"wireless.wifinet2.disabled='1'\n"

// swapRadios переставляет радио местами в обоих источниках сразу.
func swapRadios(f *executor.Fake) {
	f.Fixtures["ubus network.wireless status"] = []byte(swappedStatusJSON)
	f.Fixtures["uci show wireless"] = []byte(swappedWirelessUCI)
}

// noStationAnywhere убирает станцию отовсюду: радио включено, но станционного
// интерфейса нет ни в живом состоянии, ни в конфигурации.
func noStationAnywhere(f *executor.Fake) {
	f.Fixtures["ubus network.wireless status"] = []byte(
		`{"radio0":{"up":true,"config":{"band":"2g"},"interfaces":[]}}`)
	f.Fixtures["uci show wireless"] = []byte(
		"wireless.radio0=wifi-device\nwireless.radio0.band='2g'\n" +
			"wireless.wifinet1=wifi-iface\nwireless.wifinet1.device='radio0'\n" +
			"wireless.wifinet1.mode='ap'\nwireless.wifinet1.ssid='grenderNet'\n")
}

// Привязка идёт за режимом интерфейса, а не за индексом радио.
func TestStatusFollowsSwappedRadios(t *testing.T) {
	r, f := newReader(t)
	swapRadios(f)

	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Станция теперь на radio1: классификация обязана считать нашими
	// секции с device=radio1, иначе внешних сетей «не окажется».
	if s.SelectionState != "single" {
		t.Errorf("selection_state = %q, ожидалось single: станция уехала на другое радио, а не исчезла", s.SelectionState)
	}
	if s.ConfiguredSSID == nil || *s.ConfiguredSSID != "John24" {
		t.Errorf("configured_ssid = %v", s.ConfiguredSSID)
	}
	if s.AssociatedSSID == nil || *s.AssociatedSSID != "John24" {
		t.Errorf("associated_ssid = %v: имя интерфейса выведено не по тому радио", s.AssociatedSSID)
	}
	// Домашняя точка теперь на radio0 и на 2.4 ГГц. Прежний хардкод дал бы
	// сюда 5g — то есть панель показывала бы чужой диапазон.
	if s.AP.SSID != "grenderNet" || s.AP.Band != "2g" {
		t.Errorf("ap = %+v, ожидалось grenderNet/2g", s.AP)
	}
}

// Станционная секция фазы 1 создаётся выключенной (ADR-0009) и в ubus не
// появляется вовсе. Роутер, у которого ещё не поднята ни одна внешняя сеть,
// обязан оставаться понятным: радио выводится из UCI.
func TestStatusResolvesStationFromUCIWhenRadioIsOff(t *testing.T) {
	r, f := newReader(t)
	f.Fixtures["ubus network.wireless status"] = []byte(`{
		"radio0": {"up": true, "config": {"band": "2g"},
			"interfaces": [{"section": "wifinet1", "ifname": "phy0.0-ap0",
				"config": {"mode": "ap", "ssid": "grenderNet"}}]},
		"radio1": {"up": false, "disabled": true, "config": {"band": "5g"},
			"interfaces": []}
	}`)
	f.Fixtures["uci show wireless"] = []byte(
		"wireless.radio0=wifi-device\nwireless.radio0.band='2g'\n" +
			"wireless.radio1=wifi-device\nwireless.radio1.band='5g'\n" +
			"wireless.wifinet1=wifi-iface\nwireless.wifinet1.device='radio0'\n" +
			"wireless.wifinet1.mode='ap'\nwireless.wifinet1.ssid='grenderNet'\n" +
			"wireless.netmode_a1b2c3d4=wifi-iface\n" +
			"wireless.netmode_a1b2c3d4.device='radio1'\n" +
			"wireless.netmode_a1b2c3d4.mode='sta'\n" +
			"wireless.netmode_a1b2c3d4.ssid='ATOM'\n" +
			"wireless.netmode_a1b2c3d4.disabled='1'\n")

	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Секция найдена и выключена — это all_disabled, а не empty. Empty
	// означало бы «сохранённых сетей нет», и панель предложила бы завести
	// вторую копию той же сети.
	if s.SelectionState != "all_disabled" {
		t.Errorf("selection_state = %q, ожидалось all_disabled: секция есть, просто выключена", s.SelectionState)
	}
	if s.AssociatedSSID != nil {
		t.Errorf("associated_ssid = %v, станционного интерфейса в системе нет", *s.AssociatedSSID)
	}
	if s.AP.SSID != "grenderNet" || s.AP.Band != "2g" {
		t.Errorf("ap = %+v", s.AP)
	}
}

// Статус не отказывает никогда (ADR-0017): невыясненное радио гасит поля,
// а не превращает ответ в ошибку. 503 radio_unknown — удел путей, которые
// собираются что-то ТРОГАТЬ.
func TestStatusStaysAnsweringWhenRadioUnknown(t *testing.T) {
	r, f := newReader(t)
	noStationAnywhere(f)

	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.SelectionState != "empty" {
		t.Errorf("selection_state = %q, ожидалось empty", s.SelectionState)
	}
	if s.ConfiguredSSID != nil {
		t.Errorf("configured_ssid = %v, станционного радио не выяснено", *s.ConfiguredSSID)
	}
	// Домашняя точка выведена независимо и обязана остаться видимой.
	if s.AP.SSID != "grenderNet" || s.AP.Band != "2g" {
		t.Errorf("ap = %+v: неизвестная станция не должна гасить домашнюю точку", s.AP)
	}
}

func TestStatusFromRealFixtures(t *testing.T) {
	r, _ := newReader(t)
	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if s.Mode != "nikki" {
		t.Errorf("mode = %q", s.Mode)
	}
	if s.Hostname != "grenderRouter" {
		t.Errorf("hostname = %q", s.Hostname)
	}
	// Снимок роутера: включена ровно одна станционная секция.
	if s.SelectionState != "single" {
		t.Errorf("selection_state = %q, ожидалось single", s.SelectionState)
	}
	if s.ConfiguredSSID == nil || *s.ConfiguredSSID != "John24" {
		t.Errorf("configured_ssid = %v", s.ConfiguredSSID)
	}
	if s.AssociatedSSID == nil || *s.AssociatedSSID != "John24" {
		t.Errorf("associated_ssid = %v", s.AssociatedSSID)
	}
	if s.AP.SSID != "grenderNet" || s.AP.Band != "5g" {
		t.Errorf("ap = %+v", s.AP)
	}
	// Число клиентов не подтверждено разведкой (RQ-05) — честный null,
	// а не выдуманный ноль: неверный ноль хуже честного «не знаю».
	if s.AP.Clients != nil {
		t.Errorf("ap.clients = %v, ожидался null до ответа на RQ-05", *s.AP.Clients)
	}
	if !s.Online.OK {
		t.Error("в фикстуре wwan поднят, есть адрес и маршрут по умолчанию")
	}
	if s.Fingerprint == "" {
		t.Error("отпечаток пуст")
	}
	if s.PendingApply {
		t.Error("в фикстуре pending=false")
	}
}

// Расхождение configured/associated — самый полезный сигнал в панели,
// поэтому поля не сливаются.
func TestConfiguredAndAssociatedAreSeparate(t *testing.T) {
	r, f := newReader(t)
	// Ассоциация есть, конфигурация неоднозначна: configured обязан стать
	// null, associated — остаться.
	f.Fixtures["uci show wireless"] = []byte(
		"wireless.radio0=wifi-device\n" +
			"wireless.a=wifi-iface\nwireless.a.device='radio0'\nwireless.a.mode='sta'\n" +
			"wireless.b=wifi-iface\nwireless.b.device='radio0'\nwireless.b.mode='sta'\n")

	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.SelectionState != "ambiguous" {
		t.Fatalf("selection_state = %q", s.SelectionState)
	}
	if s.ConfiguredSSID != nil {
		t.Errorf("при ambiguous configured_ssid обязан быть null, получено %v", *s.ConfiguredSSID)
	}
	if s.AssociatedSSID == nil {
		t.Error("физическая правда доступна и при ambiguous — associated_ssid должен остаться")
	}
	if len(s.Conflict) != 2 {
		t.Errorf("conflict = %v, ожидались обе секции", s.Conflict)
	}
}

// Недоступность источника деградирует поле, а не валит весь статус.
// Панель, переставшая отвечать из-за упавшего ubus, бесполезна ровно тогда,
// когда нужна.
func TestStatusDegradesInsteadOfFailing(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*executor.Fake)
		check  func(*testing.T, *Status)
	}{
		{
			"ubus недоступен целиком",
			func(f *executor.Fake) {
				f.Errors["ubus network.wireless status"] = errors.New("ubus down")
				f.Errors["ubus iwinfo info"] = errors.New("ubus down")
				f.Errors["ubus network.interface.wwan status"] = errors.New("ubus down")
			},
			func(t *testing.T, s *Status) {
				if s.SelectionState != "single" {
					t.Error("конфигурация читается из uci и не зависит от ubus")
				}
				if s.AssociatedSSID != nil {
					t.Error("без ubus ассоциация неизвестна — должно быть null")
				}
				if s.Online.OK {
					t.Error("без ubus нельзя утверждать, что интернет есть")
				}
			},
		},
		{
			"uci недоступен",
			func(f *executor.Fake) {
				f.Errors["uci show wireless"] = errors.New("uci down")
			},
			func(t *testing.T, s *Status) {
				if s.SelectionState != "empty" {
					t.Errorf("без конфигурации ожидалось empty, получено %q", s.SelectionState)
				}
				if s.ConfiguredSSID != nil {
					t.Error("configured_ssid должен быть null")
				}
			},
		},
		{
			"netmode отсутствует (свежая установка)",
			func(f *executor.Fake) { delete(f.UCIValues, "netmode.main.mode") },
			func(t *testing.T, s *Status) {
				if s.Mode != "off" {
					t.Errorf("mode = %q, ожидалось off", s.Mode)
				}
			},
		},
		{
			"mode вне набора не исправляется",
			func(f *executor.Fake) { f.UCIValues["netmode.main.mode"] = "мусор" },
			func(t *testing.T, s *Status) {
				if s.Mode != "unknown" {
					t.Errorf("mode = %q, ожидалось unknown", s.Mode)
				}
			},
		},
	}

	for _, tt := range tests {
		r, f := newReader(t)
		tt.break_(f)
		s, err := r.Read(context.Background())
		if err != nil {
			t.Errorf("%s: статус упал целиком: %v", tt.name, err)
			continue
		}
		tt.check(t, s)
	}
}

// Сбой чтения UCI — это не «владелец выключил режим».
//
// Отдать "off" при упавшем uci означало бы показать выключенный тумблер
// над работающим nikki: владелец нажал бы «включить» и получил бы перезапуск
// того, что и так работало.
func TestModeUnknownWhenUCIFails(t *testing.T) {
	r, f := newReader(t)
	f.Errors["uci get netmode.main.mode"] = errors.New("uci down")

	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.Mode != "unknown" {
		t.Errorf("mode = %q, при сбое чтения ожидалось unknown", s.Mode)
	}
}

// online.checked отличает «проверили, канала нет» от «проверить не смогли».
func TestOnlineCheckedReportsWhetherWeAsked(t *testing.T) {
	r, _ := newReader(t)
	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !s.Online.Checked {
		t.Error("ubus ответил и разобрался — checked обязан быть true")
	}

	r, f := newReader(t)
	f.Errors["ubus network.interface.wwan status"] = errors.New("ubus down")
	s, err = r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.Online.Checked {
		t.Error("ubus недоступен — checked обязан быть false")
	}
	if s.Online.OK {
		t.Error("непроверенный канал не может считаться поднятым")
	}

	// Ответ пришёл, но не разобрался — это тоже «не проверили».
	r, f = newReader(t)
	f.Fixtures["ubus network.interface.wwan status"] = []byte("не json")
	s, err = r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.Online.Checked {
		t.Error("неразобранный ответ — checked обязан быть false")
	}
}

// Повторяющийся сбой не имеет права повторяться в журнале.
//
// Панель опрашивает статус раз в секунду; строка на каждое чтение писала бы
// на overlay-флеш роутера круглые сутки и топила бы в себе всё остальное.
// Отмена запроса — не сбой источника.
//
// b4 и nikki намеренно пропускают context.Canceled наружу без обёртки
// ErrUnavailable: отменил чтение мы сами, роутер тут ни при чём. Панель
// отменяет запросы штатно — по уходу со страницы и по таймауту, — поэтому
// без этой проверки журнал заполнялся бы парами «недоступен»/«снова
// отвечает» о сбоях, которых не было.
func TestCancelledReadIsNotASourceFailure(t *testing.T) {
	r, f, sink := newReaderWithLog(t)
	base := time.Unix(1700000000, 0)
	cur := base
	r.now = func() time.Time { return cur }

	f.Errors["ubus network.interface.wwan status"] = context.Canceled
	for i := 0; i < 3; i++ {
		cur = cur.Add(StatusCacheTTL + time.Millisecond)
		if _, err := r.Read(context.Background()); err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if got := sink.matching("network.interface.wwan"); len(got) != 0 {
		t.Errorf("отмена дала %d строк журнала, ожидалось ноль: %q", len(got), got)
	}

	// Настоящий сбой того же источника после отмен обязан быть замечен:
	// отмены не должны были взвести бит «источник уже лежит».
	f.Errors["ubus network.interface.wwan status"] = errors.New("ubus down")
	cur = cur.Add(StatusCacheTTL + time.Millisecond)
	if _, err := r.Read(context.Background()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := sink.matching("network.interface.wwan"); len(got) != 1 {
		t.Errorf("настоящий сбой после отмен дал %d строк, ожидалась одна: %q", len(got), got)
	}
}

func TestSourceFailureLoggedOncePerTransition(t *testing.T) {
	r, f, sink := newReaderWithLog(t)
	base := time.Unix(1700000000, 0)
	cur := base
	r.now = func() time.Time { return cur }

	// Каждое чтение — за пределами TTL, то есть система реально опрашивается.
	read := func() {
		cur = cur.Add(StatusCacheTTL + time.Millisecond)
		if _, err := r.Read(context.Background()); err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	f.Errors["ubus network.interface.wwan status"] = errors.New("ubus down")
	for i := 0; i < 5; i++ {
		read()
	}
	if got := sink.matching("network.interface.wwan"); len(got) != 1 {
		t.Errorf("пять неудачных чтений дали %d строк журнала, ожидалась одна: %q", len(got), got)
	}

	// Возврат источника обязан быть виден: иначе единственная строка о сбое
	// висит в журнале вечно и врёт про текущее состояние.
	delete(f.Errors, "ubus network.interface.wwan status")
	read()
	got := sink.matching("network.interface.wwan")
	if len(got) != 2 {
		t.Fatalf("после восстановления ожидались две строки, получено %d: %q", len(got), got)
	}
	if !strings.Contains(got[1], "снова") {
		t.Errorf("вторая строка обязана сообщать о восстановлении, получено %q", got[1])
	}

	// И снова молчание, пока состояние не менялось.
	read()
	if got := sink.matching("network.interface.wwan"); len(got) != 2 {
		t.Errorf("рабочий источник продолжает писать в журнал: %q", got)
	}
}

// Кэш обязан жить ровно 500 мс: панель опрашивает раз в секунду, и без
// кэша каждый опрос стоил бы четырёх запусков процессов на роутере.
func TestStatusCache500ms(t *testing.T) {
	r, f := newReader(t)
	base := time.Unix(1700000000, 0)
	cur := base
	r.now = func() time.Time { return cur }

	if _, err := r.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstCalls := len(f.Reads())

	// В пределах TTL система не трогается.
	cur = base.Add(499 * time.Millisecond)
	if _, err := r.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Reads()); got != firstCalls {
		t.Errorf("в пределах TTL было %d чтений, стало %d — кэш не сработал", firstCalls, got)
	}

	// За пределами — читается заново.
	cur = base.Add(501 * time.Millisecond)
	if _, err := r.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Reads()); got <= firstCalls {
		t.Error("после истечения TTL статус обязан перечитаться")
	}
}

func TestStatusCacheIsNotState(t *testing.T) {
	// Кэш не должен переживать изменение системы дольше TTL: правка
	// wireless мимо демона обязана стать видимой сама.
	r, f := newReader(t)
	base := time.Unix(1700000000, 0)
	cur := base
	r.now = func() time.Time { return cur }

	s1, _ := r.Read(context.Background())
	if s1.SelectionState != "single" {
		t.Fatalf("исходно %q", s1.SelectionState)
	}

	f.Fixtures["uci show wireless"] = []byte("wireless.radio0=wifi-device\n")
	cur = base.Add(StatusCacheTTL + time.Millisecond)

	s2, _ := r.Read(context.Background())
	if s2.SelectionState != "empty" {
		t.Errorf("после правки мимо демона ожидалось empty, получено %q", s2.SelectionState)
	}
}

func TestStatusSerializesWithoutSecrets(t *testing.T) {
	r, _ := newReader(t)
	s, _ := r.Read(context.Background())
	b, err := MarshalStatus(s)
	if err != nil {
		t.Fatalf("MarshalStatus: %v", err)
	}
	// В фикстуре пароли заменены плейсхолдерами, но проверяем сам факт:
	// ни одно поле статуса не несёт значения key.
	for _, bad := range []string{"REDACTED_PSK", "\"key\"", "password"} {
		if contains(b, bad) {
			t.Errorf("в статусе просочилось %q", bad)
		}
	}
}

// Ноль включённых сетов — это факт, а не отсутствие данных.
//
// Пока поля стояли под omitempty, «b4 отвечает, включённых сетов нет» и
// «демон вообще не прислал состояние b4» выглядели в JSON одинаково: ключа
// нет ни там, ни там. Потребитель обязан их различать, поэтому проверка идёт
// по сериализованному виду, а не по структуре — расхождение возникает именно
// при маршалинге.
func TestServiceFieldsAlwaysPresent(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*StatusReader)
		want    map[string]any
	}{
		{
			name:    "b4 отвечает, включённых сетов нет",
			prepare: func(r *StatusReader) { r.b4.(*fakeB4Client).sets[1].Enabled = false },
			want: map[string]any{
				"available":     true,
				"version":       "1.74.1",
				"set":           "",
				"enabled_count": float64(0),
			},
		},
		{
			name:    "b4 недоступен",
			prepare: func(r *StatusReader) { r.b4.(*fakeB4Client).err = errors.New("connection refused") },
			want: map[string]any{
				"available":     false,
				"version":       nil,
				"set":           "",
				"enabled_count": float64(0),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newReader(t)
			tc.prepare(r)

			s, _ := r.Read(context.Background())
			b, err := MarshalStatus(s)
			if err != nil {
				t.Fatalf("MarshalStatus: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("разбор: %v", err)
			}
			b4obj, ok := got["b4"].(map[string]any)
			if !ok {
				t.Fatalf("в статусе нет объекта b4: %s", b)
			}
			for k, want := range tc.want {
				v, present := b4obj[k]
				if !present {
					t.Errorf("нет ключа %q — «нет значения» неотличимо от «не прислали»: %v", k, b4obj)
					continue
				}
				if v != want {
					t.Errorf("b4.%s = %#v, ожидалось %#v", k, v, want)
				}
			}
		})
	}
}

func contains(b []byte, sub string) bool {
	return len(sub) > 0 && len(b) >= len(sub) && indexOf(string(b), sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
