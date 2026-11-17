package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"netmoded/internal/exits"
	"netmoded/internal/mixin"
	"netmoded/internal/nikki"
)

const (
	ptNode = "kz-nqz-exit-01"
	ptHop  = "🇭🇳⚡Отель"
)

// DelayTimeout — проба с заданным таймаутом (nikki.TimedProber). Записывает,
// с каким таймаутом кого пробовали: медленный выход обязан получить свой.
func (f *fakeNikkiClient) DelayTimeout(ctx context.Context, provider, name string, timeout time.Duration) (int, error) {
	f.mu.Lock()
	if f.delayTimeouts == nil {
		f.delayTimeouts = map[string]time.Duration{}
	}
	f.delayTimeouts[name] = timeout
	f.mu.Unlock()
	return f.Delay(ctx, provider, name)
}

// pathTestServer — нода с выходами dc и resi, пул из одного хопа, повторитель
// стоит на этом хопе. Узлы провайдера exits — все живые.
func pathTestServer(t *testing.T) (*Server, *fakeNikkiClient) {
	t.Helper()
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, ptNode, "krisha.kz")
	writeMixin(t, s, mixin.Render(mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops:        mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{ptHop}, Pool: []string{ptHop}},
		FollowNodes: []string{ptNode}}))
	engineHasHopGroups(t, s, ptHop)
	f := nikkiFake(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	fg := f.all[exits.FollowGroupName(ptNode)]
	fg.Now = exits.HopGroupName(ptHop)
	f.all[exits.FollowGroupName(ptNode)] = fg
	for _, n := range []string{
		ptNode + "-dc", ptNode + "-resi",
		ptNode + "-dc" + exits.ChainSep + ptHop,
		ptNode + "-resi" + exits.ChainSep + exits.AutoHop,
	} {
		f.all[n] = nikki.Proxy{Name: n, Type: "Vless", Alive: true, Provider: exitsProviderName}
	}
	return s, f
}

type ptProbe struct {
	State   string `json:"state"`
	DelayMS *int   `json:"delay_ms"`
}

type ptResponse struct {
	Hops []struct {
		Name   string `json:"name"`
		Pool   bool   `json:"pool"`
		Tunnel bool   `json:"tunnel"`
		ptProbe
	} `json:"hops"`
	Nodes []struct {
		Name   string   `json:"name"`
		Direct *ptProbe `json:"direct"`
		Chains []struct {
			Hop string `json:"hop"`
			ptProbe
		} `json:"chains"`
		Exits []struct {
			Exit string `json:"exit"`
			Via  string `json:"via"`
			Hop  string `json:"hop"`
			ptProbe
		} `json:"exits"`
	} `json:"nodes"`
	ElapsedMS int `json:"elapsed_ms"`
}

func postPathTest(t *testing.T, s *Server) (ptResponse, string) {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/nikki/pathtest", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var got ptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	return got, rec.Body.String()
}

func kill(f *fakeNikkiClient, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.all[name]
	p.Alive = false
	f.all[name] = p
}

func TestPathTestAllAlive(t *testing.T) {
	s, f := pathTestServer(t)
	got, body := postPathTest(t, s)
	assertNoSecrets(t, body)

	if len(got.Hops) == 0 || got.Hops[0].Name != ptHop || !got.Hops[0].Pool || got.Hops[0].State != "ok" {
		t.Fatalf("hops = %+v", got.Hops)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Name != ptNode {
		t.Fatalf("nodes = %+v", got.Nodes)
	}
	n := got.Nodes[0]
	if n.Direct == nil || n.Direct.State != "ok" || n.Direct.DelayMS == nil {
		t.Errorf("direct = %+v", n.Direct)
	}
	if len(n.Chains) != 1 || n.Chains[0].Hop != ptHop || n.Chains[0].State != "ok" {
		t.Errorf("chains = %+v", n.Chains)
	}
	if len(n.Exits) != 1 || n.Exits[0].Exit != "resi" || n.Exits[0].Via != "direct" || n.Exits[0].State != "ok" {
		t.Errorf("exits = %+v", n.Exits)
	}

	// Медленный выход — одно соединение и свой, длинный таймаут; цепочку
	// «⇢ авто» при живом прямом пути не трогаем вовсе.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delayTimeouts[ptNode+"-resi"] != slowExitTimeout {
		t.Errorf("таймаут resi = %v, ждали %v", f.delayTimeouts[ptNode+"-resi"], slowExitTimeout)
	}
	resi := 0
	for _, d := range f.delayed {
		if d == ptNode+"-resi" || d == ptNode+"-resi"+exits.ChainSep+exits.AutoHop {
			resi++
		}
	}
	if resi != 1 {
		t.Errorf("проб через ISP-прокси %d, ждали ровно одну: %v", resi, f.delayed)
	}
	if f.delayedFrom[ptNode+"-dc"] != exitsProviderName {
		t.Errorf("узел ноды пробовали не через провайдер exits: %q", f.delayedFrom[ptNode+"-dc"])
	}
}

// Выход resi мёртв при живой ноде — это и есть «отказал ISP-прокси».
func TestPathTestResiDeadNodeAlive(t *testing.T) {
	s, f := pathTestServer(t)
	kill(f, ptNode+"-resi")
	got, _ := postPathTest(t, s)
	n := got.Nodes[0]
	if n.Direct.State != "ok" {
		t.Errorf("direct = %+v", n.Direct)
	}
	if n.Exits[0].State != "fail" || n.Exits[0].Via != "direct" || n.Exits[0].DelayMS != nil {
		t.Errorf("exits = %+v", n.Exits)
	}
}

// Прямой путь до ноды закрыт, цепочка жива: resi проверяется через хоп
// повторителя — иначе «мёртв ISP-прокси» означало бы «не дошли до ноды».
func TestPathTestResiViaHopWhenDirectDead(t *testing.T) {
	s, f := pathTestServer(t)
	kill(f, ptNode+"-dc")
	kill(f, ptNode+"-resi")
	got, _ := postPathTest(t, s)
	n := got.Nodes[0]
	if n.Direct.State != "fail" {
		t.Errorf("direct = %+v", n.Direct)
	}
	e := n.Exits[0]
	if e.Via != "hop" || e.Hop != ptHop || e.State != "ok" {
		t.Errorf("exits = %+v", n.Exits)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.delayed {
		if d == ptNode+"-resi" {
			t.Errorf("прямой resi пробовали при мёртвом прямом пути: %v", f.delayed)
		}
	}
}

// Узла нет у движка (провайдер не перечитан) — «absent», а не «мёртв».
func TestPathTestAbsentNode(t *testing.T) {
	s, f := pathTestServer(t)
	f.mu.Lock()
	delete(f.all, ptNode+"-dc"+exits.ChainSep+ptHop)
	f.mu.Unlock()
	got, _ := postPathTest(t, s)
	if c := got.Nodes[0].Chains[0]; c.State != "absent" {
		t.Errorf("chain = %+v", c)
	}
}

func TestPathTestEngineDown(t *testing.T) {
	s, f := pathTestServer(t)
	f.mu.Lock()
	f.err = nikki.ErrUnavailable
	f.mu.Unlock()
	rec := do(t, s, http.MethodPost, "/api/nikki/pathtest", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPathTestNeedsToken(t *testing.T) {
	s, _ := pathTestServer(t)
	rec := do(t, s, http.MethodPost, "/api/nikki/pathtest", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("код %d", rec.Code)
	}
}
