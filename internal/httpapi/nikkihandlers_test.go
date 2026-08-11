package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// --- POST /api/nikki/test ---

// nikkiFake достаёт подделку, которую newServer положил в сервер: тесты
// замера смотрят не только на ответ, но и на то, КОГО опрашивали.
func nikkiFake(t *testing.T, s *Server) *fakeNikkiClient {
	t.Helper()
	f, ok := s.nikki.(*fakeNikkiClient)
	if !ok {
		t.Fatalf("клиент nikki подменён на %T", s.nikki)
	}
	return f
}

type testBody struct {
	Members []struct {
		Name    string `json:"name"`
		DelayMS *int   `json:"delay_ms"`
	} `json:"members"`
	Test *struct {
		Total     int `json:"total"`
		Measured  int `json:"measured"`
		Failed    int `json:"failed"`
		Skipped   int `json:"skipped"`
		ElapsedMS int `json:"elapsed_ms"`
	} `json:"test"`
}

// Замер обязан опросить каждого участника группы и доложить итог.
//
// Без поля test ответ неотличим от простого чтения списка: задержки в нём
// есть и без всякого замера — mihomo обновляет history сам (RQ-06). То есть
// нажатие, ничего не измерившее, выглядело бы точно так же, как удачное.
func TestNikkiTestProbesEveryMemberAndReports(t *testing.T) {
	s, _ := newServer(t)
	f := nikkiFake(t, s)

	rec := do(t, s, "POST", "/api/nikki/test", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Test == nil {
		t.Fatal("в ответе нет поля test — исход замера потерян")
	}
	// Трое участников: двое живых и «мёртвый».
	if got.Test.Total != 3 || got.Test.Measured != 2 || got.Test.Failed != 1 || got.Test.Skipped != 0 {
		t.Errorf("итог %+v, ожидалось 3/2/1/0", *got.Test)
	}
	// Ответ несёт и свежий список — панель обновляет числа тем же запросом.
	if len(got.Members) != 3 {
		t.Errorf("участников в ответе %d", len(got.Members))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delayed) != 3 {
		t.Fatalf("проб %d: %v", len(f.delayed), f.delayed)
	}
	for _, n := range f.delayed {
		if n == ProxyGroup || n == "GLOBAL" {
			t.Errorf("замерялась группа %q, а не узел: её задержка — это задержка "+
				"выбранного члена, то есть двойная проба одного узла", n)
		}
	}
}

// Замер — триггер, а не источник чисел, но числа он обязан обновить: иначе
// проверить, что проба вообще состоялась, нечем.
func TestNikkiTestRefreshesDelays(t *testing.T) {
	s, _ := newServer(t)

	rec := do(t, s, "POST", "/api/nikki/test", true)
	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	for _, m := range got.Members {
		switch m.Name {
		case "мёртвый":
			// Провал пробы не сочиняет число: прочерк остаётся прочерком.
			if m.DelayMS != nil {
				t.Errorf("мёртвому узлу приписана задержка %d", *m.DelayMS)
			}
		default:
			if m.DelayMS == nil || *m.DelayMS != 11 {
				t.Errorf("узел %q: задержка не обновилась (%v)", m.Name, m.DelayMS)
			}
		}
	}
}

// Разделители подписки («⬇️ Обходы белых списков ⬇️») в ответе /proxies
// отдельными узлами не значатся. Проба по такому имени провалилась бы, и
// кнопка честно докладывала бы о мёртвом узле, которого не существует.
func TestNikkiTestSkipsSubscriptionSeparators(t *testing.T) {
	s, _ := newServer(t)
	f := nikkiFake(t, s)

	f.mu.Lock()
	g := f.all[ProxyGroup]
	g.Members = append(g.Members, "⬇️ Обходы белых списков ⬇️")
	f.all[ProxyGroup] = g
	f.mu.Unlock()

	rec := do(t, s, "POST", "/api/nikki/test", true)
	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Test == nil || got.Test.Total != 3 || got.Test.Failed != 1 {
		t.Fatalf("итог %+v: разделитель попал в замер", got.Test)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.delayed {
		if n == "⬇️ Обходы белых списков ⬇️" {
			t.Error("разделитель подписки опрошен как узел")
		}
	}
}

// Молчащий Clash API — это 503, а не «замерили ноль узлов»: замерять нечего,
// и притворяться, что операция прошла, нельзя.
func TestNikkiTestOnDeadEngineIs503(t *testing.T) {
	s, _ := newServer(t)
	f := nikkiFake(t, s)
	f.mu.Lock()
	f.err = nikki.ErrUnavailable
	f.mu.Unlock()

	rec := do(t, s, "POST", "/api/nikki/test", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503: %s", rec.Code, rec.Body.String())
	}
}

// Маршрут закрыт токеном, как и остальные: без него — 401, а не замер.
func TestNikkiTestNeedsToken(t *testing.T) {
	s, _ := newServer(t)
	if rec := do(t, s, "POST", "/api/nikki/test", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("код %d без токена", rec.Code)
	}
}

// Поле test не может подменить поля контракта: добавка кладётся так, чтобы
// members или selected остались тем, что отдал движок.
func TestNikkiTestExtraCannotOverwriteContract(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/nikki/test", nil)
	s.writeProxies(rec, req, map[string]any{"members": "подменено", "test": 1})

	var got struct {
		Members []nikki.Proxy `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v — members затёрты строкой", err)
	}
	if len(got.Members) != 3 {
		t.Errorf("участников %d", len(got.Members))
	}
}
