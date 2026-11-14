package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"netmoded/internal/exits"
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

// Прямой узел завёрнут в группу (EXIT-KZ-DC-DIRECT), чтобы стоять первым
// в select: mihomo ставит proxies: раньше узлов провайдера, и пустая группа
// цепочек при выключенном пуле иначе выбиралась по умолчанию. Такая группа
// — «напрямую», а не «через хоп»: в ней нет ни одной цепочки.
func TestNikkiExitsDirectGroupKind(t *testing.T) {
	s, _ := newServer(t)
	f := engineWithExits(t, s)
	f.mu.Lock()
	f.all["EXIT-DC"] = nikki.Proxy{Name: "EXIT-DC", Type: "Selector", Alive: true, Selectable: true,
		Members: []string{"EXIT-DC-DIRECT", "EXIT-DC-HOP", "REJECT"}, Now: "EXIT-DC-DIRECT"}
	f.all["EXIT-DC-DIRECT"] = nikki.Proxy{Name: "EXIT-DC-DIRECT", Type: "Selector", Alive: true, Selectable: true,
		Members: []string{"kz-nqz-exit-01-dc"}, Now: "kz-nqz-exit-01-dc"}
	f.mu.Unlock()
	b := getExits(t, s)
	if len(b.Groups) != 1 {
		t.Fatalf("группы %+v — EXIT-DC-DIRECT не должна быть отдельным выходом", b.Groups)
	}
	kinds := map[string]string{}
	for _, m := range b.Groups[0].Members {
		kinds[m.Name] = m.Kind
	}
	if kinds["EXIT-DC-DIRECT"] != "direct" || kinds["EXIT-DC-HOP"] != "chains" {
		t.Errorf("виды %v", kinds)
	}
}

// Цепочка «⇢ авто» в выходах подписывается реальным хопом повторителя, а
// не словом «авто»: владелец должен видеть, через что идёт resi.
func TestNikkiExitsAutoChainShowsHop(t *testing.T) {
	s, _ := newServer(t)
	f := engineWithExits(t, s)
	d := func(v int) *int { return &v }
	auto := "kz-nqz-exit-01-resi" + exits.ChainSep + exits.AutoHop
	hg := exits.HopGroupName("🇷🇺💳Россия")
	fg := exits.FollowGroupName("kz-nqz-exit-01")
	f.mu.Lock()
	f.all["EXIT-RESI"] = nikki.Proxy{Name: "EXIT-RESI", Type: "Selector", Alive: true, Selectable: true,
		Members: []string{"EXIT-RESI-HOP", "REJECT"}, Now: "EXIT-RESI-HOP"}
	f.all["EXIT-RESI-HOP"] = nikki.Proxy{Name: "EXIT-RESI-HOP", Type: "Selector", Alive: true, Selectable: true,
		Members: []string{auto}, Now: auto}
	f.all[auto] = nikki.Proxy{Name: auto, Type: "Vless", Alive: true, Provider: "exits"}
	f.all[fg] = nikki.Proxy{Name: fg, Type: "Selector", Alive: true, Selectable: true, Members: []string{hg, "REJECT"}, Now: hg}
	f.all[hg] = nikki.Proxy{Name: hg, Type: "Selector", Alive: true, Selectable: true, Members: []string{"🇷🇺💳Россия"}, Now: "🇷🇺💳Россия"}
	f.all["kz-nqz-exit-01-dc ⇢ 🇷🇺💳Россия"] = nikki.Proxy{Name: "kz-nqz-exit-01-dc ⇢ 🇷🇺💳Россия", Alive: true, DelayMS: d(137), Provider: "exits"}
	f.mu.Unlock()
	var resi *struct {
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		Now     string `json:"now"`
		DelayMS *int   `json:"delay_ms"`
		Alive   bool   `json:"alive"`
	}
	b := getExits(t, s)
	for gi := range b.Groups {
		if b.Groups[gi].Name == "EXIT-RESI" {
			resi = &b.Groups[gi].Members[0]
		}
	}
	if resi == nil || resi.Kind != "chains" || resi.Now != "kz-nqz-exit-01-resi ⇢ 🇷🇺💳Россия" {
		t.Fatalf("resi через повторитель: %+v", resi)
	}
	if resi.DelayMS == nil || *resi.DelayMS != 137 {
		t.Errorf("задержка — замер цепочки dc через тот же хоп: %v", resi.DelayMS)
	}
}
