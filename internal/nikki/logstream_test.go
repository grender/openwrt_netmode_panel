package nikki

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// flushWriter — обёртка, чтобы тест не забыл Flush: без него httptest
// буферизует тело, и «поток» проверялся бы на ответе целиком.
func flushWrite(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		return
	}
	w.(http.Flusher).Flush()
}

// TestLogStreamReadsRecordedLines: разбор строк, записанных с роутера.
//
// Главное здесь — стрелка. В теле она приходит как >, и разбирать её по
// сырому байту нельзя; после json.Unmarshal в payload стоит «-->».
func TestLogStreamReadsRecordedLines(t *testing.T) {
	lines := rawSection(t, "виды строк: первые 2 каждого вида")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logsPath {
			t.Errorf("запрошен %s, ожидался %s", r.URL.Path, logsPath)
		}
		if got := r.URL.Query().Get("level"); got != "info" {
			t.Errorf("level = %q, ожидался info", got)
		}
		for _, l := range lines {
			flushWrite(t, w, l+"\n")
		}
	}))
	defer srv.Close()

	s, err := New(srv.URL, "s3cret").LogStream(context.Background(), "")
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	defer s.Close()

	var got []LogLine
	for s.Next() {
		got = append(got, s.Line())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("чтение кончилось ошибкой: %v", err)
	}
	if len(got) != len(lines) {
		t.Fatalf("строк %d, ожидалось %d", len(got), len(lines))
	}
	var info, warning int
	for _, l := range got {
		switch l.Type {
		case "info":
			info++
		case "warning":
			warning++
		default:
			t.Errorf("неизвестный тип строки %q", l.Type)
		}
		if strings.Contains(l.Payload, `\u003e`) {
			t.Errorf("стрелка не раскодирована: %q", l.Payload)
		}
		if !strings.Contains(l.Payload, "-->") {
			t.Errorf("в строке нет стрелки: %q", l.Payload)
		}
	}
	// level=info — это ПОЛ, а не фильтр: warning приходит вместе с info, и
	// именно им приезжает неудавшийся дозвон. Читатель, оставляющий только
	// info, потерял бы весь вердикт «не отвечает».
	if info == 0 || warning == 0 {
		t.Errorf("при level=info пришло info=%d warning=%d; должны быть оба", info, warning)
	}
}

// TestLogStreamClientShape — белый ящик по срокам.
//
// Проверяется именно форма, а не поведение: срок набора номера через stdlib
// снаружи не наблюдаем, а дозвон в чёрную дыру сделал бы тест капризным.
// Зато эти три поля и есть всё решение: любой ненулевой срок здесь рвал бы
// поток на исправном роутере.
func TestLogStreamClientShape(t *testing.T) {
	c := New("http://127.0.0.1:9090", "")
	cl := c.streamClient()
	if cl == c.client {
		t.Fatal("поток ходит тем же клиентом, что и обычные вызовы: его пул на два соединения поток занял бы на часы")
	}
	if cl.Timeout != 0 {
		t.Errorf("Client.Timeout = %v; он накрывает и чтение тела", cl.Timeout)
	}
	tr, ok := cl.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T", cl.Transport)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v; заголовки приходят только с первым событием", tr.ResponseHeaderTimeout)
	}
	if !tr.DisableKeepAlives {
		t.Error("keep-alive включён: поток занял бы соединение в общем пуле")
	}
	if tr.DialContext == nil {
		t.Error("DialContext не задан: единственный срок стоит именно там")
	}
}

// TestLogStreamOutlivesCallTimeout: у тела сроков нет вовсе.
//
// Тест медленный намеренно, и это его смысл. Заголовки mihomo отдаёт только
// с ПЕРВЫМ событием (raw/91: curl -si -m 2 не получил ничего), а у тихого
// устройства события может не быть минутами. Если поток однажды поедет через
// do — то есть через context.WithTimeout, — он умрёт здесь, а на роутере это
// выглядело бы как «наблюдатель иногда пустой».
func TestLogStreamOutlivesCallTimeout(t *testing.T) {
	// Первое событие позже probeTimeout, второе — так, чтобы суммарно
	// перевалить за callTimeout.
	first := probeTimeout + 400*time.Millisecond
	second := callTimeout - probeTimeout + 400*time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(first)
		flushWrite(t, w, `{"type":"info","payload":"первое"}`+"\n")
		time.Sleep(second)
		flushWrite(t, w, `{"type":"info","payload":"второе"}`+"\n")
	}))
	defer srv.Close()

	start := time.Now()
	s, err := New(srv.URL, "").LogStream(context.Background(), "info")
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	defer s.Close()

	for i, want := range []string{"первое", "второе"} {
		if !s.Next() {
			t.Fatalf("строка %d не пришла (прошло %v): %v", i+1, time.Since(start), s.Err())
		}
		if s.Line().Payload != want {
			t.Fatalf("строка %d = %q, ожидалось %q", i+1, s.Line().Payload, want)
		}
	}
	if d := time.Since(start); d <= callTimeout {
		t.Fatalf("тест прошёл за %v — короче callTimeout, значит ничего не доказал", d)
	}
}

// TestLogStreamCancelStopsIteration: останавливает поток только отмена.
func TestLogStreamCancelStopsIteration(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushWrite(t, w, `{"type":"info","payload":"первое"}`+"\n")
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(srv.URL, "").LogStream(ctx, "info")
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	if !s.Next() {
		t.Fatalf("первая строка не пришла: %v", s.Err())
	}
	cancel()
	if s.Next() {
		t.Errorf("после отмены пришла строка %q", s.Line().Payload)
	}
	s.Close()
}

// TestLogStreamSkipsOversizeLine: длинная строка ПРОПУСКАЕТСЯ, а не кончает
// поток.
//
// Это не придирка к краю. bufio.Scanner на переполнении буфера отдаёт
// терминальную ErrTooLong — одна такая строка закрыла бы поток, а если она
// повторяется, наблюдатель ушёл бы в вечный цикл переподключений, который
// владелец читает как «движок перезапускается».
func TestLogStreamSkipsOversizeLine(t *testing.T) {
	huge, _ := json.Marshal(LogLine{Type: "info", Payload: strings.Repeat("ы", lineLimit)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushWrite(t, w, `{"type":"info","payload":"до"}`+"\n")
		flushWrite(t, w, string(huge)+"\n")
		flushWrite(t, w, `{"type":"info","payload":"после"}`+"\n")
	}))
	defer srv.Close()

	s, err := New(srv.URL, "").LogStream(context.Background(), "info")
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	defer s.Close()

	var got []string
	for s.Next() {
		got = append(got, s.Line().Payload)
	}
	if len(got) != 2 || got[0] != "до" || got[1] != "после" {
		t.Fatalf("прочитано %q, ожидались обе короткие строки", got)
	}
	if s.Oversize() != 1 {
		t.Errorf("Oversize = %d, ожидалась 1", s.Oversize())
	}
	if err := s.Err(); err != nil {
		t.Errorf("поток кончился ошибкой %v, а длинная строка не ошибка", err)
	}
}

// TestLogStreamKeepsNonJSONLine: не-JSON отдаётся наверх с пустым типом.
//
// Это смена формата у движка, а не порча потока, и считать её должен тот же
// счётчик, что и строки, не поддавшиеся грамматике, — иначе у владельца два
// разных молчания об одной беде.
func TestLogStreamKeepsNonJSONLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushWrite(t, w, "не json вовсе\n")
	}))
	defer srv.Close()

	s, err := New(srv.URL, "").LogStream(context.Background(), "info")
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	defer s.Close()
	if !s.Next() {
		t.Fatalf("строка не пришла: %v", s.Err())
	}
	if s.Line().Type != "" || s.Line().Payload != "не json вовсе" {
		t.Errorf("строка = %+v", s.Line())
	}
}

// TestLogStreamRejectsUnknownLevel: опечатка вызывающего не уезжает
// параметром в чужую службу.
func TestLogStreamRejectsUnknownLevel(t *testing.T) {
	if _, err := New("http://127.0.0.1:1", "").LogStream(context.Background(), "verbose"); err == nil {
		t.Fatal("неизвестный уровень принят")
	}
}

// TestLogStreamUnauthorizedIsUnavailable: 401 — та же недоступность.
func TestLogStreamUnauthorizedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "wrong").LogStream(context.Background(), "info")
	if err == nil || !strings.Contains(err.Error(), "недоступен") {
		t.Errorf("401 дал %v, ожидалась ErrUnavailable", err)
	}
}
