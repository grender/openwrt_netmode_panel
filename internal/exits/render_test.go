package exits

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func mustNode(t *testing.T) Node {
	t.Helper()
	n, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func decodeProvider(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var f struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("файл провайдера не JSON: %v", err)
	}
	return f.Proxies
}

func proxyNames(ps []map[string]any) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p["name"].(string)
	}
	return out
}

func TestRenderDirectNodesHaveNoDialer(t *testing.T) {
	ps := decodeProvider(t, RenderProvider([]Node{mustNode(t)}, nil))
	for _, p := range ps {
		name := p["name"].(string)
		if _, ok := p["dialer-proxy"]; ok != strings.Contains(name, ChainSep) {
			t.Errorf("%s: dialer-proxy %v", name, p["dialer-proxy"])
		}
	}
}

// Поля — как в templates/mihomo-proxies.yaml.j2 репо нод: этот узел уже
// проверен на живой ноде, отступать от него незачем.
func TestRenderDirectFields(t *testing.T) {
	ps := decodeProvider(t, RenderProvider([]Node{mustNode(t)}, nil))
	dc := ps[0]
	want := map[string]any{
		"name": "kz-nqz-exit-01-dc", "type": "vless", "server": "kz-nqz-exit-01.example.net",
		"port": float64(443), "uuid": "66666666-7777-4888-9999-aaaaaaaaaaaa", "network": "tcp",
		"udp": true, "tls": true, "flow": "xtls-rprx-vision", "servername": "krisha.kz",
		"client-fingerprint": "chrome",
	}
	for k, v := range want {
		if dc[k] != v {
			t.Errorf("%s = %v, ждали %v", k, dc[k], v)
		}
	}
	ro, _ := dc["reality-opts"].(map[string]any)
	if ro["public-key"] != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" || ro["short-id"] != "0123abcd" {
		t.Errorf("reality-opts = %v", ro)
	}
	// Без этого mihomo получает «REALITY authentication failed», если сайт
	// маскировки поддерживает X25519MLKEM768 (CLAUDE.md репо нод).
	if ro["support-x25519mlkem768"] != true {
		t.Errorf("нет support-x25519mlkem768: true")
	}
}

// Главный инвариант раскладки (docs/recon/exit-chains.md): dialer-proxy
// разрешается только в группу верхнего уровня, mihomo -t это не ловит, и
// узел провайдера со ссылкой в пустоту отказывает на каждом соединении.
// Поэтому каждая ссылка обязана указывать на группу, которую пишет
// HopGroups из того же пула.
func TestEveryDialerProxyHasItsGroup(t *testing.T) {
	hops := []string{"🇩🇪⚡Германия", "🇹🇷⚪Турция (БС-3)☁️", "a.b|c"}
	n2 := mustNode(t)
	n2.Name = "kz-ala-exit-02"
	groups := map[string]bool{}
	for _, g := range append(HopGroups(hops), FollowGroups([]Node{mustNode(t), n2}, hops)...) {
		groups[g["name"].(string)] = true
	}
	for _, p := range decodeProvider(t, RenderProvider([]Node{mustNode(t), n2}, hops)) {
		if d, ok := p["dialer-proxy"].(string); ok && !groups[d] {
			t.Errorf("%s: dialer-proxy %q — группы нет", p["name"], d)
		}
	}
}

// Группа выбирает из провайдера sub ровно один узел. Фильтр — регулярка
// mihomo, а имена провайдера бывают со скобками, точками и чертой.
func TestHopGroupFilterMatchesExactlyOne(t *testing.T) {
	names := []string{"🇹🇷⚪Турция (БС-3)☁️", "🇹🇷⚪Турция (БС-3)☁️ EXTRA", "x🇹🇷⚪Турция (БС-3)☁️", "a.b|c", "aXb|c", "a.b"}
	for _, hop := range []string{"🇹🇷⚪Турция (БС-3)☁️", "a.b|c"} {
		g := HopGroups([]string{hop})[0]
		if g["type"] != "select" {
			t.Errorf("type = %v", g["type"])
		}
		if g["empty-fallback"] != "REJECT" {
			t.Errorf("empty-fallback = %v: пустая группа хопа иначе COMPATIBLE = DIRECT", g["empty-fallback"])
		}
		if use, _ := g["use"].([]string); len(use) != 1 || use[0] != "sub" {
			t.Errorf("use = %v, ждали [sub]", g["use"])
		}
		re := regexp.MustCompile(g["filter"].(string))
		var hit []string
		for _, n := range names {
			if re.MatchString(n) {
				hit = append(hit, n)
			}
		}
		if len(hit) != 1 || hit[0] != hop {
			t.Errorf("фильтр %q для %q совпал с %v", g["filter"], hop, hit)
		}
	}
}

// Имя группы — от имени хопа, а не от позиции в пуле: миксин применяется
// перезапуском движка, провайдер — перечиткой, и между ними файлы живут в
// разных версиях. Позиционное имя увело бы цепочку через чужой хоп.
func TestHopGroupNameStable(t *testing.T) {
	a := HopGroupName("🇩🇪⚡Германия")
	if a != HopGroupName("🇩🇪⚡Германия") || a == HopGroupName("🇷🇺💳Россия") {
		t.Fatal("имя группы не детерминировано или совпало у разных хопов")
	}
	if !regexp.MustCompile(`^nm-hop-[0-9a-f]{8}$`).MatchString(a) {
		t.Fatalf("имя %q не в форме nm-hop-<8 hex>", a)
	}
}

func TestRenderEmptyIsValidFile(t *testing.T) {
	if ps := decodeProvider(t, RenderProvider(nil, []string{"x"})); len(ps) != 0 {
		t.Fatalf("без нод узлы: %v", ps)
	}
}
