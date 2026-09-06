package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// secretURL — адрес с секретом в двух разных местах: в пути и в параметре.
// Оба обязаны исчезнуть из всего, что демон отдаёт наружу.
const secretURL = "https://sub.example.net/api/v1/client/subscribe?token=S3CRET-TOKEN&flow=xtls"

func put(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func subBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("тело не разбирается: %s", rec.Body.String())
	}
	return out
}

// ─────────── маска ───────────

// Главная проверка файла. ADR-0012 запрещает возвращать учётные данные, а
// status.go обещает прямым текстом: «сам адрес наружу не уходит НИКОГДА».
// Маска обязана отвечать на «какой провайдер» и «задан ли адрес», не отдав
// ни одного символа секрета — «хвостик из четырёх знаков» это утечка
// четырёх знаков, а не мера защиты.
func TestMaskKeepsNoSecretCharacters(t *testing.T) {
	got := maskSubURL(secretURL)

	for _, secret := range []string{"S3CRET", "TOKEN", "xtls"} {
		if strings.Contains(got, secret) {
			t.Errorf("маска %q содержит секрет %q", got, secret)
		}
	}
	// Провайдер обязан быть узнаваем — иначе блок в панели бесполезен.
	if !strings.Contains(got, "sub.example.net") {
		t.Errorf("маска %q не называет провайдера", got)
	}
	// Имена параметров секретом не являются и оставлены намеренно: по ним
	// владелец узнаёт свой адрес.
	if !strings.Contains(got, "token=…") || !strings.Contains(got, "flow=…") {
		t.Errorf("маска %q потеряла имена параметров", got)
	}
}

// Порядок параметров в карте случаен, а панель опрашивает демона раз в
// секунду: мигающая строка читалась бы как «адрес меняется сам».
func TestMaskIsStableAcrossCalls(t *testing.T) {
	first := maskSubURL(secretURL)
	for i := 0; i < 50; i++ {
		if got := maskSubURL(secretURL); got != first {
			t.Fatalf("маска непостоянна: %q против %q", got, first)
		}
	}
}

// Неразбираемый адрес — единственный случай, когда неизвестно, где лежит
// секрет. Отдать «как есть» здесь значит отдать его целиком.
func TestMaskHidesUnparsableURLEntirely(t *testing.T) {
	got := maskSubURL("://S3CRET-TOKEN")
	if strings.Contains(got, "S3CRET") {
		t.Errorf("неразбираемый адрес показан целиком: %q", got)
	}
	if got == "" {
		t.Error("неразбираемый адрес выдан за незаданный — это разные состояния")
	}
}

func TestMaskEmptyStaysEmpty(t *testing.T) {
	if got := maskSubURL(""); got != "" {
		t.Errorf("пустой адрес → %q, ожидалась пустота", got)
	}
}

// ─────────── чтение ───────────

func TestSubscriptionGetNeverLeaksURL(t *testing.T) {
	s, _ := newServer(t)
	s.subURL.set(secretURL)

	rec := do(t, s, "GET", "/api/subscription", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "S3CRET") {
		t.Fatalf("секрет уехал в ответ: %s", rec.Body.String())
	}
	got := subBody(t, rec)
	if got["configured"] != true {
		t.Errorf("configured = %#v, ожидался true", got["configured"])
	}
}

func TestSubscriptionGetUnsetIsNotConfigured(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/subscription", true)

	got := subBody(t, rec)
	if got["configured"] != false {
		t.Errorf("configured = %#v, ожидался false", got["configured"])
	}
	if got["masked"] != "" {
		t.Errorf("masked = %#v, ожидалась пустота", got["masked"])
	}
}

// ─────────── запись ───────────

func TestSubscriptionPutWritesAndCommits(t *testing.T) {
	s, f := newServer(t)

	rec := put(t, s, "/api/subscription", `{"url":"`+secretURL+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.subURL.get(); got != secretURL {
		t.Errorf("живое значение %q", got)
	}
	if !hasCall(f.Calls, "set netmode.main.subscription_url="+secretURL) {
		t.Errorf("адрес не записан в UCI: %v", f.Calls)
	}
	// Без коммита адрес остался бы черновиком: он виден в LuCI и уехал бы
	// в систему при первом чужом коммите пакета.
	if !hasCall(f.Calls, "commit netmode") {
		t.Errorf("netmode не закоммичен — адрес остался черновиком: %v", f.Calls)
	}
}

// Пустая строка стирает настройку. Отдельного DELETE нет намеренно: удаление
// настройки — это её значение, а не другая операция.
func TestSubscriptionPutEmptyClears(t *testing.T) {
	s, _ := newServer(t)
	s.subURL.set(secretURL)

	rec := put(t, s, "/api/subscription", `{"url":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.subURL.get(); got != "" {
		t.Errorf("адрес не стёрся: %q", got)
	}
	if subBody(t, rec)["configured"] != false {
		t.Error("после стирания configured остался true")
	}
}

// Адрес приезжает из буфера обмена, и хвостовой перевод строки — свойство
// копирования, а не ошибка владельца.
func TestSubscriptionPutTrimsSpace(t *testing.T) {
	s, _ := newServer(t)

	rec := put(t, s, "/api/subscription", `{"url":"  https://sub.example.net/x  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.subURL.get(); got != "https://sub.example.net/x" {
		t.Errorf("пробелы не срезаны: %q", got)
	}
}

func TestSubscriptionPutRejectsBadURL(t *testing.T) {
	for _, body := range []string{
		`{"url":"://S3CRET"}`,
		`{"url":"ftp://sub.example.net/x"}`,
		`{"url":"just-a-string"}`,
		`{`,
	} {
		s, f := newServer(t)
		rec := put(t, s, "/api/subscription", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("тело %q → %d, ожидался 400", body, rec.Code)
			continue
		}
		// Отказ не должен ничего записать: половина сделанного здесь хуже,
		// чем ничего, — в конфиге остался бы адрес, который панель считает
		// отвергнутым.
		for _, c := range f.Calls {
			if strings.HasPrefix(c, "set netmode.main.subscription_url") {
				t.Errorf("тело %q отвергнуто, но в UCI записано: %s", body, c)
			}
		}
		// И не должен вернуть присланное значение: в нём секрет.
		if strings.Contains(rec.Body.String(), "S3CRET") {
			t.Errorf("тело ошибки содержит присланный секрет: %s", rec.Body.String())
		}
	}
}

// Регрессия на ADR-0034: адрес читается в момент вызова, а не при сборке
// графа. Если бы гейт обновления смотрел в cfg, владелец задал бы адрес и
// продолжал получать subscription_not_configured до перезапуска демона.
func TestSubscriptionUpdateSeesFreshURL(t *testing.T) {
	s, _ := newServer(t)

	rec := post(t, s, "/api/subscription/update", ``, "")
	if got := errCode(t, rec); got != "subscription_not_configured" {
		t.Fatalf("без адреса код %q, ожидался subscription_not_configured", got)
	}

	if rec := put(t, s, "/api/subscription", `{"url":"`+secretURL+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT: код %d: %s", rec.Code, rec.Body.String())
	}

	rec = post(t, s, "/api/subscription/update", ``, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("после записи адреса код %d: %s", rec.Code, rec.Body.String())
	}
	// Джоб идёт в своей горутине и пишет во временный каталог теста;
	// без ожидания t.TempDir() сносится у него под руками.
	s.jobs.Wait(2 * time.Second)
}

// Тот же признак обязан дойти и до статуса — иначе панель показывает
// «адрес не задан» рядом с кнопкой, которая уже работает.
func TestStatusConfiguredFollowsWrite(t *testing.T) {
	s, _ := newServer(t)
	// Часы читателя статуса — свои: у него кэш на StatusCacheTTL, и без
	// перевода часов второй запрос вернул бы тот же снимок, что первый,
	// то есть тест доказывал бы работу кэша, а не признака.
	base := time.Now()
	cur := base
	s.status.now = func() time.Time { return cur }

	if got := statusSub(t, s)["configured"]; got != false {
		t.Fatalf("до записи configured = %#v", got)
	}
	if rec := put(t, s, "/api/subscription", `{"url":"`+secretURL+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d", rec.Code)
	}
	cur = base.Add(StatusCacheTTL + time.Millisecond)
	if got := statusSub(t, s)["configured"]; got != true {
		t.Errorf("после записи configured = %#v, ожидался true", got)
	}
}

// Статус опрашивается раз в секунду — адресу там не место ни в каком виде,
// даже маской (ADR-0023: секрет уходит отдельным запросом, не в статусе).
func TestStatusNeverCarriesSubscriptionURL(t *testing.T) {
	s, _ := newServer(t)
	s.subURL.set(secretURL)

	rec := do(t, s, "GET", "/api/status", true)
	body := rec.Body.String()
	for _, needle := range []string{"S3CRET", "sub.example.net"} {
		if strings.Contains(body, needle) {
			t.Errorf("статус содержит %q", needle)
		}
	}
}

func statusSub(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, "GET", "/api/status", true)
	var out struct {
		Subscription map[string]any `json:"subscription"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("статус не разбирается: %v", err)
	}
	return out.Subscription
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
