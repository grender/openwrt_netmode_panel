package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"netmoded/internal/nikki"
)

// engineWithExits — движок с раскладкой ADR-0050: EXIT-DC = select
// [EXIT-DC-HOP, REJECT, прямой узел], EXIT-DC-HOP = url-test над цепочками.
func engineWithExits(t *testing.T, s *Server) *fakeNikkiClient {
	t.Helper()
	f := nikkiFake(t, s)
	d := func(v int) *int { return &v }
	f.mu.Lock()
	defer f.mu.Unlock()
	chain := "kz-nqz-exit-01-dc ⇢ 🇷🇺💳Россия"
	f.all["EXIT-DC"] = nikki.Proxy{Name: "EXIT-DC", Type: "Selector", Alive: true, Selectable: true,
		Members: []string{"EXIT-DC-HOP", "REJECT", "kz-nqz-exit-01-dc"}, Now: "EXIT-DC-HOP"}
	f.all["EXIT-DC-HOP"] = nikki.Proxy{Name: "EXIT-DC-HOP", Type: "URLTest", Alive: true, Selectable: true,
		Members: []string{chain, "kz-nqz-exit-01-dc ⇢ 🇩🇪⚡Германия"}, Now: chain, DelayMS: d(929)}
	f.all[chain] = nikki.Proxy{Name: chain, Type: "Vless", Alive: true, DelayMS: d(929), Provider: "exits"}
	f.all["kz-nqz-exit-01-dc ⇢ 🇩🇪⚡Германия"] = nikki.Proxy{Name: "kz-nqz-exit-01-dc ⇢ 🇩🇪⚡Германия", Type: "Vless", Provider: "exits"}
	f.all["kz-nqz-exit-01-dc"] = nikki.Proxy{Name: "kz-nqz-exit-01-dc", Type: "Vless", Alive: false, Provider: "exits"}
	f.all["REJECT"] = nikki.Proxy{Name: "REJECT", Type: "Reject", Alive: true}
	return f
}

type exitsBody struct {
	Groups []struct {
		Name    string `json:"name"`
		Now     string `json:"now"`
		Members []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Now     string `json:"now"`
			DelayMS *int   `json:"delay_ms"`
			Alive   bool   `json:"alive"`
		} `json:"members"`
	} `json:"groups"`
}

func getExits(t *testing.T, s *Server) exitsBody {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/nikki/exits", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var b exitsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Только группы EXIT-*, участники — с видом: панель рисует «прямо / через
// хоп / закрыть», а не голые имена.
func TestNikkiExitsList(t *testing.T) {
	s, _ := newServer(t)
	engineWithExits(t, s)
	b := getExits(t, s)
	var names []string
	for _, g := range b.Groups {
		names = append(names, g.Name)
	}
	// EXIT-DC-HOP — тоже EXIT-*, но это участник, а не выход: отдельной
	// строкой он не идёт.
	if strings.Join(names, ",") != "EXIT-DC" {
		t.Fatalf("группы %v", names)
	}
	g := b.Groups[0]
	if g.Now != "EXIT-DC-HOP" || len(g.Members) != 3 {
		t.Fatalf("группа %+v", g)
	}
	kinds := map[string]string{}
	for _, m := range g.Members {
		kinds[m.Name] = m.Kind
	}
	if kinds["EXIT-DC-HOP"] != "chains" || kinds["REJECT"] != "reject" || kinds["kz-nqz-exit-01-dc"] != "direct" {
		t.Errorf("виды %v", kinds)
	}
	hop := g.Members[0]
	if hop.Now != "kz-nqz-exit-01-dc ⇢ 🇷🇺💳Россия" || hop.DelayMS == nil || *hop.DelayMS != 929 {
		t.Errorf("группа цепочек: now=%q delay=%v", hop.Now, hop.DelayMS)
	}
}

func postExitSelect(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/nikki/exits/select", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestNikkiExitSelect(t *testing.T) {
	s, _ := newServer(t)
	f := engineWithExits(t, s)
	rec := postExitSelect(t, s, `{"group":"EXIT-DC","member":"REJECT"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	now := f.all["EXIT-DC"].Now
	f.mu.Unlock()
	if now != "REJECT" {
		t.Errorf("now = %q", now)
	}
	var b exitsBody
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if len(b.Groups) != 1 || b.Groups[0].Now != "REJECT" {
		t.Errorf("ответ не отражает выбор: %s", rec.Body.String())
	}
}

// Переключать можно только группы EXIT-* и только на их участников:
// эндпоинт не должен становиться вторым способом перещёлкнуть PROXY или
// GLOBAL мимо своих правил.
func TestNikkiExitSelectRejects(t *testing.T) {
	s, _ := newServer(t)
	engineWithExits(t, s)
	for _, tc := range []struct{ body, code string }{
		{`{"group":"PROXY","member":"🇭🇳⚡Отель"}`, "bad_group"},
		{`{"group":"EXIT-DC","member":"🇭🇳⚡Отель"}`, "unknown_member"},
		{`{"group":"EXIT-NOPE","member":"REJECT"}`, "unknown_group"},
		{`{"group":"EXIT-DC"}`, "bad_request"},
	} {
		rec := postExitSelect(t, s, tc.body)
		if rec.Code < 400 || errCodeOf(t, rec) != tc.code {
			t.Errorf("%s: %d %s, ждали %s", tc.body, rec.Code, rec.Body.String(), tc.code)
		}
	}
}

func TestNikkiExitsEngineDown(t *testing.T) {
	s, _ := newServer(t)
	nikkiFake(t, s).err = nikki.ErrUnavailable
	if rec := do(t, s, http.MethodGet, "/api/nikki/exits", true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d", rec.Code)
	}
}

// У url-test-группы своя история замеров у mihomo часто пуста, а у её
// текущей цепочки замер есть: задержку группы берём у той, через которую
// она работает (замер на VM, mihomo 1.19.27).
func TestNikkiExitsChainsDelayFromNow(t *testing.T) {
	s, _ := newServer(t)
	f := engineWithExits(t, s)
	f.mu.Lock()
	g := f.all["EXIT-DC-HOP"]
	g.DelayMS = nil
	f.all["EXIT-DC-HOP"] = g
	f.mu.Unlock()
	hop := getExits(t, s).Groups[0].Members[0]
	if hop.DelayMS == nil || *hop.DelayMS != 929 {
		t.Errorf("задержка группы цепочек %v, ждали 929 от текущей цепочки", hop.DelayMS)
	}
}
