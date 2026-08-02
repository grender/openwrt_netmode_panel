package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, s *Server, path, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func del(t *testing.T, s *Server, path, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func etag(t *testing.T, s *Server) string {
	t.Helper()
	rec := do(t, s, "GET", "/api/wifi/networks", true)
	if e := rec.Header().Get("ETag"); e != "" {
		return e
	}
	t.Fatal("GET не отдал ETag — записывать будет нечем")
	return ""
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело не разбирается как ошибка: %s", rec.Body.String())
	}
	return e.Code
}

// ─────────── создание ───────────

func TestCreateNetworkWritesDisabledOnce(t *testing.T) {
	s, f := newServer(t)

	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"НоваяСеть","encryption":"psk2","key":"пароль12345"}`, etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	calls := f.Calls
	if len(calls) == 0 {
		t.Fatal("ни одной записи не сделано")
	}

	// Секция обязана создаваться выключенной: отсутствие disabled означает
	// «включена», и такая секция немедленно вступила бы в борьбу за radio0.
	var sawDisabled1, sawCommit bool
	for _, c := range calls {
		if strings.Contains(c, ".disabled=") {
			if !strings.HasSuffix(c, ".disabled=1") {
				t.Errorf("disabled записан не единицей: %q", c)
			}
			sawDisabled1 = true
		}
		if c == "commit wireless" {
			sawCommit = true
		}
	}
	if !sawDisabled1 {
		t.Error("новая секция создана БЕЗ disabled — она окажется включённой")
	}
	if !sawCommit {
		t.Error("нет commit — правка осталась в стейджинге")
	}

	// Имя секции — именованное и предсказуемой формы; индексы наружу
	// не выходят никогда.
	named := f.CallsContaining("add-named")
	if len(named) != 1 {
		t.Fatalf("ожидалось одно создание секции, получено %v", named)
	}
	if !strings.Contains(named[0], "wireless.netmode_") {
		t.Errorf("секция названа неожиданно: %q", named[0])
	}
	if strings.Contains(named[0], "@wifi-iface") {
		t.Error("создана анонимная секция — её нельзя адресовать стабильно")
	}
}

func TestCreateSetsRequiredOptions(t *testing.T) {
	s, f := newServer(t)
	post(t, s, "/api/wifi/networks", `{"ssid":"X","encryption":"psk2","key":"пароль12345"}`, etag(t, s))

	joined := strings.Join(f.Calls, "\n")
	// radio0 здесь — не константа кода, а факт снимка роутера: станция
	// действительно живёт там (raw/10, raw/21).
	for _, want := range []string{".device=radio0", ".mode=sta", ".network=wwan", ".ssid=X"} {
		if !strings.Contains(joined, want) {
			t.Errorf("не записано %s\nвызовы:\n%s", want, joined)
		}
	}
}

// Новая секция обязана лечь на то радио, которое работает станцией.
// С прежней константой она легла бы на домашнюю точку — и панель, задуманная
// как физически не способная порвать связь (ADR-0009), правила бы ровно ту
// сеть, через которую владелец в неё и зашёл.
func TestCreateWritesResolvedRadioNotIndex(t *testing.T) {
	s, f := newServer(t)
	swapRadios(f)

	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"psk2","key":"пароль12345"}`, etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	joined := strings.Join(f.Calls, "\n")
	if !strings.Contains(joined, ".device=radio1") {
		t.Errorf("секция создана не на станционном радио\nвызовы:\n%s", joined)
	}
	if strings.Contains(joined, ".device=radio0") {
		t.Errorf("секция создана на радио домашней точки\nвызовы:\n%s", joined)
	}
}

// Список тоже обязан описывать выведенное радио: панель показывает по полю
// `radio`, в каком диапазоне искать сеть.
func TestNetworksListFollowsSwappedRadios(t *testing.T) {
	s, f := newServer(t)
	swapRadios(f)

	rec := do(t, s, "GET", "/api/wifi/networks", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		SelectionState string `json:"selection_state"`
		Radio          struct {
			Device string `json:"device"`
			Band   string `json:"band"`
			Mode   string `json:"mode"`
		} `json:"radio"`
		Networks []struct {
			SSID string `json:"ssid"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Radio.Device != "radio1" || got.Radio.Band != "5g" || got.Radio.Mode != "sta" {
		t.Errorf("radio = %+v, ожидалось radio1/5g/sta", got.Radio)
	}
	// Наши секции переехали вместе с радио — их обязано быть видно.
	if got.SelectionState != "single" || len(got.Networks) != 2 {
		t.Errorf("состояние %q, сетей %d — секции ищутся не на том радио", got.SelectionState, len(got.Networks))
	}
}

// Запись без выясненного радио невозможна: неизвестно даже, какие секции
// наши. Отказ, а не догадка (ADR-0019).
func TestWriteRefusesWhenStationRadioUnknown(t *testing.T) {
	s, f := newServer(t)
	// Отпечаток берём до подмены — просто чтобы заголовок If-Match вообще
	// был. Значение здесь роли не играет: радио разрешается РАНЬШЕ сверки
	// отпечатка, потому что без него неизвестно, о каких секциях идёт речь.
	e := etag(t, s)
	noStationAnywhere(f)

	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"psk2","key":"пароль12345"}`, e)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "radio_unknown" {
		t.Errorf("код ошибки %q, ожидался radio_unknown", code)
	}
	// И ничего не записано: отказ до первой команды uci.
	if len(f.Calls) != 0 {
		t.Errorf("при невыясненном радио сделаны записи: %v", f.Calls)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	s, _ := newServer(t)
	e := etag(t, s)

	tests := []struct{ name, body, wantCode string }{
		{"пустой ssid", `{"ssid":"","encryption":"psk2","key":"пароль12345"}`, "bad_request"},
		{"не JSON", `{`, "bad_request"},
		{"короткий пароль psk2", `{"ssid":"X","encryption":"psk2","key":"кор"}`, "bad_request"},
		{"неизвестное шифрование", `{"ssid":"X","encryption":"wep"}`, "bad_request"},
		{"psk2 без пароля", `{"ssid":"X","encryption":"psk2"}`, "bad_request"},
	}
	for _, tt := range tests {
		rec := post(t, s, "/api/wifi/networks", tt.body, e)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: код %d, ожидался 400 (%s)", tt.name, rec.Code, rec.Body.String())
			continue
		}
		if got := errCode(t, rec); got != tt.wantCode {
			t.Errorf("%s: код ошибки %q, ожидался %q", tt.name, got, tt.wantCode)
		}
	}

	// Открытая сеть без пароля — законна.
	if rec := post(t, s, "/api/wifi/networks", `{"ssid":"Открытая","encryption":"none"}`, e); rec.Code != http.StatusOK {
		t.Errorf("открытая сеть отвергнута: %d %s", rec.Code, rec.Body.String())
	}
}

// ─────────── незнакомые поля ───────────

// Поле `network` до этой правки принималось и молча выбрасывалось: клиент
// получал 200 и секцию на wwan. Принять его нельзя — это смена upstream,
// отложенная до фазы 2 (ADR-0016), — значит остаётся честно отказать.
func TestNetworkFieldIsRejectedNotIgnored(t *testing.T) {
	s, f := newServer(t)

	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"none","network":"lan"}`, etag(t, s))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "unsupported_field" {
		t.Errorf("код ошибки %q, ожидался unsupported_field", got)
	}
	// Текст обязан отсылать к решению, а не просто ругаться: клиент должен
	// понять, что поле не забыли, а отложили.
	if !strings.Contains(rec.Body.String(), "ADR-0016") {
		t.Errorf("в сообщении нет отсылки к ADR-0016: %s", rec.Body.String())
	}
	if len(f.Calls) != 0 {
		t.Errorf("отвергнутый запрос не смеет ничего писать: %v", f.Calls)
	}
}

// Отказ общий, а не про одно поле: любое незнакомое поле означает, что
// клиент и демон расходятся в понимании контракта.
func TestUnknownFieldIsRejected(t *testing.T) {
	s, _ := newServer(t)
	e := etag(t, s)

	for _, body := range []string{
		`{"ssid":"X","encryption":"none","disabled":"0"}`,
		`{"id":"wifinet2","bssid":"de:ad:be:ef:00:01"}`,
		`{"ssid":"X","encryption":"none","Network":"lan"}`,
	} {
		rec := post(t, s, "/api/wifi/networks", body, e)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → код %d, ожидался 400", body, rec.Code)
			continue
		}
		if got := errCode(t, rec); got != "unsupported_field" {
			t.Errorf("%s → код ошибки %q", body, got)
		}
	}
}

// Тела, которые шлёт панель, обязаны проходить: строгость разбора не должна
// стоить записи сетей. Формы взяты из web/app.js (NetworkSheet.submit):
// создание — {ssid, encryption, key?}, правка — {id, key?}.
func TestPanelBodiesStillAccepted(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"создание с паролем", `{"ssid":"X","encryption":"psk2","key":"пароль12345"}`},
		{"создание открытой", `{"ssid":"X","encryption":"none"}`},
		{"правка только пароля", `{"id":"wifinet2","key":"новыйпароль1"}`},
		{"правка без пароля", `{"id":"wifinet2","ssid":"ATOM-2","encryption":"psk2"}`},
	} {
		s, _ := newServer(t)
		rec := post(t, s, "/api/wifi/networks", tt.body, etag(t, s))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: код %d, ожидался 200: %s", tt.name, rec.Code, rec.Body.String())
		}
	}
}

// Второе значение в теле — тот же класс ошибки: часть запроса была бы
// прочитана и отброшена молча.
func TestTrailingJSONIsRejected(t *testing.T) {
	s, f := newServer(t)
	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"none"} {"ssid":"Y","encryption":"none"}`, etag(t, s))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400: %s", rec.Code, rec.Body.String())
	}
	if len(f.Calls) != 0 {
		t.Errorf("отвергнутый запрос не смеет ничего писать: %v", f.Calls)
	}
}

// Имя поля достаётся из текста ошибки encoding/json: отдельного типа для
// этого случая стандартная библиотека не заводит. Если формулировка изменится
// в новой версии Go, упасть обязан этот тест, а не ответ клиенту в проде.
func TestUnknownFieldNameExtracted(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"network":"lan"}`))
	dec.DisallowUnknownFields()

	err := dec.Decode(&NetworkWrite{})
	if err == nil {
		t.Fatal("DisallowUnknownFields не сработал")
	}
	field, ok := unknownField(err)
	if !ok {
		t.Fatalf("ошибка не опознана как «незнакомое поле»: %v", err)
	}
	if field != "network" {
		t.Errorf("имя поля %q, ожидалось network", field)
	}
}

// ─────────── правка ───────────

func TestEditDisabledNetwork(t *testing.T) {
	s, f := newServer(t)
	rec := post(t, s, "/api/wifi/networks",
		`{"id":"wifinet2","ssid":"ATOM-2","key":"новыйпароль1"}`, etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	joined := strings.Join(f.Calls, "\n")
	if !strings.Contains(joined, "set wireless.wifinet2.ssid=ATOM-2") {
		t.Errorf("ssid не записан:\n%s", joined)
	}
	// Правка не смеет трогать disabled — это была бы смена upstream.
	if strings.Contains(joined, "wifinet2.disabled") {
		t.Errorf("правка тронула disabled:\n%s", joined)
	}
}

// Главный запрет фазы 1: активную сеть не трогаем. Смена её пароля порвала
// бы ассоциацию при ближайшем применении, кем бы оно ни было вызвано.
func TestEditEnabledNetworkIs409(t *testing.T) {
	s, f := newServer(t)
	rec := post(t, s, "/api/wifi/networks", `{"id":"wifinet0","ssid":"Другое"}`, etag(t, s))

	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409", rec.Code)
	}
	if got := errCode(t, rec); got != "enabled_network_readonly" {
		t.Errorf("код ошибки %q", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("при отказе не должно быть ни одной записи: %v", f.Calls)
	}
}

func TestDeleteDisabledNetwork(t *testing.T) {
	s, f := newServer(t)
	rec := del(t, s, "/api/wifi/networks/wifinet2", etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	joined := strings.Join(f.Calls, "\n")
	if !strings.Contains(joined, "delete wireless.wifinet2") {
		t.Errorf("секция не удалена:\n%s", joined)
	}
}

func TestDeleteEnabledNetworkIs409(t *testing.T) {
	s, f := newServer(t)
	rec := del(t, s, "/api/wifi/networks/wifinet0", etag(t, s))
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409", rec.Code)
	}
	if got := errCode(t, rec); got != "enabled_network_readonly" {
		t.Errorf("код ошибки %q", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("при отказе записей быть не должно: %v", f.Calls)
	}
}

func TestWriteToForeignSectionIs404(t *testing.T) {
	s, _ := newServer(t)
	e := etag(t, s)

	// wifinet1 — домашняя точка доступа на radio1. Не наша.
	if rec := del(t, s, "/api/wifi/networks/wifinet1", e); rec.Code != http.StatusNotFound {
		t.Errorf("удаление точки доступа → %d, ожидался 404", rec.Code)
	}
	if rec := del(t, s, "/api/wifi/networks/нет-такой", e); rec.Code != http.StatusNotFound {
		t.Errorf("несуществующая секция → %d, ожидался 404", rec.Code)
	}
	// Индексы не принимаются никогда: они сдвигаются при удалении в LuCI.
	for _, bad := range []string{"@wifi-iface[0]", "@wifi-iface%5B0%5D"} {
		rec := del(t, s, "/api/wifi/networks/"+bad, e)
		if rec.Code == http.StatusOK {
			t.Errorf("индекс %q принят как идентификатор", bad)
		}
	}
}

// ─────────── конкурентность ───────────

// Незакоммиченные чужие правки означают, что наш commit опубликует чужую
// работу под своим именем и в момент, который её автор не выбирал.
func TestRefusesOverForeignStagedChanges(t *testing.T) {
	s, f := newServer(t)
	e := etag(t, s)
	f.Staged["wireless"] = "wireless.wifinet2.ssid='кто-то правит'\n"

	rec := post(t, s, "/api/wifi/networks", `{"id":"wifinet2","ssid":"наше"}`, e)
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409", rec.Code)
	}
	if got := errCode(t, rec); got != "foreign_staged_changes" {
		t.Errorf("код ошибки %q", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("записей быть не должно: %v", f.Calls)
	}
}

func TestFingerprintMismatchIs409(t *testing.T) {
	s, f := newServer(t)
	rec := post(t, s, "/api/wifi/networks", `{"id":"wifinet2","ssid":"X"}`, "sha256:чужойотпечаток")
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409", rec.Code)
	}
	if got := errCode(t, rec); got != "fingerprint_mismatch" {
		t.Errorf("код ошибки %q", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("записей быть не должно: %v", f.Calls)
	}
}

func TestWriteWithoutIfMatchIsRefused(t *testing.T) {
	// Без отпечатка клиент не доказал, что видел актуальное состояние.
	s, _ := newServer(t)
	rec := post(t, s, "/api/wifi/networks", `{"id":"wifinet2","ssid":"X"}`, "")
	if rec.Code != http.StatusConflict && rec.Code != http.StatusBadRequest {
		t.Errorf("запись без If-Match прошла с кодом %d", rec.Code)
	}
}

// При неоднозначной конфигурации запрещены ВСЕ записи, включая правку
// выключенных секций: мы не можем доказать, что правка безвредна, пока
// не знаем, какую секцию поднимет netifd.
func TestAmbiguousRefusesEveryWrite(t *testing.T) {
	s, f := newServer(t)
	f.Fixtures["uci show wireless"] = []byte(
		"wireless.radio0=wifi-device\n" +
			"wireless.a=wifi-iface\nwireless.a.device='radio0'\nwireless.a.mode='sta'\n" +
			"wireless.b=wifi-iface\nwireless.b.device='radio0'\nwireless.b.mode='sta'\n" +
			"wireless.c=wifi-iface\nwireless.c.device='radio0'\nwireless.c.mode='sta'\nwireless.c.disabled='1'\n")
	e := etag(t, s)

	cases := []struct {
		name string
		run  func() *httptest.ResponseRecorder
	}{
		{"создание", func() *httptest.ResponseRecorder {
			return post(t, s, "/api/wifi/networks", `{"ssid":"X","encryption":"none"}`, e)
		}},
		{"правка выключенной", func() *httptest.ResponseRecorder {
			return post(t, s, "/api/wifi/networks", `{"id":"c","ssid":"X"}`, e)
		}},
		{"удаление выключенной", func() *httptest.ResponseRecorder {
			return del(t, s, "/api/wifi/networks/c", e)
		}},
	}
	for _, tt := range cases {
		rec := tt.run()
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: код %d, ожидался 409", tt.name, rec.Code)
			continue
		}
		if got := errCode(t, rec); got != "ambiguous_selection" {
			t.Errorf("%s: код ошибки %q", tt.name, got)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("при ambiguous записей быть не должно: %v", f.Calls)
	}
}

// ─────────── пароли ───────────

func TestKeyAbsentMeansUnchanged(t *testing.T) {
	// Отсутствие key и пустой key — разные вещи. Первое «не трогай пароль»,
	// второе «запиши пустой». Слив их, мы стёрли бы пароль при правке ssid.
	s, f := newServer(t)
	post(t, s, "/api/wifi/networks", `{"id":"wifinet2","ssid":"Только имя"}`, etag(t, s))

	for _, c := range f.Calls {
		if strings.Contains(c, ".key=") {
			t.Errorf("пароль тронут, хотя в теле его не было: %q", c)
		}
	}
}

func TestKeyNeverAppearsInResponse(t *testing.T) {
	s, _ := newServer(t)
	secret := "оченьСекретный1"
	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"psk2","key":"`+secret+`"}`, etag(t, s))
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("пароль вернулся в ответе")
	}
}

func TestKeyNeverAppearsInErrorText(t *testing.T) {
	// Ошибки логируются, логи читают и пересылают.
	s, f := newServer(t)
	secret := "оченьСекретный1"
	f.Errors["commit wireless"] = errors.New("uci: I/O error")

	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"psk2","key":"`+secret+`"}`, etag(t, s))
	if rec.Code == http.StatusOK {
		t.Fatal("ошибка коммита проглочена")
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("пароль просочился в текст ошибки: %s", rec.Body.String())
	}
}

// ─────────── ответ ───────────

func TestWriteReturnsUpdatedListAndNewETag(t *testing.T) {
	s, _ := newServer(t)
	before := etag(t, s)

	rec := post(t, s, "/api/wifi/networks", `{"id":"wifinet2","ssid":"ATOM-2"}`, before)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ответ без ETag — следующая запись не будет знать, от чего отталкиваться")
	}
	var got struct {
		Networks []struct{ ID string } `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("тело не список сетей: %v", err)
	}
	if len(got.Networks) == 0 {
		t.Error("тело ответа должно повторять GET, чтобы панель не делала второй запрос")
	}
}

func TestCommitFailureIsReported(t *testing.T) {
	s, f := newServer(t)
	f.Errors["commit wireless"] = errors.New("нет места")

	rec := post(t, s, "/api/wifi/networks", `{"id":"wifinet2","ssid":"X"}`, etag(t, s))
	if rec.Code == http.StatusOK {
		t.Fatal("сбой коммита выдан за успех")
	}
	if rec.Code < 500 {
		t.Errorf("код %d — сбой записи это не вина клиента", rec.Code)
	}
}

// Полный цикл: создали сеть — она появилась в списке; удалили — исчезла.
//
// Без него тесты доказывали только «нужные команды отправлены», а не
// «результат такой, как ждёт пользователь». Фейк отражает записи в
// чтениях именно ради этой проверки.
func TestCreateThenListThenDelete(t *testing.T) {
	s, _ := newServer(t)

	before := len(listNetworks(t, s))

	// Создаём.
	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"НоваяСеть","encryption":"psk2","key":"пароль12345"}`, etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("создание: %d %s", rec.Code, rec.Body.String())
	}

	nets := listNetworks(t, s)
	if len(nets) != before+1 {
		t.Fatalf("сетей %d, ожидалось %d", len(nets), before+1)
	}

	var created netRow
	for _, n := range nets {
		if n.SSID == "НоваяСеть" {
			created = n
		}
	}
	if created.ID == "" {
		t.Fatalf("созданная сеть не найдена: %+v", nets)
	}
	// Родилась выключенной — иначе немедленно вступила бы в борьбу
	// за radio0 (ADR-0009).
	if created.Enabled {
		t.Error("новая сеть оказалась включённой")
	}
	if !created.Editable {
		t.Error("выключенная сеть должна быть редактируемой")
	}
	if !created.HasKey {
		t.Error("пароль не записан")
	}

	// Удаляем.
	rec = del(t, s, "/api/wifi/networks/"+created.ID, etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("удаление: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(listNetworks(t, s)); got != before {
		t.Errorf("после удаления сетей %d, ожидалось %d", got, before)
	}
}

// Отпечаток обязан меняться после записи: иначе повторный запрос со
// старым значением прошёл бы, и оптимистичная блокировка была бы
// декоративной.
func TestFingerprintChangesAfterWrite(t *testing.T) {
	s, _ := newServer(t)

	before := etag(t, s)
	if rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"X","encryption":"none"}`, before); rec.Code != http.StatusOK {
		t.Fatalf("создание: %d", rec.Code)
	}
	after := etag(t, s)
	if before == after {
		t.Fatal("отпечаток не изменился — блокировка не работает")
	}

	// Со старым отпечатком вторая запись обязана отбиться.
	rec := post(t, s, "/api/wifi/networks", `{"ssid":"Y","encryption":"none"}`, before)
	if rec.Code != http.StatusConflict {
		t.Errorf("запись со старым отпечатком → %d, ожидался 409", rec.Code)
	}
}

type netRow struct {
	ID       string `json:"id"`
	SSID     string `json:"ssid"`
	Enabled  bool   `json:"enabled"`
	Editable bool   `json:"editable"`
	HasKey   bool   `json:"has_key"`
}

func listNetworks(t *testing.T, s *Server) []netRow {
	t.Helper()
	rec := do(t, s, "GET", "/api/wifi/networks", true)
	var got struct {
		Networks []netRow `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор списка: %v", err)
	}
	return got.Networks
}
