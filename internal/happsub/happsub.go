// Package happsub собирает подписку для Happ на телефонах: свои выходы
// напрямую и цепочками через узлы подписки, плюс «Умный маршрут» с
// правилами из наборов панели.
//
// Формат — тот же, что отдаёт провайдер на User-Agent Happ: JSON-массив
// полных конфигов Xray, по одному на запись (docs/recon/happ.md). Что
// здесь сделано именно так — по спайкам на телефоне и Xray 26.10.21
// (docs/recon/exit-chains.md, ADR-0050):
//
//   - dialerProxy указывает только на outbound: на балансировщик он молча
//     не работает. Поэтому запись «через хоп» — по одной на хоп, а «Умный»
//     держит балансировщики над ЦЕЛЫМИ цепочками (observatory пробует
//     цепочку до ноды и сам отсеивает хопы, подменяющие адрес по SNI);
//   - domainStrategy AsIs и DoH отдельным первым правилом через хоп или
//     напрямую — не через выход: иначе каждый DNS-запрос шёл через resi;
//   - первым outbound стоит хоп или прямой выход dc: Happ пингует первый.
package happsub

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"netmoded/internal/exits"
	"netmoded/internal/mixin"
)

// Hop — узел подписки, через который строится цепочка, в форме outbound
// Xray провайдера (internal/happ, outbounds.json).
type Hop struct {
	Name     string
	Outbound map[string]any
}

// Input — из чего собирается подписка.
type Input struct {
	Nodes []exits.Node
	// Hops — пул хопов в его порядке (mixin.HopConfig.Pool): через них
	// цепочки до своих нод.
	Hops []Hop
	// Tunnel — узлы авто-пула: куда «Умный» гонит туннельный трафик, как
	// BYPASS на роутере. Не хопы: хопы подобраны для дороги до своей ноды и
	// бывают с выходом в РФ (там YouTube режут), а туннель — для обхода.
	Tunnel []Hop
	// Rules — выбор наборов и своих правил панели.
	Rules mixin.Config
}

var (
	dohServers = []string{"https://8.8.8.8/dns-query", "https://8.8.4.4/dns-query"}
	dohIPs     = []string{"8.8.8.8", "8.8.4.4"}
)

const (
	probeURL      = "https://www.gstatic.com/generate_204"
	probeInterval = "5m"
	// Записи с правилами; хвост имени говорит, как они ходят на ноды.
	smartDirect = "Умный · прямо"
	smartViaHop = "Умный ⇢ хоп"
)

// Build собирает подписку. Порядок записей: «Умный» (прямо, через хоп),
// затем по ноде и выходу — прямо и через каждый хоп.
func Build(in Input) ([]byte, error) {
	hops := usableHops(in.Hops)
	tun := usableHops(in.Tunnel)
	var out []map[string]any
	out = append(out, smart(in, tun, nil))
	if len(hops) > 0 {
		out = append(out, smart(in, tun, hops))
	}
	for _, n := range in.Nodes {
		for _, ex := range n.Exits() {
			base := n.Name + " · " + ex
			c := config(base+" · прямо", []map[string]any{exitOutbound(n, ex, "proxy", "")}, nil)
			out = append(out, finish(c, "direct", "proxy"))
			for _, h := range hops {
				c := config(base+exits.ChainSep+h.Name,
					[]map[string]any{exitOutbound(n, ex, "proxy", "hop"), hopOutbound(h, "hop")}, nil)
				out = append(out, finish(c, "hop", "proxy"))
			}
		}
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return nil, fmt.Errorf("happsub: подписка не сериализуется: %w", err)
	}
	return b, nil
}

// usableHops — хопы, которые можно отдать отдельно от их записи
// провайдера. Outbound, сам цепляющийся через другой (dialerProxy или
// proxySettings), без своей записи не работает — такой пропускаем.
func usableHops(in []Hop) []Hop {
	var out []Hop
	for _, h := range in {
		if h.Outbound == nil {
			continue
		}
		if _, ok := h.Outbound["proxySettings"]; ok {
			continue
		}
		if ss, ok := h.Outbound["streamSettings"].(map[string]any); ok {
			if so, ok := ss["sockopt"].(map[string]any); ok && so["dialerProxy"] != nil {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

// smart — «Умный маршрут». hops == nil — выходы напрямую. Первым идёт
// узел туннеля: его Happ пингует как «сам узел».
func smart(in Input, tun, hops []Hop) map[string]any {
	name := smartDirect
	var obs []map[string]any
	var balancers []map[string]any
	var selectors []string

	for i, h := range tun {
		obs = append(obs, hopOutbound(h, "tun-"+strconv.Itoa(i)))
	}
	if len(tun) > 0 {
		balancers = append(balancers, balancer("tunnel", "tun-"))
		selectors = append(selectors, "tun-")
	}
	for i, h := range hops {
		obs = append(obs, hopOutbound(h, "hop-"+strconv.Itoa(i)))
	}

	exitSet := map[string]bool{}
	var exitNames []string
	for _, n := range in.Nodes {
		for _, ex := range n.Exits() {
			if !exitSet[ex] {
				exitSet[ex] = true
				exitNames = append(exitNames, ex)
			}
		}
	}
	sortStrings(exitNames)
	for _, ex := range exitNames {
		prefix := "exit-" + ex + "-"
		if hops != nil {
			name = smartViaHop
			prefix = "chain-" + ex + "-"
		}
		for _, n := range in.Nodes {
			if _, ok := n.Users[ex]; !ok {
				continue
			}
			if hops == nil {
				obs = append(obs, exitOutbound(n, ex, prefix+n.Name, ""))
				continue
			}
			for i := range hops {
				obs = append(obs, exitOutbound(n, ex, prefix+n.Name+"-h"+strconv.Itoa(i), "hop-"+strconv.Itoa(i)))
			}
		}
		balancers = append(balancers, balancer(ex, prefix))
		selectors = append(selectors, prefix)
	}

	tunnel := "block"
	if len(tun) > 0 {
		tunnel = "balancer:tunnel"
	}
	target := func(a mixin.RuleAction, group string) string {
		switch a {
		case mixin.ActionDirect:
			return "direct"
		case mixin.ActionExit:
			// EXIT-<X> → выход «x» всех нод. Нет такого выхода — fail
			// closed, как REJECT в группе на роутере.
			ex := strings.ToLower(strings.TrimPrefix(group, mixin.ExitGroupPrefix))
			if exitSet[ex] {
				return "balancer:" + ex
			}
			return "block"
		default:
			return tunnel
		}
	}

	var rules []map[string]any
	for _, r := range in.Rules.Rules {
		switch r.Kind {
		case mixin.RuleSuffix:
			rules = append(rules, rule("domain", "domain:"+r.Value, target(r.Action, r.Group)))
		case mixin.RuleDomain:
			rules = append(rules, rule("domain", "full:"+r.Value, target(r.Action, r.Group)))
		case mixin.RuleCIDR:
			rules = append(rules, rule("ip", r.Value, target(r.Action, r.Group)))
		}
	}
	for _, s := range in.Rules.Sets {
		t := target(s.Action, s.Group)
		rules = append(rules, rule("domain", "geosite:"+s.Name, t))
		if s.IP {
			rules = append(rules, rule("ip", "geoip:"+s.Name, t))
		}
	}

	final := "direct"
	if in.Rules.Policy == mixin.PolicyTunnel {
		final = tunnel
	}
	dns := "direct"
	if len(tun) > 0 {
		dns = "balancer:tunnel"
	}
	c := config(name, obs, rules)
	r := c["routing"].(map[string]any)
	if len(balancers) > 0 {
		r["balancers"] = balancers
		c["observatory"] = map[string]any{
			"subjectSelector": selectors,
			"probeURL":        probeURL,
			"probeInterval":   probeInterval,
			// Параллельно: иначе Xray пробует по одному outbound с паузой
			// probeInterval между каждым, и балансировщики минутами пусты.
			"enableConcurrency": true,
		}
	}
	return finish(c, dns, final)
}

func balancer(tag, prefix string) map[string]any {
	// fallbackTag — block: все цепочки мертвы — отказ, а не утечка.
	return map[string]any{"tag": tag, "selector": []string{prefix},
		"strategy": map[string]any{"type": "leastPing"}, "fallbackTag": "block"}
}

func rule(key, val, target string) map[string]any {
	r := map[string]any{"type": "field", key: []string{val}}
	setTarget(r, target)
	return r
}

func setTarget(r map[string]any, target string) {
	if b, ok := strings.CutPrefix(target, "balancer:"); ok {
		r["balancerTag"] = b
	} else {
		r["outboundTag"] = target
	}
}

// config — скелет записи: inbounds и DNS как у провайдера.
func config(remarks string, obs []map[string]any, rules []map[string]any) map[string]any {
	obs = append(obs,
		map[string]any{"tag": "direct", "protocol": "freedom"},
		map[string]any{"tag": "block", "protocol": "blackhole"})
	return map[string]any{
		"remarks": remarks,
		"log":     map[string]any{"loglevel": "warning"},
		"dns":     map[string]any{"servers": dohServers, "queryStrategy": "UseIP"},
		"inbounds": []map[string]any{
			{"tag": "socks", "listen": "127.0.0.1", "port": 10808, "protocol": "socks",
				"settings": map[string]any{"udp": true},
				"sniffing": map[string]any{"enabled": true, "destOverride": []string{"http", "tls"}, "routeOnly": false}},
			{"tag": "http", "listen": "127.0.0.1", "port": 10809, "protocol": "http"},
		},
		"outbounds": obs,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rules},
	}
}

// finish дописывает правила по краям: DoH первым, затем UDP 443, частные
// сети и BitTorrent — как на ноде и у провайдера, — затем правила записи,
// последним — хвост.
func finish(c map[string]any, dns, final string) map[string]any {
	r := c["routing"].(map[string]any)
	body, _ := r["rules"].([]map[string]any)
	head := []map[string]any{
		{"type": "field", "ip": dohIPs, "port": "443"},
		{"type": "field", "network": "udp", "port": "443", "outboundTag": "block"},
		{"type": "field", "ip": []string{"geoip:private"}, "outboundTag": "direct"},
		{"type": "field", "protocol": []string{"bittorrent"}, "outboundTag": "block"},
	}
	setTarget(head[0], dns)
	tail := map[string]any{"type": "field", "network": "tcp,udp"}
	setTarget(tail, final)
	r["rules"] = append(append(head, body...), tail)
	return c
}

// exitOutbound — своя нода как outbound Xray (VLESS + Reality + vision).
func exitOutbound(n exits.Node, exit, tag, dialer string) map[string]any {
	ss := map[string]any{"network": "tcp", "security": "reality", "realitySettings": map[string]any{
		"serverName": n.SNI, "fingerprint": n.Fingerprint, "publicKey": n.PublicKey,
		"shortId": n.ShortID, "spiderX": "/"}}
	if dialer != "" {
		ss["sockopt"] = map[string]any{"dialerProxy": dialer}
	}
	return map[string]any{
		"tag":      tag,
		"protocol": "vless",
		"settings": map[string]any{"vnext": []any{map[string]any{"address": n.Address, "port": n.Port,
			"users": []any{map[string]any{"id": n.Users[exit], "encryption": "none", "flow": n.Flow}}}}},
		"streamSettings": ss,
	}
}

// hopOutbound — копия outbound провайдера с нашим тегом. Копия глубокая:
// исходник читают и другие записи.
func hopOutbound(h Hop, tag string) map[string]any {
	b, _ := json.Marshal(h.Outbound)
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	c["tag"] = tag
	return c
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
