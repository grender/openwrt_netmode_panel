package exits

import (
	"regexp"
	"strings"
	"testing"
)

// Хоп выбирается по замерам dc, resi повторяет его (ADR-0050, «Автоподбор
// хопа»): у dc — цепочка на каждый хоп (их меряет url-test профиля), у
// остальных выходов — одна «⇢ авто» через группу-повторитель. Проверки
// resi — новые соединения через ISP-прокси, который отказывает на частых
// соединениях; их здесь нет вовсе.
func TestRenderFollowLayout(t *testing.T) {
	hops := []string{"🇩🇪⚡Германия", "🇷🇺💳Россия"}
	ps := decodeProvider(t, RenderProvider([]Node{mustNode(t)}, hops))
	want := []string{
		"kz-nqz-exit-01-dc", "kz-nqz-exit-01-dc ⇢ 🇩🇪⚡Германия", "kz-nqz-exit-01-dc ⇢ 🇷🇺💳Россия",
		"kz-nqz-exit-01-resi", "kz-nqz-exit-01-resi ⇢ авто",
	}
	if got := proxyNames(ps); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("узлы:\n%s", strings.Join(got, "\n"))
	}
	if ps[4]["dialer-proxy"] != FollowGroupName("kz-nqz-exit-01") {
		t.Errorf("resi ⇢ авто: dialer-proxy %v", ps[4]["dialer-proxy"])
	}
	if ps[4]["uuid"] != ps[3]["uuid"] {
		t.Error("цепочка авто не того выхода")
	}
}

// Цепочка «⇢ авто» есть и без пула: повторитель тогда [REJECT], и группа
// resi в профиле никогда не пустеет — никакого COMPATIBLE = DIRECT.
func TestRenderFollowWithoutHops(t *testing.T) {
	got := strings.Join(proxyNames(decodeProvider(t, RenderProvider([]Node{mustNode(t)}, nil))), "|")
	if got != "kz-nqz-exit-01-dc|kz-nqz-exit-01-resi|kz-nqz-exit-01-resi ⇢ авто" {
		t.Fatalf("узлы без пула: %s", got)
	}
	gs := FollowGroups([]Node{mustNode(t)}, nil)
	if len(gs) != 1 || strings.Join(gs[0]["proxies"].([]string), ",") != "REJECT" {
		t.Fatalf("повторитель без пула: %v", gs)
	}
}

// Повторитель: select из групп-однохопников пула и REJECT последним —
// никогда DIRECT. До первой синхронизации движок берёт первый хоп.
func TestFollowGroups(t *testing.T) {
	hops := []string{"🇩🇪⚡Германия", "🇷🇺💳Россия"}
	gs := FollowGroups([]Node{mustNode(t)}, hops)
	if len(gs) != 1 {
		t.Fatalf("групп %d", len(gs))
	}
	g := gs[0]
	if g["name"] != FollowGroupName("kz-nqz-exit-01") || g["type"] != "select" {
		t.Errorf("группа %v", g)
	}
	want := HopGroupName(hops[0]) + "," + HopGroupName(hops[1]) + ",REJECT"
	if got := strings.Join(g["proxies"].([]string), ","); got != want {
		t.Errorf("участники %s, ждали %s", got, want)
	}
	for _, m := range g["proxies"].([]string) {
		if m == "DIRECT" || m == "COMPATIBLE" {
			t.Errorf("в повторителе %s — kill switch", m)
		}
	}
	if !regexp.MustCompile(`^nm-follow-[0-9a-f]{8}$`).MatchString(g["name"].(string)) {
		t.Errorf("имя %v", g["name"])
	}
}

// Нода без dc меряет нечем — её выходы получают цепочки по-старому, на
// каждый хоп, и повторителя у неё нет.
func TestRenderNodeWithoutDC(t *testing.T) {
	n := mustNode(t)
	delete(n.Users, "dc")
	got := strings.Join(proxyNames(decodeProvider(t, RenderProvider([]Node{n}, []string{"h1"}))), "|")
	if got != "kz-nqz-exit-01-resi|kz-nqz-exit-01-resi ⇢ h1" {
		t.Fatalf("узлы: %s", got)
	}
	if gs := FollowGroups([]Node{n}, []string{"h1"}); len(gs) != 0 {
		t.Fatalf("повторитель у ноды без dc: %v", gs)
	}
}

// FollowNodes — ноды, которым нужен повторитель: есть dc и есть что
// повторять.
func TestFollowNodes(t *testing.T) {
	a := mustNode(t)
	b := mustNode(t)
	b.Name = "kz-ala-exit-02"
	delete(b.Users, "resi")
	if got := strings.Join(FollowNodes([]Node{a, b}), ","); got != "kz-nqz-exit-01" {
		t.Fatalf("FollowNodes = %s", got)
	}
}
