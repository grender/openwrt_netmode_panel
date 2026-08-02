package httpapi

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"netmoded/internal/executor"
)

func newReader(t *testing.T) (*StatusReader, *executor.Fake) {
	t.Helper()
	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.UCIValues["netmode.main.mode"] = "nikki"
	f.UCIValues["system.@system[0].hostname"] = "grenderRouter"
	r := NewStatusReader(f)
	r.b4 = newFakeB4Client()
	r.nikki = newFakeNikkiClient()
	return r, f
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
