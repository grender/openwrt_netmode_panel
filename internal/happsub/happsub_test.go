package happsub

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"netmoded/internal/exits"
	"netmoded/internal/mixin"
)

func node(t *testing.T) exits.Node {
	t.Helper()
	n, err := exits.Parse([]byte(`{"schema":1,"name":"kz-nqz-exit-01","country":"kz","address":"kz-nqz-exit-01.example.net",
	  "vless":{"port":443,"flow":"xtls-rprx-vision","sni":"krisha.kz","fingerprint":"chrome",
	  "public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","short_id":"0123abcd",
	  "users":{"resi":"11111111-2222-4333-8444-555555555555","dc":"66666666-7777-4888-9999-aaaaaaaaaaaa"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Хопы — в форме outbound провайдера (docs/recon/happ.md).
func hops() []Hop {
	vless := map[string]any{"tag": "proxy", "protocol": "vless",
		"settings": map[string]any{"vnext": []any{map[string]any{"address": "203.0.113.10", "port": 443,
			"users": []any{map[string]any{"id": "00000000-0000-0000-0000-000000000001", "encryption": "none", "flow": "xtls-rprx-vision"}}}}},
		"streamSettings": map[string]any{"network": "tcp", "security": "reality",
			"realitySettings": map[string]any{"serverName": "example.com", "publicKey": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "shortId": "ab", "fingerprint": "chrome"}}}
	ss := map[string]any{"tag": "proxy", "protocol": "shadowsocks",
		"settings": map[string]any{"servers": []any{map[string]any{"address": "203.0.113.20", "port": 8388, "method": "chacha20-ietf-poly1305", "password": "x"}}}}
	return []Hop{{Name: "🇩🇪⚡Германия", Outbound: vless}, {Name: "🇷🇺💳Россия", Outbound: ss}}
}

// tunnel — узлы авто-пула: через них «Умный» гонит туннельный трафик, как
// BYPASS на роутере. Не хопы: хопы подобраны для дороги до своей ноды
// (бывают с выходом в РФ), а туннель — для обхода.
func tunnel() []Hop {
	h := hops()
	return []Hop{{Name: "🇳🇱⚡Нидерланды", Outbound: h[0].Outbound}}
}

func rules() mixin.Config {
	return mixin.Config{Policy: mixin.PolicyTunnel, Download: mixin.DownloadDirect,
		Sets: []mixin.Set{
			{Name: "openai", Action: mixin.ActionExit, Group: "EXIT-RESI"},
			{Name: "telegram", IP: true, Action: mixin.ActionExit, Group: "EXIT-DC"},
			{Name: "youtube", Action: mixin.ActionTunnel},
			{Name: "category-ru", Action: mixin.ActionDirect},
			{Name: "netflix", Action: mixin.ActionExit, Group: "EXIT-NOPE"},
		},
		Rules: []mixin.Rule{{Kind: mixin.RuleSuffix, Value: "worldsimseries.com", Action: mixin.ActionTunnel},
			{Kind: mixin.RuleDomain, Value: "chat.example.org", Action: mixin.ActionExit, Group: "EXIT-RESI"},
			{Kind: mixin.RuleCIDR, Value: "100.64.0.0/10", Action: mixin.ActionDirect}},
	}
}

func build(t *testing.T, in Input) []map[string]any {
	t.Helper()
	b, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("подписка — не JSON-массив: %v", err)
	}
	return out
}

func remarks(cs []map[string]any) []string {
	var r []string
	for _, c := range cs {
		r = append(r, c["remarks"].(string))
	}
	return r
}

func outbounds(c map[string]any) []map[string]any {
	var out []map[string]any
	for _, o := range c["outbounds"].([]any) {
		out = append(out, o.(map[string]any))
	}
	return out
}

func dialer(o map[string]any) string {
	ss, _ := o["streamSettings"].(map[string]any)
	so, _ := ss["sockopt"].(map[string]any)
	d, _ := so["dialerProxy"].(string)
	return d
}

func TestBuildWithoutHops(t *testing.T) {
	got := remarks(build(t, Input{Nodes: []exits.Node{node(t)}, Rules: rules()}))
	want := []string{"Умный · прямо", "kz-nqz-exit-01 · dc · прямо", "kz-nqz-exit-01 · resi · прямо"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("записи %q", got)
	}
}

func TestBuildWithHops(t *testing.T) {
	cs := build(t, Input{Nodes: []exits.Node{node(t)}, Hops: hops(), Rules: rules()})
	want := []string{"Умный · прямо", "Умный ⇢ хоп",
		"kz-nqz-exit-01 · dc · прямо", "kz-nqz-exit-01 · dc ⇢ 🇩🇪⚡Германия", "kz-nqz-exit-01 · dc ⇢ 🇷🇺💳Россия",
		"kz-nqz-exit-01 · resi · прямо", "kz-nqz-exit-01 · resi ⇢ 🇩🇪⚡Германия", "kz-nqz-exit-01 · resi ⇢ 🇷🇺💳Россия"}
	if got := remarks(cs); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("записи:\n%s", strings.Join(got, "\n"))
	}
	chain := outbounds(cs[3])
	if chain[0]["tag"] != "proxy" || dialer(chain[0]) != "hop" {
		t.Errorf("цепочка: первый outbound %v → %q", chain[0]["tag"], dialer(chain[0]))
	}
	if chain[1]["tag"] != "hop" || chain[1]["protocol"] != "vless" {
		t.Errorf("хоп: %v", chain[1])
	}
	// Хоп — в форме провайдера: исходный объект не тронут (тег — в копии).
	if hops()[0].Outbound["tag"] != "proxy" {
		t.Error("исходный outbound хопа изменён")
	}
}

// Спайк 2 (docs/recon/exit-chains.md): dialerProxy на балансировщик Xray
// молча не работает. Каждый dialerProxy обязан указывать на outbound того
// же конфига.
func TestEveryDialerProxyIsAnOutbound(t *testing.T) {
	for _, c := range build(t, Input{Nodes: []exits.Node{node(t)}, Hops: hops(), Rules: rules()}) {
		tags := map[string]bool{}
		for _, o := range outbounds(c) {
			tags[o["tag"].(string)] = true
		}
		for _, o := range outbounds(c) {
			if d := dialer(o); d != "" && !tags[d] {
				t.Errorf("%s: %s → dialerProxy %q — такого outbound нет", c["remarks"], o["tag"], d)
			}
		}
	}
}

func routing(c map[string]any) (rules []map[string]any, balancers map[string][]string) {
	r := c["routing"].(map[string]any)
	for _, x := range r["rules"].([]any) {
		rules = append(rules, x.(map[string]any))
	}
	balancers = map[string][]string{}
	if bs, ok := r["balancers"].([]any); ok {
		for _, b := range bs {
			m := b.(map[string]any)
			var sel []string
			for _, s := range m["selector"].([]any) {
				sel = append(sel, s.(string))
			}
			balancers[m["tag"].(string)] = sel
		}
	}
	return rules, balancers
}

func target(r map[string]any) string {
	if b, ok := r["balancerTag"].(string); ok {
		return "balancer:" + b
	}
	return r["outboundTag"].(string)
}

func findRule(rs []map[string]any, key, val string) map[string]any {
	for _, r := range rs {
		if vs, ok := r[key].([]any); ok {
			for _, v := range vs {
				if v == val {
					return r
				}
			}
		}
	}
	return nil
}

// «Умный ⇢ хоп»: наборы панели → балансировщики над ЦЕЛЫМИ цепочками
// (observatory пробует цепочку до ноды и отсеивает хопы с подменой SNI),
// туннель — над хопами.
func TestSmartViaHopRules(t *testing.T) {
	cs := build(t, Input{Nodes: []exits.Node{node(t)}, Hops: hops(), Tunnel: tunnel(), Rules: rules()})
	rs, bal := routing(cs[1])
	if strings.Join(bal["resi"], ",") != "chain-resi-" || strings.Join(bal["dc"], ",") != "chain-dc-" || strings.Join(bal["tunnel"], ",") != "tun-" {
		t.Errorf("балансировщики %v", bal)
	}
	for _, tc := range []struct{ key, val, want string }{
		{"domain", "geosite:openai", "balancer:resi"},
		{"domain", "geosite:telegram", "balancer:dc"},
		{"ip", "geoip:telegram", "balancer:dc"},
		{"domain", "geosite:youtube", "balancer:tunnel"},
		{"domain", "geosite:category-ru", "direct"},
		// Группы EXIT-NOPE нет среди выходов нод — fail closed, как
		// REJECT на роутере, а не туннель и не прямо.
		{"domain", "geosite:netflix", "block"},
		{"domain", "domain:worldsimseries.com", "balancer:tunnel"},
		{"domain", "full:chat.example.org", "balancer:resi"},
		{"ip", "100.64.0.0/10", "direct"},
	} {
		r := findRule(rs, tc.key, tc.val)
		if r == nil {
			t.Errorf("нет правила для %s", tc.val)
			continue
		}
		if got := target(r); got != tc.want {
			t.Errorf("%s → %s, ждали %s", tc.val, got, tc.want)
		}
	}
	// Свои правила — раньше наборов, как на роутере.
	if indexOf(rs, "full:chat.example.org") > indexOf(rs, "geosite:openai") {
		t.Error("свои правила стоят после наборов")
	}
	last := rs[len(rs)-1]
	if target(last) != "balancer:tunnel" {
		t.Errorf("политика tunnel → хвост %v", target(last))
	}
	// Первый outbound — узел туннеля: его Happ пингует как «сам узел».
	if outbounds(cs[1])[0]["tag"] != "tun-0" {
		t.Errorf("первый outbound %v", outbounds(cs[1])[0]["tag"])
	}
	for _, o := range outbounds(cs[1]) {
		if strings.HasPrefix(o["tag"].(string), "chain-") && !strings.HasPrefix(dialer(o), "hop-") {
			t.Errorf("%s: dialerProxy %q", o["tag"], dialer(o))
		}
	}
	obs, ok := cs[1]["observatory"].(map[string]any)
	if !ok {
		t.Fatal("нет observatory — leastPing выбирать не из чего")
	}
	// Без параллельных проб Xray проверяет по одному outbound с паузой
	// probeInterval между КАЖДЫМ: за минуту после старта замерена одна
	// цепочка, балансировщики пусты и уходят в fallback block (замер на
	// Xray 26.10.21).
	if obs["enableConcurrency"] != true {
		t.Errorf("enableConcurrency = %v", obs["enableConcurrency"])
	}
}

// Туннель — из авто-пула, а не из хопов, в обоих «Умных».
func TestSmartTunnelFromAutopool(t *testing.T) {
	cs := build(t, Input{Nodes: []exits.Node{node(t)}, Hops: hops(), Tunnel: tunnel(), Rules: rules()})
	for _, c := range cs[:2] {
		rs, bal := routing(c)
		if strings.Join(bal["tunnel"], ",") != "tun-" {
			t.Errorf("%s: туннель %v", c["remarks"], bal["tunnel"])
		}
		if r := findRule(rs, "domain", "geosite:youtube"); r == nil || target(r) != "balancer:tunnel" {
			t.Errorf("%s: youtube %v", c["remarks"], r)
		}
		if target(rs[0]) != "balancer:tunnel" {
			t.Errorf("%s: DoH → %s, ждали туннель", c["remarks"], target(rs[0]))
		}
	}
}

// «Умный · прямо» без авто-пула: туннель некуда — его правила fail closed.
func TestSmartDirectWithoutHopsBlocksTunnel(t *testing.T) {
	cs := build(t, Input{Nodes: []exits.Node{node(t)}, Rules: rules()})
	rs, bal := routing(cs[0])
	if _, ok := bal["tunnel"]; ok {
		t.Error("балансировщик туннеля без хопов")
	}
	if r := findRule(rs, "domain", "geosite:youtube"); r == nil || target(r) != "block" {
		t.Errorf("туннельный набор без хопов: %v", r)
	}
	if r := findRule(rs, "domain", "geosite:openai"); r == nil || target(r) != "balancer:resi" {
		t.Errorf("openai: %v", r)
	}
	if strings.Join(bal["resi"], ",") != "exit-resi-" {
		t.Errorf("resi напрямую: %v", bal["resi"])
	}
}

// DNS идёт отдельным правилом, не через выход (спайк 3: в v1 DoH уходил в
// resi и ждал по 10 с), и стоит первым.
func TestDNSRuleFirstAndNotViaExit(t *testing.T) {
	for _, c := range build(t, Input{Nodes: []exits.Node{node(t)}, Hops: hops(), Rules: rules()}) {
		rs, _ := routing(c)
		if findRule(rs, "ip", "8.8.8.8") == nil || rs[0]["port"] != "443" {
			t.Errorf("%s: первое правило не DoH: %v", c["remarks"], rs[0])
			continue
		}
		switch got := target(rs[0]); got {
		case "direct", "hop", "balancer:tunnel":
		default:
			t.Errorf("%s: DoH → %s", c["remarks"], got)
		}
		if c["routing"].(map[string]any)["domainStrategy"] != "AsIs" {
			t.Errorf("%s: domainStrategy не AsIs", c["remarks"])
		}
	}
}

// Хоп, который сам цепляется через другой outbound, вне своей записи не
// работает — его пропускаем, а не отдаём полуконфиг.
func TestHopWithOwnDialerSkipped(t *testing.T) {
	h := hops()
	h[0].Outbound["streamSettings"].(map[string]any)["sockopt"] = map[string]any{"dialerProxy": "proxy-decoy"}
	got := strings.Join(remarks(build(t, Input{Nodes: []exits.Node{node(t)}, Hops: h, Rules: rules()})), "|")
	if strings.Contains(got, "Германия") {
		t.Errorf("хоп с собственным dialerProxy попал в подписку: %s", got)
	}
}

func indexOf(rs []map[string]any, val string) int {
	for i, r := range rs {
		for _, k := range []string{"domain", "ip"} {
			if vs, ok := r[k].([]any); ok {
				for _, v := range vs {
					if v == val {
						return i
					}
				}
			}
		}
	}
	return -1
}

// Каждый конфиг — через настоящий xray: `xray run -test`. Нужен путь к
// бинарю и каталог с geosite.dat/geoip.dat (наборы ссылаются на geosite).
// Без них тест пропускается: make verify не зависит от xray на машине.
//
//	XRAY_TEST_BIN=…/xray XRAY_LOCATION_ASSET=…/assets go test ./internal/happsub/
func TestConfigsPassXrayTest(t *testing.T) {
	bin := os.Getenv("XRAY_TEST_BIN")
	if bin == "" || os.Getenv("XRAY_LOCATION_ASSET") == "" {
		t.Skip("XRAY_TEST_BIN и XRAY_LOCATION_ASSET не заданы")
	}
	dir := t.TempDir()
	r := rules()
	r.Sets = r.Sets[:4] // netflix-с-несуществующей-группой проверен выше; тут — валидность
	for i, c := range build(t, Input{Nodes: []exits.Node{node(t)}, Hops: hops(), Rules: r}) {
		p := filepath.Join(dir, "c.json")
		b, _ := json.Marshal(c)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "run", "-test", "-c", p).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Configuration OK") {
			t.Errorf("%d %s: xray -test: %v\n%s", i, c["remarks"], err, out)
		}
	}
}
