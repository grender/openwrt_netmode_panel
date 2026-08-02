package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"netmoded/internal/executor"
)

const testToken = "dGVzdC10b2tlbi1iYXNlNjR1cmwtMzJieXRlcw"

func newServer(t *testing.T) (*Server, *executor.Fake) {
	t.Helper()
	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.UCIValues["netmode.main.mode"] = "nikki"
	f.UCIValues["system.@system[0].hostname"] = "grenderRouter"

	// Журнал — во временный каталог: путь по умолчанию (/etc/nikki) на
	// машине разработчика недоступен, и запись молча проваливалась бы.
	s, err := NewServer(Config{
		Listen:  "192.168.9.1",
		Port:    8088,
		Token:   testToken,
		LogPath: filepath.Join(t.TempDir(), "updates.log"),
	}, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.SetB4Client(newFakeB4Client())
	s.SetNikkiClient(newFakeNikkiClient())
	return s, f
}

func do(t *testing.T, s *Server, method, path string, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if withToken {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Демон не выставляется наружу ни при каких условиях: это граница
// безопасности, а не настройка с неудачным значением по умолчанию.
func TestRefusesToListenOnAllInterfaces(t *testing.T) {
	f := executor.NewFake()
	bad := []string{"0.0.0.0", "::", "", "не адрес"}
	for _, addr := range bad {
		if _, err := NewServer(Config{Listen: addr, Port: 8088, Token: testToken}, f); err == nil {
			t.Errorf("адрес %q принят, ожидался отказ запуска", addr)
		}
	}
	// LAN-адрес роутера — принимается.
	if _, err := NewServer(Config{Listen: "192.168.9.1", Port: 8088, Token: testToken}, f); err != nil {
		t.Errorf("LAN-адрес отвергнут: %v", err)
	}
}

func TestRefusesBadTokens(t *testing.T) {
	f := executor.NewFake()
	bad := map[string]string{
		"пустой":          "",
		"короткий":        "abc123",
		"кириллица":       "токен-который-достаточно-длинный",
		"пробел":          "token with a space in it",
		"точка с запятой": "token;with;semicolons;here",
		"кавычка":         "token\"with\"quotes\"here",
	}
	for name, tok := range bad {
		if _, err := NewServer(Config{Listen: "192.168.9.1", Port: 8088, Token: tok}, f); err == nil {
			t.Errorf("%s: токен %q принят, ожидался отказ", name, tok)
		}
	}
	// Настоящая форма — base64url от crypto/rand.
	if _, err := NewServer(Config{Listen: "192.168.9.1", Port: 8088,
		Token: "dGVzdC10b2tlbi1iYXNlNjR1cmwtMzJieXRlcw"}, f); err != nil {
		t.Errorf("корректный токен отвергнут: %v", err)
	}
}

// Токен проверяется и на статике: панель раскрывает имена сетей и
// топологию, поэтому защищать только API означало бы оставить разведку
// целей открытой.
func TestTokenRequiredEverywhereIncludingStatic(t *testing.T) {
	s, _ := newServer(t)

	for _, path := range []string{"/api/status", "/api/wifi/networks", "/", "/app.js"} {
		if rec := do(t, s, "GET", path, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s без токена → %d, ожидался 401", path, rec.Code)
		}
		if rec := do(t, s, "GET", path, true); rec.Code == http.StatusUnauthorized {
			t.Errorf("%s с токеном → 401", path)
		}
	}
}

func TestTokenAcceptedViaCookieAndQuery(t *testing.T) {
	s, _ := newServer(t)

	// Кука нужна статике: браузер не пошлёт заголовок за <script src>.
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: "netmode_token", Value: testToken})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("кука → %d, ожидался 200", rec.Code)
	}

	rec = do(t, s, "GET", "/api/status?token="+testToken, false)
	if rec.Code != http.StatusOK {
		t.Errorf("query → %d, ожидался 200", rec.Code)
	}

	// Похожий, но неверный токен не проходит.
	rec = do(t, s, "GET", "/api/status?token="+testToken+"x", false)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("неверный токен → %d, ожидался 401", rec.Code)
	}
}

func TestStatusEndpointShape(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/status", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	// Статус не кэшируется браузером: он опрашивается раз в секунду и
	// обязан быть свежим.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, ожидался no-store", cc)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	// Поля, которых требует панель, обязаны присутствовать — даже как null.
	for _, k := range []string{
		"generated_at", "hostname", "mode", "selection_state",
		"configured_ssid", "associated_ssid", "pending_apply",
		"wireless_fingerprint", "ap", "online", "links", "job", "last_fail",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("в ответе нет поля %q", k)
		}
	}
}

func TestWifiNetworksEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/wifi/networks", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Fingerprint    string `json:"fingerprint"`
		SelectionState string `json:"selection_state"`
		Networks       []struct {
			ID       string `json:"id"`
			SSID     string `json:"ssid"`
			HasKey   bool   `json:"has_key"`
			Enabled  bool   `json:"enabled"`
			Editable bool   `json:"editable"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.SelectionState != "single" || len(got.Networks) != 2 {
		t.Fatalf("состояние %q, сетей %d", got.SelectionState, len(got.Networks))
	}
	if got.Networks[0].SSID != "John24" || !got.Networks[0].Enabled {
		t.Errorf("первая сеть: %+v", got.Networks[0])
	}
	if got.Networks[0].Editable {
		t.Error("активная сеть не редактируется в фазе 1 (ADR-0009)")
	}
	// Пароль не покидает роутер ни в каком виде.
	if strings.Contains(rec.Body.String(), "REDACTED_PSK") {
		t.Error("значение key просочилось в ответ")
	}
}

func TestWifiScanEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/wifi/scan", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Ifname   string `json:"ifname"`
		Band     string `json:"band"`
		Note     string `json:"note"`
		Networks []struct {
			SSID   string `json:"ssid"`
			Hidden bool   `json:"hidden"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	// Имя выведено из живого состояния, а не захардкожено.
	if got.Ifname != "phy0.0-sta0" {
		t.Errorf("ifname = %q", got.Ifname)
	}
	if got.Band != "2g" || got.Note == "" {
		t.Error("панель обязана знать, что 5 ГГц сетей тут не будет")
	}
	if len(got.Networks) != 14 {
		t.Fatalf("сетей %d, ожидалось 14", len(got.Networks))
	}
	hidden := 0
	for _, n := range got.Networks {
		if n.Hidden {
			hidden++
		}
	}
	if hidden != 3 {
		t.Errorf("скрытых %d, ожидалось 3", hidden)
	}
}

func TestScanFailsLoudlyWithoutIfname(t *testing.T) {
	// Без подтверждённого имени интерфейса скан невозможен. Угадывать
	// `wlan0` нельзя: попадём не туда и не заметим.
	s, f := newServer(t)
	f.Fixtures["ubus network.wireless status"] = []byte(`{"radio0":{"up":true,"interfaces":[]}}`)

	rec := do(t, s, "GET", "/api/wifi/scan", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503", rec.Code)
	}
	var e apiError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "ifname_unknown" {
		t.Errorf("код ошибки %q, ожидался ifname_unknown", e.Code)
	}
}

// Скан обязан идти на радио, которое РАБОТАЕТ станцией, а не на radio0.
// Здесь станция на radio1 и на 5 ГГц — прежний хардкод отдал бы имя
// интерфейса домашней точки и подпись «сети 5 ГГц не появятся» поверх
// списка, целиком состоящего из сетей 5 ГГц.
func TestScanFollowsSwappedRadios(t *testing.T) {
	s, f := newServer(t)
	swapRadios(f)

	rec := do(t, s, "GET", "/api/wifi/scan", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Ifname string `json:"ifname"`
		Band   string `json:"band"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Ifname != "phy0.1-sta0" {
		t.Errorf("ifname = %q, ожидался интерфейс станции с radio1", got.Ifname)
	}
	if got.Band != "5g" {
		t.Errorf("band = %q, ожидался 5g — диапазон взят не у того радио", got.Band)
	}
	// Пояснение обязано соответствовать диапазону: «видны только 2.4 ГГц»
	// на пятигигагерцовой станции — не пояснение, а дезинформация ровно
	// там, где владелец ищет пропавшую сеть.
	if !strings.Contains(got.Note, "5 ГГц") || strings.Contains(got.Note, "сети 5 ГГц в этом списке не появятся") {
		t.Errorf("note = %q — текст не следует за диапазоном", got.Note)
	}
}

// Не выяснили радио — отказываем. Взять «первое попавшееся» нельзя: на
// этом роутере первым попадётся домашняя точка.
func TestScanRefusesWhenStationRadioUnknown(t *testing.T) {
	s, f := newServer(t)
	noStationAnywhere(f)

	rec := do(t, s, "GET", "/api/wifi/scan", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503: %s", rec.Code, rec.Body.String())
	}
	var e apiError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "radio_unknown" {
		t.Errorf("код ошибки %q, ожидался radio_unknown", e.Code)
	}
	if e.Error == "" {
		t.Error("пустое тело неотличимо от неисправного сервера")
	}
}

// Переключение внешней сети — вторая фаза. Заглушка обязана объяснять
// причину: пользователь видит кнопку и должен понять, почему она не
// работает, не читая ADR.
func TestUpstreamReturns501WithExplanation(t *testing.T) {
	s, _ := newServer(t)
	req := httptest.NewRequest("POST", "/api/upstream", strings.NewReader(`{"id":"wifinet2"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("код %d, ожидался 501", rec.Code)
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if e.Code != "not_implemented" {
		t.Errorf("код %q", e.Code)
	}
	if len(e.Error) < 40 {
		t.Errorf("объяснение слишком короткое: %q", e.Error)
	}
}

func TestPanelIsEmbedded(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/", "/app.js", "/app.css", "/i18n.js",
		"/vendor/htm-preact-standalone.module.js"} {
		rec := do(t, s, "GET", path, true)
		if rec.Code != http.StatusOK {
			t.Errorf("%s → %d, панель должна быть встроена в бинарь", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s пуст", path)
		}
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	s, _ := newServer(t)
	if rec := do(t, s, "GET", "/api/нет-такого", true); rec.Code != http.StatusNotFound {
		t.Errorf("неизвестный маршрут → %d, ожидался 404", rec.Code)
	}
}

// Регрессия на пустую панель.
//
// Симптом был обманчив: GET / возвращал 200, то есть выглядело, будто всё
// работает, — а страница оставалась чёрной. За <script src> браузер не шлёт
// ни заголовок Authorization, ни query из адреса страницы, поэтому app.js
// получал 401 и модуль не грузился.
func TestQueryTokenSetsCookieSoSubresourcesLoad(t *testing.T) {
	s, _ := newServer(t)

	rec := do(t, s, "GET", "/?token="+testToken, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("страница по ссылке с токеном → %d", rec.Code)
	}

	var jar *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "netmode_token" {
			jar = c
		}
	}
	if jar == nil {
		t.Fatal("кука не поставлена — подресурсы страницы получат 401, панель останется пустой")
	}
	if jar.Value != testToken || jar.Path != "/" {
		t.Errorf("кука = %+v", jar)
	}
	if !jar.HttpOnly {
		t.Error("кука должна быть HttpOnly: скриптам она не нужна")
	}
	if jar.Secure {
		t.Error("Secure нельзя: демон слушает LAN по http, браузер такую куку не сохранит")
	}

	// Именно так браузер запросит app.js — только с кукой, без query.
	req := httptest.NewRequest("GET", "/app.js", nil)
	req.AddCookie(jar)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Errorf("app.js с кукой → %d, ожидался 200", rec2.Code)
	}
}

func TestFreshQueryTokenOverridesStaleCookie(t *testing.T) {
	// После ротации токена ссылка со свежим значением обязана сработать,
	// не заставляя чистить куки руками.
	s, _ := newServer(t)
	req := httptest.NewRequest("GET", "/api/status?token="+testToken, nil)
	req.AddCookie(&http.Cookie{Name: "netmode_token", Value: "stale-token-value"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("свежий query при протухшей куке → %d, ожидался 200", rec.Code)
	}
}

func TestNoCookieWhenTokenCameFromHeader(t *testing.T) {
	// API-клиент куку не просил — навязывать её незачем.
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/status", true)
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("на заголовок Authorization поставлена кука: %v", rec.Result().Cookies())
	}
}
