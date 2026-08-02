package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"netmoded/internal/nikki"
)

func TestNikkiProxiesShape(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/nikki/proxies", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Available  bool          `json:"available"`
		Group      string        `json:"group"`
		Type       string        `json:"type"`
		Selected   string        `json:"selected"`
		Selectable bool          `json:"selectable"`
		Pinned     bool          `json:"pinned"`
		Members    []nikki.Proxy `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !got.Available || got.Group != "PROXY" || got.Type != "URLTest" {
		t.Errorf("%+v", got)
	}
	// URLTest принимает закрепление: mihomo проверяет интерфейс SelectAble,
	// а не конкретный тип группы.
	if !got.Selectable {
		t.Error("URLTest обязан принимать ручное закрепление")
	}
	if got.Pinned {
		t.Error("исходно ничего не закреплено — pinned должен быть false")
	}
	if got.Selected != "🇨🇭⚡Швейцария 2" {
		t.Errorf("selected = %q", got.Selected)
	}
	if len(got.Members) != 3 {
		t.Errorf("участников %d, ожидалось 3", len(got.Members))
	}
}

// Мёртвый узел отдаётся с delay_ms: null, а не 0: ноль в ответе mihomo
// означает несостоявшуюся пробу, и покрасить его как самый быстрый было
// бы прямо неверно.
func TestNikkiDeadNodeHasNullDelay(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/nikki/proxies", true)

	var got struct {
		Members []struct {
			Name    string `json:"name"`
			DelayMS *int   `json:"delay_ms"`
		} `json:"members"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	for _, m := range got.Members {
		if m.Name == "мёртвый" {
			if m.DelayMS != nil {
				t.Errorf("мёртвый узел отдан с задержкой %d, ожидался null", *m.DelayMS)
			}
			return
		}
	}
	t.Error("мёртвый узел не найден в ответе")
}

// Закрепление узла работает и у URLTest — правка профиля не нужна.
func TestNikkiSelectPinsNode(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	s.SetNikkiClient(fake)

	rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Selected string `json:"selected"`
		Fixed    string `json:"fixed"`
		Pinned   bool   `json:"pinned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Selected != "🇵🇱⚡Польша" || got.Fixed != "🇵🇱⚡Польша" || !got.Pinned {
		t.Errorf("после закрепления: %+v", got)
	}
}

// AUTO из SPEC §7 снимает закрепление. Спека предполагала вложенную группу
// в профиле; движок умеет это сам, поэтому внешний контракт сохранён,
// а профиль не трогается.
func TestNikkiAutoUnpins(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	s.SetNikkiClient(fake)

	if rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("закрепление: %d", rec.Code)
	}
	rec := post(t, s, "/api/nikki/proxy", `{"name":"AUTO"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("AUTO: код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Fixed  string `json:"fixed"`
		Pinned bool   `json:"pinned"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Pinned || got.Fixed != "" {
		t.Errorf("после AUTO закрепление осталось: %+v", got)
	}
}

func TestNikkiSelectUnknownNodeIs404(t *testing.T) {
	s, _ := newServer(t)
	rec := post(t, s, "/api/nikki/proxy", `{"name":"нет-такого-узла"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("код %d, ожидался 404", rec.Code)
	}
}

func TestNikkiSelectRequiresName(t *testing.T) {
	s, _ := newServer(t)
	for _, body := range []string{`{}`, `{"name":""}`, `{`} {
		if rec := post(t, s, "/api/nikki/proxy", body, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("тело %q → %d, ожидался 400", body, rec.Code)
		}
	}
}

func TestNikkiUnavailableDegradesNotFails(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	fake.err = nikki.ErrUnavailable
	s.SetNikkiClient(fake)

	rec := do(t, s, "GET", "/api/nikki/proxies", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d, ожидался 503", rec.Code)
	}
	if got := errCode(t, rec); got != "nikki_unavailable" {
		t.Errorf("код ошибки %q", got)
	}

	// Статус продолжает отвечать.
	st := do(t, s, "GET", "/api/status", true)
	if st.Code != http.StatusOK {
		t.Fatalf("статус упал из-за Nikki: %d", st.Code)
	}
}

func TestStatusCarriesNikkiNode(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/status", true)

	var got struct {
		Nikki struct {
			Available bool   `json:"available"`
			Set       string `json:"set"`
		} `json:"nikki"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !got.Nikki.Available || got.Nikki.Set != "🇨🇭⚡Швейцария 2" {
		t.Errorf("nikki в статусе: %+v", got.Nikki)
	}
}
