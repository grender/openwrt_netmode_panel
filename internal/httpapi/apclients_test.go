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

// TestAPClientsCountsAssocList: пять MAC — пять клиентов.
//
// Тело фикстуры СИНТЕТИЧЕСКОЕ: в разведке команда прошла через grep mac
// (raw/91:83), и обёртка {"results": […]} выведена по отступам и по
// соседнему raw/23, а не наблюдалась. MAC внутри — настоящие из той выдачи.
func TestAPClientsCountsAssocList(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()
	f.Fixtures["ubus iwinfo assoclist"] = []byte(`{"results":[
		{"mac":"02:00:00:00:00:03"},{"mac":"02:00:00:00:00:04"},{"mac":"02:00:00:00:00:02"},
		{"mac":"02:00:00:00:00:06"},{"mac":"02:00:00:00:00:05"}]}`)

	got, code := apClients(t, s)
	if code != http.StatusOK {
		t.Fatalf("код = %d", code)
	}
	if got == nil || *got != 5 {
		t.Fatalf("клиентов = %v, ожидалось 5", got)
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
// Обёртка assoclist не снята целиком, и это единственная защита от того,
// что догадка окажется неверной: панель покажет прочерк, а не выдуманное
// число. Уберут разборчивость — тест покраснеет.
func TestAPClientsNullOnUnknownShape(t *testing.T) {
	s, f := newServer(t)
	defer s.Close()
	f.Fixtures["ubus iwinfo assoclist"] = []byte(`{"clients":{"02:00:00:00:00:03":{}}}`)

	if got, _ := apClients(t, s); got != nil {
		t.Fatalf("клиентов = %v, ожидался null: форма ответа другая", *got)
	}
}
