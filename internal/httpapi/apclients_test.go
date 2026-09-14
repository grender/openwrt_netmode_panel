package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// Число клиентов домашней точки (RQ-05).
//
// До этого коммита поле было null всегда и означало «мы этого не делали».
// Теперь null означает единственное, что оно и должно означать: спросить не
// смогли. Разница не косметическая — «0 устройств» на исправной точке с
// пятью телефонами выглядит как поломка сети, а не как молчание ubus.

func apClients(t *testing.T, s *Server) (*int, int) {
	t.Helper()
	rec := watchReq(t, s, "GET", "/api/status", "")
	var st struct {
		AP struct {
			Clients *int `json:"clients"`
		} `json:"ap"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("статус не разобран: %v", err)
	}
	return st.AP.Clients, rec.Code
}

// TestAPClientsCountsAssocList: четыре клиента в записанном выводе — четыре
// в статусе.
//
// Тело — raw/92, снятое с роутера 2026-09-14 целиком, а не выведенное по
// отступам: живой демон на том же выводе ответил clients: 4.
func TestAPClientsCountsAssocList(t *testing.T) {
	s, _ := newServer(t)
	defer s.Close()

	got, code := apClients(t, s)
	if code != http.StatusOK {
		t.Fatalf("код = %d", code)
	}
	if got == nil || *got != 4 {
		t.Fatalf("клиентов = %v, в raw/92 их 4", got)
	}
}

// TestAPClientsZeroIsAnAnswer: точка ответила, клиентов нет — это НОЛЬ.
func TestAPClientsZeroIsAnAnswer(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()
	f.Fixtures["ubus iwinfo assoclist"] = []byte(`{"results":[]}`)

	got, _ := apClients(t, s)
	if got == nil || *got != 0 {
		t.Fatalf("клиентов = %v, ожидался ноль: точка ответила", got)
	}
}

// TestAPClientsNullWhenNotAsked: ubus не ответил — null, а не ноль.
func TestAPClientsNullWhenNotAsked(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()
	f.Errors["ubus iwinfo assoclist"] = errors.New("объект не отвечает")

	if got, _ := apClients(t, s); got != nil {
		t.Fatalf("клиентов = %v, ожидался null: спросить не смогли", *got)
	}
}

// TestAPClientsNullOnUnknownShape: форма ответа не та — тоже null.
//
// Форма снята (raw/92), но она чужая и без контракта: следующая сборка
// iwinfo вправе её поменять. Тогда панель обязана показать прочерк, а не
// выдуманное число. Уберут разборчивость — тест покраснеет.
func TestAPClientsNullOnUnknownShape(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()
	f.Fixtures["ubus iwinfo assoclist"] = []byte(`{"clients":{"02:00:00:00:00:03":{}}}`)

	if got, _ := apClients(t, s); got != nil {
		t.Fatalf("клиентов = %v, ожидался null: форма ответа другая", *got)
	}
}
