package httpapi

import (
	"context"
	"testing"

	"netmoded/internal/exits"
	"netmoded/internal/mixin"
	"netmoded/internal/nikki"
)

const syncNode = "kz-nqz-exit-01"

// syncEngine — движок после склейки: однохопники A и B, повторитель ноды на
// now, цепочки dc с заданными задержками (nil — мертва).
func syncEngine(t *testing.T, s *Server, now string, delays map[string]*int) *fakeNikkiClient {
	t.Helper()
	writeMixin(t, s, mixin.Render(mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops:        mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"A", "B"}, Pool: []string{"A", "B"}},
		FollowNodes: []string{syncNode}}))
	f := nikkiFake(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	fg := exits.FollowGroupName(syncNode)
	f.all[fg] = nikki.Proxy{Name: fg, Type: "Selector", Alive: true, Selectable: true,
		Members: []string{exits.HopGroupName("A"), exits.HopGroupName("B"), "REJECT"}, Now: now}
	for _, h := range []string{"A", "B"} {
		name := syncNode + "-dc" + exits.ChainSep + h
		d := delays[h]
		f.all[name] = nikki.Proxy{Name: name, Type: "Vless", Alive: d != nil, DelayMS: d, Provider: "exits"}
	}
	return f
}

func ms(v int) *int { return &v }

func followNow(t *testing.T, f *fakeNikkiClient) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.all[exits.FollowGroupName(syncNode)].Now
}

func TestHopSyncPicksBestDCChain(t *testing.T) {
	s, _ := newServer(t)
	f := syncEngine(t, s, exits.HopGroupName("A"), map[string]*int{"A": ms(900), "B": ms(120)})
	s.syncHopsOnce(context.Background())
	if got := followNow(t, f); got != exits.HopGroupName("B") {
		t.Fatalf("повторитель %q, ждали хоп B (лучшая цепочка dc)", got)
	}
}

// Гистерезис: живой текущий хоп не хуже лучшего больше чем на порог —
// остаётся, иначе resi дёргался бы между хопами на каждом замере.
func TestHopSyncHysteresis(t *testing.T) {
	s, _ := newServer(t)
	f := syncEngine(t, s, exits.HopGroupName("A"), map[string]*int{"A": ms(200), "B": ms(120)})
	s.syncHopsOnce(context.Background())
	if got := followNow(t, f); got != exits.HopGroupName("A") {
		t.Fatalf("повторитель %q — переключился внутри порога", got)
	}
}

// Ни одной живой цепочки dc — повторитель в REJECT: kill switch, resi не
// идёт через хоп, про который известно только плохое.
func TestHopSyncAllDeadRejects(t *testing.T) {
	s, _ := newServer(t)
	f := syncEngine(t, s, exits.HopGroupName("A"), map[string]*int{})
	s.syncHopsOnce(context.Background())
	if got := followNow(t, f); got != "REJECT" {
		t.Fatalf("повторитель %q, ждали REJECT", got)
	}
}

// Ожила цепочка — повторитель уходит из REJECT.
func TestHopSyncRecoversFromReject(t *testing.T) {
	s, _ := newServer(t)
	f := syncEngine(t, s, "REJECT", map[string]*int{"B": ms(300)})
	s.syncHopsOnce(context.Background())
	if got := followNow(t, f); got != exits.HopGroupName("B") {
		t.Fatalf("повторитель %q, ждали B", got)
	}
}

// Не режим nikki — синхронизировать нечего, движок не трогается.
func TestHopSyncIdleOutsideNikki(t *testing.T) {
	s, ex := newServer(t)
	ex.UCIValues["netmode.main.mode"] = "off"
	f := syncEngine(t, s, exits.HopGroupName("A"), map[string]*int{"A": ms(900), "B": ms(100)})
	s.syncHopsOnce(context.Background())
	if got := followNow(t, f); got != exits.HopGroupName("A") {
		t.Fatalf("вне режима nikki повторитель тронут: %q", got)
	}
}

// Ранжирование — общее для синхронизации и подписки телефонов: живые
// цепочки dc по задержке, лучший — по всем нодам.
func TestHopRanking(t *testing.T) {
	all := map[string]nikki.Proxy{
		syncNode + "-dc ⇢ A": {Alive: true, DelayMS: ms(300)},
		syncNode + "-dc ⇢ B": {Alive: true, DelayMS: ms(100)},
		syncNode + "-dc ⇢ C": {Alive: false, DelayMS: ms(50)},
	}
	got := hopRanking(all, []string{syncNode}, []string{"A", "B", "C", "D"})
	if len(got) != 2 || got[0].Hop != "B" || got[1].Hop != "A" {
		t.Fatalf("ранжирование %+v", got)
	}
}
