package netif

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "recon", "raw",
		"26-ubus-network-interface-wwan.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	return b
}

func TestParseStatusFixture(t *testing.T) {
	s, err := ParseStatus(fixture(t))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if !s.Up || s.Pending {
		t.Errorf("up=%v pending=%v", s.Up, s.Pending)
	}
	if s.Proto != "dhcp" || s.Device != "phy0.0-sta0" {
		t.Errorf("proto=%q device=%q", s.Proto, s.Device)
	}
	if s.Uptime != 215 {
		t.Errorf("uptime=%d, ожидалось 215", s.Uptime)
	}
	if len(s.Addresses) != 1 || s.Addresses[0].Address != "192.168.0.234" || s.Addresses[0].Mask != 24 {
		t.Errorf("адреса: %+v", s.Addresses)
	}
	if got := s.Gateway(); got != "192.168.0.1" {
		t.Errorf("шлюз %q, ожидался 192.168.0.1", got)
	}
	if len(s.DNSServers) != 1 || s.DNSServers[0] != "192.168.0.1" {
		t.Errorf("dns: %v", s.DNSServers)
	}
}

// Одного up недостаточно — это главное в этом файле. Интерфейс поднимается
// и без аренды DHCP; проверка одного флага дала бы уверенное «всё хорошо»
// ровно тогда, когда интернета нет.
func TestOnlineNeedsAddressAndRoute(t *testing.T) {
	full, err := ParseStatus(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !full.Online() {
		t.Error("полный набор признаков должен давать online")
	}

	tests := []struct {
		name string
		mut  func(*Status)
	}{
		{"интерфейс опущен", func(s *Status) { s.Up = false }},
		{"нет адреса", func(s *Status) { s.Addresses = nil }},
		{"нет маршрутов", func(s *Status) { s.Routes = nil }},
		{"маршрут без шлюза", func(s *Status) { s.Routes[0].Nexthop = "" }},
		{"маршрут не по умолчанию", func(s *Status) { s.Routes[0].Mask = 24 }},
	}
	for _, tt := range tests {
		s, _ := ParseStatus(fixture(t))
		tt.mut(&s)
		if s.Online() {
			t.Errorf("%s: Online() = true, ожидалось false", tt.name)
		}
	}
}

func TestParseStatusRejectsGarbage(t *testing.T) {
	if _, err := ParseStatus([]byte("не json")); err == nil {
		t.Error("мусор принят молча")
	}
}

func TestZeroStatusIsOffline(t *testing.T) {
	// Пустой ответ (интерфейса нет) не должен выглядеть как «всё хорошо».
	var s Status
	if s.Online() {
		t.Error("пустой статус не может быть online")
	}
	if s.Gateway() != "" {
		t.Error("у пустого статуса не может быть шлюза")
	}
}
