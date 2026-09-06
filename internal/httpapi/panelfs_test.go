package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getPanel — запрос к панели с токеном и произвольными заголовками.
func getPanel(t *testing.T, s *Server, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Точка входа обязана перечитываться ВСЕГДА: в ней лежат адреса ассетов
// с новой версией. Закэшируй её браузер — и обновлённая панель не приедет
// никогда, потому что о новых адресах он не узнает.
func TestPanelIndexIsNeverCached(t *testing.T) {
	s, _ := newServer(t)
	rec := getPanel(t, s, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/ → %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control у / = %q, ожидался no-store", got)
	}
}

// Адрес ассета несёт версию содержимого (ADR-0036), поэтому по нему
// содержимое измениться не может — и кэшировать его можно навсегда.
// Без этих заголовков версия в адресе не покупает ничего.
func TestPanelAssetsAreImmutable(t *testing.T) {
	s, _ := newServer(t)
	for _, p := range panelSubresources(getPanel(t, s, "/", nil).Body.String()) {
		rec := getPanel(t, s, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s → %d", p, rec.Code)
		}
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("Cache-Control у %s = %q, ожидались max-age=31536000 и immutable", p, cc)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("%s без ETag: условный запрос невозможен", p)
		}
	}
}

// Сжатие обещано документацией с самого начала, а в коде его не было:
// статика ехала голым http.FileServer. Тест — единственное, что не даёт
// обещанию снова разойтись с кодом.
func TestPanelServesGzip(t *testing.T) {
	s, _ := newServer(t)
	subs := panelSubresources(getPanel(t, s, "/", nil).Body.String())
	if len(subs) == 0 {
		t.Fatal("в HTML не нашлось подресурсов")
	}

	js := ""
	for _, p := range subs {
		if strings.HasSuffix(p, ".js") {
			js = p
		}
	}
	if js == "" {
		t.Fatal("среди подресурсов нет js")
	}

	plain := getPanel(t, s, js, nil)
	zipped := getPanel(t, s, js, map[string]string{"Accept-Encoding": "gzip, deflate, br"})

	if zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, ожидался gzip", zipped.Header().Get("Content-Encoding"))
	}
	// Vary обязателен: без него кэш-посредник отдал бы сжатое тело клиенту,
	// который сжатия не просил.
	if !strings.Contains(zipped.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("нет Vary: Accept-Encoding — посредник отдаст сжатое тело тому, кто не просил")
	}
	if zipped.Body.Len() >= plain.Body.Len() {
		t.Errorf("сжатое тело %d байт не меньше несжатого %d", zipped.Body.Len(), plain.Body.Len())
	}

	// И оно обязано РАСПАКОВЫВАТЬСЯ в ровно то же самое.
	zr, err := gzip.NewReader(strings.NewReader(zipped.Body.String()))
	if err != nil {
		t.Fatalf("тело не разжимается: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("чтение разжатого: %v", err)
	}
	if string(out) != plain.Body.String() {
		t.Error("разжатое тело не совпадает с несжатым")
	}
}

// Клиенту, который не просил сжатия, сжатое тело не отдаётся.
func TestPanelPlainWhenNotAccepted(t *testing.T) {
	s, _ := newServer(t)
	rec := getPanel(t, s, "/", map[string]string{"Accept-Encoding": "identity"})
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, а сжатия не просили", got)
	}
}

// Условный запрос: второй заход по тому же ETag не тащит тело заново.
func TestPanelConditionalGet(t *testing.T) {
	s, _ := newServer(t)
	first := getPanel(t, s, "/", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("нет ETag")
	}
	second := getPanel(t, s, "/", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Errorf("повторный запрос по тому же ETag → %d, ожидался 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 с телом в %d байт", second.Body.Len())
	}
}

// Панель — не SPA с маршрутизацией: неизвестный путь это неизвестный путь.
// Подмена его на index превратила бы опечатку в адресе ассета в белый экран
// без единой ошибки в консоли.
func TestPanelUnknownFileIs404(t *testing.T) {
	s, _ := newServer(t)
	if rec := getPanel(t, s, "/assets/нет-такого.js", nil); rec.Code != http.StatusNotFound {
		t.Errorf("неизвестный файл панели → %d, ожидался 404", rec.Code)
	}
}
