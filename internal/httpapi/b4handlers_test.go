package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"netmoded/internal/b4"
)

func TestB4SetsEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/b4/sets", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Available    bool     `json:"available"`
		Version      string   `json:"version"`
		Selected     string   `json:"selected"`
		EnabledCount int      `json:"enabled_count"`
		Sets         []b4.Set `json:"sets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !got.Available || got.Version != "1.74.1" {
		t.Errorf("available=%v version=%q", got.Available, got.Version)
	}
	// Имена с настоящего роутера, а не general/discord из макета.
	if len(got.Sets) != 2 || got.Sets[0].Name != "workki" {
		t.Errorf("сеты: %+v", got.Sets)
	}
	if got.Selected != "HomeSet" || got.EnabledCount != 1 {
		t.Errorf("selected=%q count=%d", got.Selected, got.EnabledCount)
	}
	// Стратегии наружу не проксируются: ~2.5 КБ на сет при опросе раз
	// в секунду — это мегабайты в час ради трёх полей.
	if len(rec.Body.Bytes()) > 2000 {
		t.Errorf("ответ %d байт — похоже, стратегии не отброшены", len(rec.Body.Bytes()))
	}
}

// Ключи ответа /api/b4/sets присутствуют всегда, даже когда сообщать нечего.
//
// Разбор в типизированную структуру этого не ловит: пропущенный ключ и
// нулевое значение дают одинаковый результат. Поэтому проверка идёт по карте:
// `selected: ""` при нуле включённых — это ответ, а не молчание.
func TestB4SetsResponseKeysAlwaysPresent(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeB4Client()
	for i := range fake.sets {
		fake.sets[i].Enabled = false
	}
	s.SetB4Client(fake)

	rec := do(t, s, "GET", "/api/b4/sets", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	for _, k := range []string{"available", "version", "selected", "enabled_count", "sets"} {
		if _, ok := got[k]; !ok {
			t.Errorf("нет ключа %q: %v", k, got)
		}
	}
	if got["selected"] != "" {
		t.Errorf("selected = %#v, ожидалась пустая строка при нуле включённых", got["selected"])
	}
	if got["enabled_count"] != float64(0) {
		t.Errorf("enabled_count = %#v, ожидался 0", got["enabled_count"])
	}
}

// Эксклюзивность — наша семантика, не b4: там у каждого сета свой флаг,
// и включённых может быть несколько.
func TestB4SetSwitchIsExclusive(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeB4Client()
	s.SetB4Client(fake)

	rec := post(t, s, "/api/b4/set", `{"id":"909a6fb1-b9b1-4af1-8ee0-bdce82e3d8ff"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	on := 0
	for _, x := range fake.sets {
		if x.Enabled {
			on++
			if x.Name != "workki" {
				t.Errorf("включён не тот сет: %s", x.Name)
			}
		}
	}
	if on != 1 {
		t.Errorf("включено %d сетов, панель навязывает ровно один", on)
	}
}

func TestB4SetRequiresID(t *testing.T) {
	s, _ := newServer(t)
	// Имя ключом не является: в b4 оно не уникально (sets.go:315).
	for _, body := range []string{`{}`, `{"id":""}`, `{"name":"workki"}`, `{`} {
		rec := post(t, s, "/api/b4/set", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("тело %q → %d, ожидался 400", body, rec.Code)
		}
	}
}

func TestB4SetUnknownIDIs404(t *testing.T) {
	s, _ := newServer(t)
	rec := post(t, s, "/api/b4/set", `{"id":"нет-такого"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("код %d, ожидался 404", rec.Code)
	}
}

// Частичное переключение — не «сервис недоступен».
//
// SelectOnly гасит прочие сеты, затем включает целевой. Если второй шаг
// упал, обход DPI выключен целиком, но b4 при этом жив и отвечает.
// Отдать 503 значило бы соврать про причину и предложить владельцу ждать
// вместо того единственного действия, которое помогает, — повторить.
func TestB4PartialIsConflictNotUnavailable(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeB4Client()
	fake.err = b4.ErrPartial
	s.SetB4Client(fake)

	rec := post(t, s, "/api/b4/set", `{"id":"909a6fb1-b9b1-4af1-8ee0-bdce82e3d8ff"}`, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("код %d, ожидался 409", rec.Code)
	}
	if got := errCode(t, rec); got != "b4_partial" {
		t.Errorf("код ошибки %q, ожидался b4_partial", got)
	}
}

// b4 перезапускается сам (raw/25-netstat.txt: в снимке разведки он лежал).
// Это штатная ситуация: сеты гаснут, остальная панель работает.
func TestB4UnavailableDegradesNotFails(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeB4Client()
	fake.err = b4.ErrUnavailable
	s.SetB4Client(fake)

	rec := do(t, s, "GET", "/api/b4/sets", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d, ожидался 503", rec.Code)
	}
	if got := errCode(t, rec); got != "b4_unavailable" {
		t.Errorf("код ошибки %q", got)
	}

	// Статус при этом обязан продолжать отвечать.
	st := do(t, s, "GET", "/api/status", true)
	if st.Code != http.StatusOK {
		t.Fatalf("статус упал из-за b4: %d", st.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(st.Body.Bytes(), &got)
	b4field, _ := got["b4"].(map[string]any)
	if b4field["available"] != false {
		t.Errorf("b4.available = %v, ожидалось false", b4field["available"])
	}
}

func TestStatusCarriesB4Set(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/status", true)

	var got struct {
		B4 struct {
			Available    bool   `json:"available"`
			Set          string `json:"set"`
			EnabledCount int    `json:"enabled_count"`
		} `json:"b4"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !got.B4.Available || got.B4.Set != "HomeSet" || got.B4.EnabledCount != 1 {
		t.Errorf("b4 в статусе: %+v", got.B4)
	}
}

// Когда сетов включено несколько (кто-то менял через веб-морду b4),
// «текущего» нет — и выдавать первый попавшийся значило бы врать.
func TestStatusB4SetEmptyWhenSeveralEnabled(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeB4Client()
	for i := range fake.sets {
		fake.sets[i].Enabled = true
	}
	s.SetB4Client(fake)

	rec := do(t, s, "GET", "/api/status", true)
	var got struct {
		B4 struct {
			Set          string `json:"set"`
			EnabledCount int    `json:"enabled_count"`
		} `json:"b4"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.B4.Set != "" {
		t.Errorf("set = %q, ожидалась пустота при двух включённых", got.B4.Set)
	}
	if got.B4.EnabledCount != 2 {
		t.Errorf("enabled_count = %d, ожидалось 2", got.B4.EnabledCount)
	}
}

func TestB4ErrorOtherThanUnavailableIs502(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeB4Client()
	fake.err = errors.New("что-то пошло не так")
	s.SetB4Client(fake)

	rec := do(t, s, "GET", "/api/b4/sets", true)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("код %d, ожидался 502", rec.Code)
	}
}
