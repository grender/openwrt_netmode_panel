package luci

import "testing"

// Путь берётся из подтверждённой константы, а не пишется в тесте заново.
//
// Второй литерал сделал бы тест зелёным при любой правке константы — то
// есть перестал бы стеречь ровно то, ради чего пакет и заведён.
func TestPanelURLUsesConfirmedPath(t *testing.T) {
	u, ok := PanelURL("192.168.9.1")
	if !ok {
		t.Fatal("PanelURL отказал на годном хосте")
	}
	if want := "http://192.168.9.1" + PanelPath; u != want {
		t.Errorf("PanelURL = %q, ожидалось %q", u, want)
	}
	// Порт 80 — умолчание схемы, в адресе его быть не должно.
	if u != "http://192.168.9.1"+PanelPath {
		t.Errorf("в адресе появился порт: %q", u)
	}
}

// ГЛАВНАЯ ловушка: url.URL.String() голый IPv6 не скобкует.
//
// Без скобок получается http://fd00::1/cgi-bin/luci — браузер разберёт это
// как хост fd00 с портом, то есть уведёт владельца в никуда молча. Порта у
// нас нет, поэтому net.JoinHostPort сюда не подставишь, и скобки — наша
// забота.
func TestPanelURLBracketsIPv6(t *testing.T) {
	u, ok := PanelURL("fd00::1")
	if !ok {
		t.Fatal("PanelURL отказал на IPv6")
	}
	if want := "http://[fd00::1]" + PanelPath; u != want {
		t.Errorf("PanelURL = %q, ожидалось %q — IPv6 без скобок ведёт на другой хост", u, want)
	}
}

// Скобки, пришедшие снаружи, не удваиваются.
func TestPanelURLKeepsExistingBrackets(t *testing.T) {
	u, ok := PanelURL("[fd00::1]")
	if !ok {
		t.Fatal("PanelURL отказал на скобленном IPv6")
	}
	if want := "http://[fd00::1]" + PanelPath; u != want {
		t.Errorf("PanelURL = %q, ожидалось %q", u, want)
	}
}

// Пустой хост — отказ, а не адрес без хоста. http:///cgi-bin/luci открылся бы
// в браузере как относительная ссылка на саму панель.
func TestPanelURLRefusesEmptyHost(t *testing.T) {
	if u, ok := PanelURL(""); ok {
		t.Errorf("PanelURL(\"\") = (%q, true), ожидался отказ", u)
	}
}

// Имя хоста проходит так же, как адрес: владелец ходит на роутер по имени
// из dnsmasq не реже, чем по IP.
func TestPanelURLAcceptsHostname(t *testing.T) {
	u, ok := PanelURL("router.lan")
	if !ok {
		t.Fatal("PanelURL отказал на имени хоста")
	}
	if want := "http://router.lan" + PanelPath; u != want {
		t.Errorf("PanelURL = %q, ожидалось %q", u, want)
	}
}
