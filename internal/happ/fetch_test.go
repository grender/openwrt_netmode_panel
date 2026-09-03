package happ

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchRefusesInsecureScheme: в адресе подписки лежит идентификатор
// доступа ко всем серверам владельца. Отдать его по http — показать каждому
// на пути.
func TestFetchRefusesInsecureScheme(t *testing.T) {
	cases := []string{
		"http://example.net/sub/deadbeefcafe1234",
		"ftp://example.net/sub/deadbeefcafe1234",
		"example.net/sub/deadbeefcafe1234", // без схемы вовсе
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			_, err := Fetch(context.Background(), addr)
			if !errors.Is(err, ErrNotHTTPS) {
				t.Fatalf("ошибка %v, ожидалась ErrNotHTTPS", err)
			}
			assertNoSecret(t, err)
		})
	}
}

// TestFetchRefusesOversizedBody: потолок тела — отказ, а не усечение.
//
// Логика ADR-0022: тело разбирается, и молча обрезанный JSON даёт
// загадочную ошибку парсера — владелец идёт искать поломку в нашем разборе
// вместо болтливого сервера.
func TestFetchRefusesOversizedBody(t *testing.T) {
	chunk := strings.Repeat("A", 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for written := 0; written <= maxBody; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	_, err := fetch(context.Background(), srv.Client(), srv.URL+"/sub/deadbeefcafe1234")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ошибка %v, ожидалась ErrTooLarge", err)
	}
	assertNoSecret(t, err)
}

func TestFetchReportsStatus(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))

		_, err := fetch(context.Background(), srv.Client(), srv.URL+"/sub/deadbeefcafe1234")
		srv.Close()

		// Отдельный класс ошибки, а не текст: 401 означает протухший
		// доступ и требует владельца, 5xx пройдёт сам к следующему
		// запуску по расписанию. Различать их поиском подстроки значило
		// бы поставить поведение планировщика в зависимость от
		// формулировки сообщения.
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("ошибка %v (%T), ожидался *StatusError", err, err)
		}
		if se.Code != code {
			t.Errorf("код %d, ожидался %d", se.Code, code)
		}
		assertNoSecret(t, err)
	}
}

// TestFetchSendsHappUserAgent: UA — часть контракта, а не вежливость.
// На "Happ" провайдер отдаёт массив полных конфигов Xray, на clash-подобный
// UA — собранный за нас профиль на 15 узлов без xhttp, без записей обхода и
// без разделителя. Сменить строку значит получить другой товар.
func TestFetchSendsHappUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	body, err := fetch(context.Background(), srv.Client(), srv.URL+"/sub/deadbeefcafe1234")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != "Happ" {
		t.Errorf("User-Agent %q, ожидался Happ", got)
	}
	if string(body) != "[]" {
		t.Errorf("тело %q", body)
	}
}

// TestFetchHidesAddressOnTransportError — главная ловушка файла: Do
// возвращает *url.Error, и поле URL в нём несёт полный адрес с секретом.
// Обёртка через %w опубликовала бы его в журнале самой безобидной на вид
// строкой.
func TestFetchHidesAddressOnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL + "/sub/deadbeefcafe1234"
	srv.Close() // сервер закрыт — соединение не установится

	_, err := fetch(context.Background(), http.DefaultClient, addr)
	if err == nil {
		t.Fatal("ожидалась ошибка соединения")
	}
	assertNoSecret(t, err)
	if strings.Contains(err.Error(), addr) {
		t.Errorf("адрес подписки утёк в текст ошибки: %v", err)
	}
}

// assertNoSecret проверяет, что идентификатор подписки не попал в текст
// ошибки. В журнале ему места нет ни при одном исходе.
func assertNoSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range []string{"deadbeefcafe1234", "/sub/"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("в тексте ошибки %q найден %q", err.Error(), secret)
		}
	}
}
