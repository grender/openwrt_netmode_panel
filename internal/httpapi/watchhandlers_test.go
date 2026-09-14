package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netmoded/internal/nikki"
	"netmoded/internal/watch"
)

func watchReq(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func errCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ответа не разобрано: %v (%s)", err, rec.Body.String())
	}
	return e.Code
}

// TestWatchStartRejectsBadIP: адрес не из локальной сети и не IPv4.
//
// Отдельный код bad_ip, а не общий bad_request: панель подсвечивает поле
// адреса и предлагает список устройств.
func TestWatchStartRejectsBadIP(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()

	cases := []struct{ name, body string }{
		{"мусор", `{"ip":"не адрес"}`},
		{"IPv6", `{"ip":"fd00::2"}`},
		{"чужая подсеть", `{"ip":"10.0.0.5"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := watchReq(t, s, "POST", "/api/watch", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("код = %d, ожидался 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := errCodeOf(t, rec); got != "bad_ip" {
				t.Errorf("код отказа = %q, ожидался bad_ip", got)
			}
		})
	}
	if s.currentWatch() != nil {
		t.Error("отбитый запрос всё-таки завёл сессию")
	}
}

// TestWatchStartRejectsOffMode: наблюдать нечего, и это 409, а не 503.
//
// Движок не сломан — владелец сам его выключил, и чинится это сменой режима
// на главной, а не ожиданием.
func TestWatchStartRejectsOffMode(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()
	f.UCIValues["netmode.main.mode"] = "off"

	rec := watchReq(t, s, "POST", "/api/watch", `{"ip":"192.168.9.219"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("код = %d, ожидался 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := errCodeOf(t, rec); got != "engine_off" {
		t.Errorf("код отказа = %q, ожидался engine_off", got)
	}
	if s.currentWatch() != nil {
		t.Error("сессия завелась при выключенном движке")
	}
}

// TestWatchGetWithoutSession: наблюдение не запущено — нормальное
// состояние, а не ошибка.
func TestWatchGetWithoutSession(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()

	rec := watchReq(t, s, "GET", "/api/watch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	var st watch.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("тело не разобрано: %v", err)
	}
	if st.Active {
		t.Error("без сессии ответ называет наблюдение активным")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q: состояние наблюдения не должно оседать в кэше", got)
	}
}

// TestWatchStartThenGetThenDelete: весь цикл сессии.
func TestWatchStartThenGetThenDelete(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()

	rec := watchReq(t, s, "POST", "/api/watch", `{"ip":"192.168.9.219"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("старт: код = %d (%s)", rec.Code, rec.Body.String())
	}
	var st watch.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("тело не разобрано: %v", err)
	}
	if !st.Active || st.IP != "192.168.9.219" || st.Engine != watch.EngineRunning {
		t.Fatalf("состояние после старта = %+v", st)
	}

	if rec := watchReq(t, s, "GET", "/api/watch", ""); rec.Code != http.StatusOK {
		t.Fatalf("чтение: код = %d", rec.Code)
	}

	rec = watchReq(t, s, "DELETE", "/api/watch", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("остановка: код = %d, ожидался 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("у 204 есть тело: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control у 204 = %q", got)
	}
	if s.currentWatch() != nil {
		t.Error("после DELETE сессия жива")
	}
	// Повторная остановка — не ошибка: владелец мог нажать дважды, а вкладка
	// могла уйти сама.
	if rec := watchReq(t, s, "DELETE", "/api/watch", ""); rec.Code != http.StatusNoContent {
		t.Errorf("повторная остановка: код = %d", rec.Code)
	}
}

// TestWatchStartReplacesSession: выбор другого устройства ЗАМЕНЯЕТ сессию.
//
// Две сессии сразу означали бы два потока к журналу движка, а mihomo молча
// выбрасывает события при переполнении канала подписчика.
func TestWatchStartReplacesSession(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()

	watchReq(t, s, "POST", "/api/watch", `{"ip":"192.168.9.219"}`)
	first := s.currentWatch()
	if first == nil {
		t.Fatal("первая сессия не завелась")
	}
	watchReq(t, s, "POST", "/api/watch", `{"ip":"192.168.9.163"}`)
	second := s.currentWatch()
	if second == nil || second == first {
		t.Fatal("вторая сессия не заменила первую")
	}
	select {
	case <-first.Done():
	default:
		t.Error("прежняя сессия осталась крутиться: два потока к движку сразу")
	}
	if got := s.currentWatch().State().IP; got != "192.168.9.163" {
		t.Errorf("наблюдается %q", got)
	}
}

// TestServerCloseStopsSession: остановка демона гасит и наблюдение.
func TestServerCloseStopsSession(t *testing.T) {
	s, _ := newServer(t)
	watchReq(t, s, "POST", "/api/watch", `{"ip":"192.168.9.219"}`)
	sess := s.currentWatch()
	if sess == nil {
		t.Fatal("сессия не завелась")
	}
	s.Close()
	select {
	case <-sess.Done():
	default:
		t.Error("Close вернулся, оставив поток к движку и две горутины")
	}
}

// TestWatchHostsMergesSources: список устройств из трёх источников.
func TestWatchHostsMergesSources(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()

	dir := t.TempDir()
	leases := filepath.Join(dir, "dhcp.leases")
	if err := os.WriteFile(leases, []byte(
		"1789370216 02:00:00:00:00:01 192.168.9.207 neural 01:02\n"+
			"1789386146 02:00:00:00:00:02 192.168.9.219 * 01:02\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.LeasesPath = leases
	// Форма ответа assoclist в разведке НЕ снята целиком (raw/91 прошёл через
	// grep mac), поэтому здесь она синтетическая — и ровно поэтому же
	// разборщик терпимый: см. TestWatchHostsSurvivesUnknownAssocList.
	f.Fixtures["ubus iwinfo assoclist"] = []byte(`{"results":[{"mac":"02:00:00:00:00:02"}]}`)

	rec := watchReq(t, s, "GET", "/api/watch/hosts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct{ Hosts []watch.Host }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не разобрано: %v", err)
	}
	byIP := map[string]watch.Host{}
	for _, h := range body.Hosts {
		byIP[h.IP] = h
	}
	if got := byIP["192.168.9.219"]; got.Kind != watch.KindWireless {
		t.Errorf("устройство из assoclist = %+v, ожидалось беспроводное", got)
	}
	if got := byIP["192.168.9.207"]; got.Kind != watch.KindWired || got.Name != "neural" {
		t.Errorf("устройство вне assoclist = %+v, ожидался кабель", got)
	}
}

// TestWatchHostsSurvivesUnknownAssocList: точка доступа не ответила — вид
// связи НЕИЗВЕСТЕН, а не «кабель».
//
// Записать всех в кабель значило бы соврать про каждое устройство сразу, и
// соврать правдоподобно: строка «кабель» у телефона не выглядит ошибкой.
func TestWatchHostsSurvivesUnknownAssocList(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()

	dir := t.TempDir()
	leases := filepath.Join(dir, "dhcp.leases")
	if err := os.WriteFile(leases, []byte("1789370216 02:00:00:00:00:01 192.168.9.207 neural 01:02\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.LeasesPath = leases
	f.Errors["ubus iwinfo assoclist"] = errors.New("точка доступа не ответила")

	rec := watchReq(t, s, "GET", "/api/watch/hosts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d: список устройств нужен панели и при лежащей точке", rec.Code)
	}
	var body struct{ Hosts []watch.Host }
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Hosts) != 1 || body.Hosts[0].Kind != watch.KindUnknown {
		t.Errorf("устройства = %+v, ожидался единственный с неизвестным видом связи", body.Hosts)
	}
}

// TestWatchHostsWithoutLeasesFile: аренд нет — список всё равно есть.
//
// dnsmasq может быть выключен, а устройства со статикой всё равно говорят, и
// отказ здесь лишил бы владельца единственного способа их увидеть.
func TestWatchHostsWithoutLeasesFile(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()
	s.cfg.LeasesPath = filepath.Join(t.TempDir(), "нет-такого-файла")

	rec := watchReq(t, s, "GET", "/api/watch/hosts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d", rec.Code)
	}
}

// TestWatchSessionReadsEngine: строки из подделанного движка доезжают до
// ответа.
func TestWatchSessionReadsEngine(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()

	fake := newFakeNikkiClient()
	fake.snapshots = []nikki.Snapshot{{
		UploadTotal: 1000,
		Connections: []nikki.Conn{{
			ID: "a", Net: "tcp", SourceIP: "192.168.9.219", SourcePort: 50317,
			Host: "api.anthropic.com", RemoteDestination: "154.222.132.99", DestinationPort: 443,
			Upload: 3456, Download: 4953, Start: time.Now().Add(-time.Minute),
			Chains: []string{"🇫🇷⚡Франция", "PROXY", "BYPASS"},
			Rule:   "RuleSet", RulePayload: "nm-geosite-anthropic",
		}},
	}}
	s.SetNikkiClient(fake)

	if rec := watchReq(t, s, "POST", "/api/watch", `{"ip":"192.168.9.219"}`); rec.Code != http.StatusOK {
		t.Fatalf("старт: %d (%s)", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	var st watch.State
	for time.Now().Before(deadline) {
		rec := watchReq(t, s, "GET", "/api/watch", "")
		json.Unmarshal(rec.Body.Bytes(), &st)
		if len(st.Targets) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(st.Targets) == 0 {
		t.Fatal("за три секунды в ответе не появилось ни одного адресата")
	}
	got := st.Targets[0]
	if got.Name != "api.anthropic.com" {
		t.Errorf("адресат = %q", got.Name)
	}
	if got.Chain != "BYPASS[🇫🇷⚡Франция]" {
		t.Errorf("цепочка = %q", got.Chain)
	}
	if got.Up != 3456 || got.Down != 4953 {
		t.Errorf("байты = ↑%d ↓%d", got.Up, got.Down)
	}
}
