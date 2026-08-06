package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netmoded/internal/b4"
	"netmoded/internal/executor"
	"netmoded/internal/luci"
	"netmoded/internal/nikki"
)

const testToken = "dGVzdC10b2tlbi1iYXNlNjR1cmwtMzJieXRlcw"

// testNikkiSecret стоит во ВСЕХ тестовых серверах намеренно.
//
// Секрет Clash API имеет право уехать наружу ровно одним путём —
// GET /api/nikki/panel. Держать его пустым в помощнике значило бы, что
// проверка «секрет не протёк» сводится к поиску пустой строки, то есть не
// проверяет ничего. Значение приметное, чтобы искать подстрокой.
const testNikkiSecret = "nikki-secret-must-not-leak"

// testNikkiURL — то, что даёт nikkiURL() в cmd/netmoded из
// nikki.mixin.api_listen. Хост тут для ХОЖДЕНИЯ демона по петле; в ссылку
// для браузера уезжает только порт.
const testNikkiURL = "http://127.0.0.1:9090"

// NewServer обязан выдать читателю статуса те же клиенты, что и себе.
//
// Проверка идёт боевым путём — через NewServer, БЕЗ SetB4Client и
// SetNikkiClient. Именно подмена клиентов в помощниках прятала настоящий
// дефект: поле nikki у читателя не заполнялось нигде, кроме подменялки, и в
// бою /api/status вечно докладывал «Clash API недоступен», пока
// GET /api/nikki/proxies рядом отвечал списком узлов. Тест, который сначала
// всё подставит, такую дыру увидеть не может по построению.
func TestNewServerWiresClientsIntoStatus(t *testing.T) {
	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))

	s, err := NewServer(Config{
		Listen:  "192.168.9.1",
		Port:    8088,
		Token:   testToken,
		LogPath: filepath.Join(t.TempDir(), "updates.log"),
	}, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if s.status.nikki == nil {
		t.Error("читателю статуса не отдан клиент Nikki: блок чтения будет пропущен, статус соврёт «недоступен»")
	}
	if s.status.nikki != s.nikki {
		t.Error("читатель статуса и обработчики ходят к разным клиентам Nikki")
	}
	if s.status.b4 == nil {
		t.Error("читателю статуса не отдан клиент b4")
	}
	if s.status.jobs == nil || s.status.logs == nil {
		t.Error("читателю статуса не отданы джобы или журнал")
	}
}

func newServer(t *testing.T) (*Server, *executor.Fake) {
	t.Helper()
	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.UCIValues["netmode.main.mode"] = "nikki"
	f.UCIValues["system.@system[0].hostname"] = "grenderRouter"

	// Журнал — во временный каталог: путь по умолчанию (/etc/nikki) на
	// машине разработчика недоступен, и запись молча проваливалась бы.
	s, err := NewServer(Config{
		Listen:      "192.168.9.1",
		Port:        8088,
		Token:       testToken,
		NikkiURL:    testNikkiURL,
		NikkiSecret: testNikkiSecret,
		LogPath:     filepath.Join(t.TempDir(), "updates.log"),
	}, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.SetB4Client(newFakeB4Client())
	s.SetNikkiClient(newFakeNikkiClient())
	return s, f
}

// nikkiPanelURL делает GET /api/nikki/panel с заданным Host и возвращает
// ответ целиком: телом интересуется и проверка кода отказа тоже.
func nikkiPanelURL(t *testing.T, s *Server, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/nikki/panel", nil)
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
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
	// Внутри links тот же уговор: ключ присутствует всегда, даже как null.
	// Пропавший ключ читается клиентом как «демон не прислал», а не как
	// «адрес не собрался», и панель не отличит старую сборку от непригодного
	// Host.
	links, ok := got["links"].(map[string]any)
	if !ok {
		t.Fatalf("links не объект: %#v", got["links"])
	}
	for _, k := range []string{"nikki", "b4", "luci"} {
		if _, ok := links[k]; !ok {
			t.Errorf("в links нет ключа %q", k)
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

// Заглушка 501 на POST /api/upstream жила здесь всю первую фазу и удалена
// вместе с ней: оба блокера ADR-0016 закрыты замером, обработчик настоящий.
// Проверки переключения — в upstreamhandler_test.go.

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

// Сторож завершения — единственная горутина сервера: он ждёт отмену
// контекста и гасит http.Server. Паника в нём унесла бы процесс мимо всякого
// запроса, уже на выходе, — и в логе не осталось бы ничего, кроме внезапной
// смерти демона.
func TestShutdownWatchdogSurvivesPanic(t *testing.T) {
	sink := &logSink{}
	s, err := NewServer(Config{
		Listen:  "192.168.9.1",
		Port:    8088,
		Token:   testToken,
		LogPath: filepath.Join(t.TempDir(), "updates.log"),
		Logf:    sink.logf,
	}, executor.NewFake())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Запускаем именно горутиной, как в ListenAndServe: recover действует
	// только в той горутине, где случилась паника, и тест обязан проверять
	// ту же расстановку, что работает на роутере.
	done := make(chan struct{})
	go func() {
		s.awaitShutdown(ctx, func(context.Context) error {
			panic("подложенный сбой остановки")
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("сторож завершения не вернулся")
	}
	if len(sink.matching("паника")) == 0 {
		t.Error("паника сторожа не попала в журнал демона")
	}
	if len(sink.matching("server.go")) == 0 {
		t.Error("в журнале нет стека — по такой записи не найти место паники")
	}
}

// ─────────── ссылки на веб-морды (links) ───────────

// statusLinks делает GET /api/status с заданным заголовком Host.
//
// Host выставляется полем req.Host, а не заголовком: net/http на стороне
// сервера читает именно его, и Header.Set("Host", ...) был бы проигнорирован.
func statusLinks(t *testing.T, s *Server, host string) Links {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Host %q: код %d", host, rec.Code)
	}
	var got struct {
		Links Links `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Host %q: разбор ответа: %v", host, err)
	}
	return got.Links
}

// Ссылка обязана строиться по Host КАЖДОГО запроса, а не по снимку кэша.
//
// Оба запроса идут внутри одного TTL, то есть второй обслуживается из кэша.
// Если бы links заполнялись в build, второй клиент получил бы адрес первого —
// то есть ссылку на чужой хост. Панель на роутере опрашивается двумя
// вкладками одновременно сплошь и рядом.
func TestStatusLinksFollowRequestHostNotCache(t *testing.T) {
	s, _ := newServer(t)
	base := time.Unix(1700000000, 0)
	s.status.now = func() time.Time { return base }

	first := statusLinks(t, s, "192.168.9.1:8088")
	if first.B4 == nil {
		t.Fatal("links.b4 пуст при годном Host — кнопка панели мертва")
	}
	if *first.B4 != "http://192.168.9.1:7000/" {
		t.Errorf("links.b4 = %q, ожидалось http://192.168.9.1:7000/", *first.B4)
	}

	if first.LuCI == nil {
		t.Fatal("links.luci пуст при годном Host — кнопка веб-морды роутера мертва")
	}
	if *first.LuCI != "http://192.168.9.1"+luci.PanelPath {
		t.Errorf("links.luci = %q, ожидалось http://192.168.9.1%s", *first.LuCI, luci.PanelPath)
	}

	second := statusLinks(t, s, "router.lan:8088")
	if second.B4 == nil {
		t.Fatal("links.b4 пуст для Host без IP")
	}
	if *second.B4 != "http://router.lan:7000/" {
		t.Errorf("второй клиент получил %q — ссылка приехала из кэша первого", *second.B4)
	}
	if second.LuCI == nil {
		t.Fatal("links.luci пуст для Host без IP")
	}
	if *second.LuCI != "http://router.lan"+luci.PanelPath {
		t.Errorf("второй клиент получил luci=%q — ссылка приехала из кэша первого", *second.LuCI)
	}
}

// Асимметрия ADR-0024, раздел «Границы правила», в виде теста.
//
// Оба движка лежат — links.luci обязан остаться на месте. uhttpd не
// управляется netmode-apply: мы его не гасим, гаснуть по нашей вине он не
// может, и правило «кнопка только когда служба отвечает» на него не
// распространяется. Без этого теста первая же правка «привести luci к общему
// виду» отняла бы у владельца единственную работающую кнопку ровно тогда,
// когда оба движка не поднялись и он полез разбираться в веб-морду роутера.
func TestStatusLuCILinkSurvivesBothEnginesDown(t *testing.T) {
	s, _ := newServer(t)
	s.SetB4Client(&fakeB4Client{err: b4.ErrUnavailable})
	s.SetNikkiClient(&fakeNikkiClient{err: nikki.ErrUnavailable})

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "192.168.9.1:8088"
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}

	var got struct {
		Links Links `json:"links"`
		Nikki struct {
			Available bool `json:"available"`
		} `json:"nikki"`
		B4 struct {
			Available bool `json:"available"`
		} `json:"b4"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if got.B4.Available || got.Nikki.Available {
		t.Fatalf("движки не легли: b4=%v nikki=%v — тест проверяет не то", got.B4.Available, got.Nikki.Available)
	}
	if got.Links.LuCI == nil {
		t.Fatal("links.luci обнулился вместе с движками: веб-морда роутера от netmode-apply не зависит (ADR-0024, «Границы правила»)")
	}
	if *got.Links.LuCI != "http://192.168.9.1"+luci.PanelPath {
		t.Errorf("links.luci = %q", *got.Links.LuCI)
	}
}

// IPv6 в Host доезжает до ссылки скобленным.
//
// Сквозная проверка: panelHost скобки снимает, luci.PanelURL обязан вернуть
// их обратно. Порта у адреса LuCI нет, поэтому net.JoinHostPort здесь не
// работает, и потерять скобки легко — а без них браузер уйдёт на другой хост.
func TestStatusLuCILinkBracketsIPv6Host(t *testing.T) {
	s, _ := newServer(t)
	l := statusLinks(t, s, "[fd00::1]:8088")
	if l.LuCI == nil {
		t.Fatal("links.luci пуст при IPv6 в Host")
	}
	if want := "http://[fd00::1]" + luci.PanelPath; *l.LuCI != want {
		t.Errorf("links.luci = %q, ожидалось %q", *l.LuCI, want)
	}
}

// ГЛАВНЫЙ тест решения: секрет Clash API не уезжает в /api/status.
//
// Статус опрашивается раз в секунду и любой вкладкой, у которой есть токен;
// секрет в нём осел бы в кэше браузера, в devtools и в любом логе, который
// кто-нибудь снимет с панели. Разрешённый путь для него ровно один —
// GET /api/nikki/panel, по клику и с no-store.
//
// Это единственный механический сторож решения: положить полный адрес
// (с ?secret=) в links.nikki — правка на одну строку, и без этого теста она
// прошла бы ревью как «упрощение, зачем два запроса».
func TestStatusNeverLeaksSecret(t *testing.T) {
	s, _ := newServer(t)
	if s.cfg.NikkiSecret == "" {
		t.Fatal("тестовый сервер без секрета: проверка ничего не проверяет")
	}

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "192.168.9.1:8088"
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), s.cfg.NikkiSecret) {
		t.Errorf("секрет Clash API уехал в /api/status:\n%s", rec.Body.String())
	}
}

// links.nikki — адрес БЕЗ параметров: «куда идти». Хост из Host запроса,
// порт из конфигурации nikki.
//
// Слушаем 192.168.9.1, а спрашиваем с Host: router.lan — если бы адрес
// собирался из cfg.Listen, тест это увидел бы. Владелец ходит на роутер по
// имени из dnsmasq не реже, чем по адресу, и ссылка обязана вести туда же,
// откуда он пришёл: иначе она уводит на адрес, который в его сети может
// быть не маршрутизируем вовсе.
func TestStatusNikkiLinkFollowsHostAndConfiguredPort(t *testing.T) {
	s, _ := newServer(t)
	l := statusLinks(t, s, "router.lan:8088")
	if l.Nikki == nil {
		t.Fatal("links.nikki пуст при годном Host и заданном api_listen — кнопка мертва")
	}
	if *l.Nikki != "http://router.lan:9090/ui/" {
		t.Errorf("links.nikki = %q, ожидалось http://router.lan:9090/ui/", *l.Nikki)
	}
	if strings.Contains(*l.Nikki, s.cfg.Listen) {
		t.Errorf("в ссылке адрес из конфигурации, а не из Host: %q", *l.Nikki)
	}
}

// Порт вывести неоткуда — ссылки нет. Кнопка в никуда хуже отсутствующей.
func TestStatusNikkiLinkNullWithoutAPIListen(t *testing.T) {
	s, _ := newServer(t)
	s.cfg.NikkiURL = ""
	if l := statusLinks(t, s, "192.168.9.1:8088"); l.Nikki != nil {
		t.Errorf("links.nikki = %q при пустом api_listen: порт взялся из воздуха", *l.Nikki)
	}
}

// ─────────── GET /api/nikki/panel ───────────

// Полный адрес собирается в Go и отдаётся целиком.
//
// Целиком, а не частями: шаблон (/ui/, четыре параметра, query вместо hash)
// — внешнее знание, снятое с LuCI, и жить оно обязано рядом с evidence.json.
// Собранный в панели из кусков, он не проверялся бы ни одним гейтом.
func TestNikkiPanelReturnsFullURLWithSecret(t *testing.T) {
	s, _ := newServer(t)
	rec := nikkiPanelURL(t, s, "router.lan:8088")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, тело %s", rec.Code, rec.Body.String())
	}

	var got struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}

	u, err := url.Parse(got.URL)
	if err != nil {
		t.Fatalf("адрес не разбирается: %v (%q)", err, got.URL)
	}
	if u.Scheme != "http" || u.Host != "router.lan:9090" || u.Path != "/ui/" {
		t.Errorf("адрес = %q, ожидалось http://router.lan:9090/ui/…", got.URL)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"host":     "router.lan",
		"hostname": "router.lan",
		"port":     "9090",
		"secret":   testNikkiSecret,
	} {
		if q.Get(k) != want {
			t.Errorf("параметр %s = %q, ожидалось %q", k, q.Get(k), want)
		}
	}
}

// no-store обязателен: в теле секрет, и его копия в кэше пережила бы ротацию.
func TestNikkiPanelIsNotCacheable(t *testing.T) {
	s, _ := newServer(t)
	rec := nikkiPanelURL(t, s, "192.168.9.1:8088")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, ожидалось no-store: ответ с секретом кэшируем", cc)
	}
}

// Три отказа, все 503 и все различимы кодом: чинятся они по-разному.
func TestNikkiPanelRefusals(t *testing.T) {
	// Открыто через ssh-туннель: собирать адрес не из чего, а угадывать
	// демон не будет.
	t.Run("host_unknown", func(t *testing.T) {
		s, _ := newServer(t)
		rec := nikkiPanelURL(t, s, "localhost:8088")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("код %d, ожидался 503", rec.Code)
		}
		if c := errCode(t, rec); c != "host_unknown" {
			t.Errorf("код отказа %q, ожидался host_unknown", c)
		}
	})

	// Секрета нет — панель откроется, но не войдёт. Отдавать адрес,
	// который заведомо не работает, значит врать кнопкой.
	t.Run("nikki_unconfigured", func(t *testing.T) {
		s, _ := newServer(t)
		s.cfg.NikkiSecret = ""
		rec := nikkiPanelURL(t, s, "192.168.9.1:8088")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("код %d, ожидался 503", rec.Code)
		}
		if c := errCode(t, rec); c != "nikki_unconfigured" {
			t.Errorf("код отказа %q, ожидался nikki_unconfigured", c)
		}
	})

	// Статики по /ui/ нет: дашборд не скачан. Без живой пробы секрет уехал
	// бы в историю браузера ради страницы 404.
	t.Run("panel_missing", func(t *testing.T) {
		s, _ := newServer(t)
		fake := newFakeNikkiClient()
		fake.panelErr = nikki.ErrPanelMissing
		s.SetNikkiClient(fake)
		rec := nikkiPanelURL(t, s, "192.168.9.1:8088")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("код %d, ожидался 503", rec.Code)
		}
		if c := errCode(t, rec); c != "panel_missing" {
			t.Errorf("код отказа %q, ожидался panel_missing", c)
		}
	})
}

// Ни один исход не пишет в журнал ни адрес, ни секрет.
//
// Журнал демона уходит в syslog роутера, а оттуда — в любой снимок, который
// владелец пришлёт с жалобой «кнопка не работает». Секрет в такой строке
// переживает и ротацию, и удаление панели.
func TestNikkiPanelNeverLogsSecret(t *testing.T) {
	s, _ := newServer(t)
	var sink strings.Builder
	s.logf = func(f string, a ...any) { fmt.Fprintf(&sink, f+"\n", a...) }
	s.status.logf = s.logf

	// Все четыре исхода эндпоинта, а не только удачный: текст отказа —
	// самое место, где адрес просачивается через err.Error().
	nikkiPanelURL(t, s, "router.lan:8088") // 200
	nikkiPanelURL(t, s, "localhost:8088")  // host_unknown

	secret := s.cfg.NikkiSecret
	s.cfg.NikkiSecret = ""
	nikkiPanelURL(t, s, "router.lan:8088") // nikki_unconfigured
	s.cfg.NikkiSecret = secret

	fake := newFakeNikkiClient()
	fake.panelErr = nikki.ErrPanelMissing
	s.SetNikkiClient(fake)
	nikkiPanelURL(t, s, "router.lan:8088") // panel_missing

	if strings.Contains(sink.String(), testNikkiSecret) {
		t.Errorf("секрет попал в журнал демона:\n%s", sink.String())
	}
	if strings.Contains(sink.String(), nikki.PanelPath) {
		t.Errorf("адрес панели попал в журнал демона:\n%s", sink.String())
	}
}

// Панель, открытая через ssh-туннель, не должна получать ссылку на себя же.
func TestStatusLinksRefuseLoopbackHost(t *testing.T) {
	s, _ := newServer(t)
	l := statusLinks(t, s, "localhost:8088")
	if l.B4 != nil {
		t.Errorf("links.b4 = %q при Host=localhost: ссылка ведёт на ноутбук владельца, а не на роутер", *l.B4)
	}
	// Веб-морда роутера — тот же случай: на 80-м порту ноутбука владельца
	// либо ничего нет, либо его собственный сервис. Отказ от гейта по
	// живости причин отказать НЕпригодному Host не отменяет.
	if l.LuCI != nil {
		t.Errorf("links.luci = %q при Host=localhost: ссылка ведёт на ноутбук владельца, а не на роутер", *l.LuCI)
	}
}

func TestPanelHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"192.168.9.1:8088", "192.168.9.1", true},
		// Без порта: SplitHostPort здесь ошибается штатно, и это не сбой.
		{"router.lan", "router.lan", true},
		{"[fd00::1]:8088", "fd00::1", true},
		// IPv6 без порта — скобки снимаем сами, SplitHostPort его не берёт.
		{"[fd00::1]", "fd00::1", true},
		// HTTP/1.0 без заголовка — собирать ссылку не из чего.
		{"", "", false},
		// Петля во всех видах: ссылка указывала бы на машину владельца.
		{"localhost:8088", "", false},
		{"LocalHost", "", false},
		{"router.localhost", "", false},
		{"127.0.0.1:8088", "", false},
		{"[::1]:8088", "", false},
		// Мусор в Host уезжает в href — отказ, а не экранирование.
		{"evil host/x", "", false},
		{"evil\"onclick=x", "", false},
	}
	for _, c := range cases {
		got, ok := panelHost(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("panelHost(%q) = (%q, %v), ожидалось (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
