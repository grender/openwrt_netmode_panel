package nikki

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Форма адреса снята с LuCI (raw/71-luci-nikki-open-dashboard.txt): query,
// четыре параметра, host и hostname дублируются.
func TestPanelURLMatchesLuCIShape(t *testing.T) {
	got, ok := PanelURL("router.lan", "9090", "s3cret")
	if !ok {
		t.Fatal("PanelURL отказал на годных данных")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("адрес не разбирается: %v", err)
	}
	if u.Scheme != "http" || u.Host != "router.lan:9090" || u.Path != PanelPath {
		t.Errorf("основа адреса = %q", got)
	}
	// Hash вместо query оставил бы секрет на клиенте, но дашборд его не
	// прочитал бы: LuCI кладёт параметры именно в строку запроса.
	if u.Fragment != "" {
		t.Errorf("параметры уехали во фрагмент: %q", got)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"host": "router.lan", "hostname": "router.lan",
		"port": "9090", "secret": "s3cret",
	} {
		if q.Get(k) != want {
			t.Errorf("параметр %s = %q, ожидалось %q", k, q.Get(k), want)
		}
	}
}

// Секрет с «неудобным» символом обязан доезжать целиком.
//
// Ровно то, ради чего сборка идёт через url.Values: склейка руками дала бы
// адрес, в котором браузер увидел бы лишний параметр и усечённый секрет —
// и владелец получил бы «неверный пароль» без единого следа причины.
func TestPanelURLEscapesSecret(t *testing.T) {
	secret := "a&b=c d/#?"
	got, ok := PanelURL("192.168.9.1", "9090", secret)
	if !ok {
		t.Fatal("PanelURL отказал")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("адрес не разбирается: %v", err)
	}
	if u.Query().Get("secret") != secret {
		t.Errorf("секрет доехал как %q, ожидалось %q (адрес %q)",
			u.Query().Get("secret"), secret, got)
	}
	if len(u.Query()) != 4 {
		t.Errorf("в адресе %d параметров вместо четырёх: %q", len(u.Query()), got)
	}
}

// В /api/status уезжает адрес БЕЗ параметров — в нём не должно быть секрета
// даже случайно.
func TestPanelBaseURLHasNoQuery(t *testing.T) {
	got, ok := PanelBaseURL("router.lan", "9090")
	if !ok {
		t.Fatal("PanelBaseURL отказал")
	}
	if got != "http://router.lan:9090"+PanelPath {
		t.Errorf("адрес = %q", got)
	}
	if strings.ContainsAny(got, "?#") {
		t.Errorf("в адресе для статуса есть параметры: %q", got)
	}
}

// IPv6 обязан скобковаться: ручная склейка дала бы другой хост без порта.
func TestPanelURLBracketsIPv6(t *testing.T) {
	got, ok := PanelBaseURL("fd00::1", "9090")
	if !ok {
		t.Fatal("PanelBaseURL отказал на IPv6")
	}
	if got != "http://[fd00::1]:9090"+PanelPath {
		t.Errorf("адрес = %q", got)
	}
}

func TestPanelURLRefusesEmptyParts(t *testing.T) {
	if got, ok := PanelBaseURL("", "9090"); ok {
		t.Errorf("пустой хост принят: %q", got)
	}
	if got, ok := PanelURL("router.lan", "", "s"); ok {
		t.Errorf("пустой порт принят: %q", got)
	}
}

// Порт берётся разбором адреса Clash API, а не вторым литералом: адрес
// выводится из nikki.mixin.api_listen, и владелец вправе сменить порт.
func TestPanelPort(t *testing.T) {
	if p, ok := PanelPort("http://127.0.0.1:9091"); !ok || p != "9091" {
		t.Errorf("PanelPort = (%q, %v), ожидалось (9091, true)", p, ok)
	}
	// Без порта выводить нечего — и подставлять «обычный» 9090 нельзя:
	// это и есть догадка, от которой ссылка ведёт в никуда.
	for _, bad := range []string{"", "http://127.0.0.1", "://"} {
		if p, ok := PanelPort(bad); ok {
			t.Errorf("PanelPort(%q) = %q, ожидался отказ", bad, p)
		}
	}
}

// Живая проба: 2xx — панель есть, всё остальное — ErrPanelMissing.
func TestPanelAlive(t *testing.T) {
	cases := []struct {
		name string
		code int
		want error
	}{
		{"есть", http.StatusOK, nil},
		{"дашборд не скачан", http.StatusNotFound, ErrPanelMissing},
		{"движок отвечает ошибкой", http.StatusInternalServerError, ErrPanelMissing},
		{"статика под авторизацией", http.StatusUnauthorized, ErrPanelMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var path string
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, auth = r.URL.Path, r.Header.Get("Authorization")
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte("<html></html>"))
			}))
			defer srv.Close()

			err := New(srv.URL, "s3cret").PanelAlive(context.Background())
			if !errors.Is(err, c.want) {
				t.Errorf("PanelAlive = %v, ожидалось %v", err, c.want)
			}
			if path != PanelPath {
				t.Errorf("проба ушла на %q вместо %q", path, PanelPath)
			}
			// Проверяем ровно то, что сделает браузер, а он заголовков не
			// носит. Проба с заголовком показала бы «панель есть» там, где
			// владелец увидит отказ.
			if auth != "" {
				t.Errorf("проба ушла с заголовком %q — проверено не то, что откроет браузер", auth)
			}
		})
	}
}

// Молчащий движок — это недоступность, а не «панели нет»: чинится это
// разными действиями, и код отказа обязан их различать.
func TestPanelAliveOnDeadEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	err := New(addr, "").PanelAlive(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("PanelAlive на мёртвом движке = %v, ожидалось ErrUnavailable", err)
	}
	if errors.Is(err, ErrPanelMissing) {
		t.Error("молчание движка выдано за отсутствие панели")
	}
}
