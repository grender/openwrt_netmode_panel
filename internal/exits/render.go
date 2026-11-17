package exits

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
)

// SubProvider — провайдер узлов подписки в профиле (контракт с демоном,
// files/etc/nikki/profiles/netmoded.yaml). Из него группы-однохопники
// берут свой хоп.
const SubProvider = "sub"

// ChainSep — разделитель в имени цепочки: «<нода>-<выход> ⇢ <хоп>».
// Профиль владельца фильтрует по нему цепочки выхода (`-dc ⇢ `).
const ChainSep = " ⇢ "

// HopGroupName — имя группы-однохопника для узла подписки.
//
// От имени хопа, а не от позиции в пуле: группы едут в миксине и
// применяются перезапуском движка, цепочки — в файле провайдера и
// применяются перечиткой. Между этими двумя моментами файлы живут в разных
// версиях, и позиционное имя увело бы цепочку через чужой хоп. С именем от
// хопа ссылка либо попадает в свой хоп, либо не находит группу — и mihomo
// отказывает соединению, а не выпускает его другим путём
// (docs/recon/exit-chains.md).
func HopGroupName(hop string) string {
	sum := sha256.Sum256([]byte(hop))
	return "nm-hop-" + hex.EncodeToString(sum[:4])
}

// HopGroups — группы-однохопники для nikki-proxy-groups миксина: по одной
// на хоп, `select` ровно из одного узла провайдера sub.
//
// Нужны потому, что dialer-proxy в mihomo разрешается только в узел или
// группу верхнего уровня — узел провайдера по имени он не находит, а
// mihomo -t этого не замечает (docs/recon/exit-chains.md).
func HopGroups(hops []string) []map[string]any {
	out := make([]map[string]any, 0, len(hops))
	for _, h := range hops {
		out = append(out, map[string]any{
			"name":   HopGroupName(h),
			"type":   "select",
			"use":    []string{SubProvider},
			"filter": "^" + regexp.QuoteMeta(h) + "$",
			// Хоп пропал из подписки — группа пуста. Пустая группа у mihomo —
			// COMPATIBLE, то есть DIRECT: цепочка молча пошла бы на ноду без
			// хопа. Kill switch требует отказа (docs/recon/exit-chains.md).
			"empty-fallback": "REJECT",
		})
	}
	return out
}

// MeasuredExit — выход, по чьим цепочкам выбирается хоп. dc выходит с IP
// самого VPS (freedom), и проверка цепочки через него дешёвая; проверка
// resi — новое соединение через ISP-прокси, который отказывает на частых
// соединениях (docs/recon/exit-chains.md). Участок «хоп → VPS» у выходов
// общий, поэтому хоп, годный для dc, годен и для остальных.
const MeasuredExit = "dc"

// AutoHop — хвост имени цепочки, хоп которой выбирает демон: «<нода>-resi ⇢ авто».
const AutoHop = "авто"

// FollowGroupName — группа-повторитель ноды: select из групп-однохопников
// пула и REJECT; демон держит в ней хоп с лучшим замером цепочки dc этой
// ноды (internal/httpapi/hopsync.go). Имя — от ноды, по той же причине,
// что HopGroupName.
func FollowGroupName(node string) string {
	sum := sha256.Sum256([]byte(node))
	return "nm-follow-" + hex.EncodeToString(sum[:4])
}

// follows — нужен ли ноде повторитель: есть измеряемый выход и есть что
// повторять.
func follows(n Node) bool {
	_, ok := n.Users[MeasuredExit]
	return ok && len(n.Users) > 1
}

// FollowNodes — ноды, которым нужен повторитель, в порядке нод.
func FollowNodes(nodes []Node) []string {
	var out []string
	for _, n := range nodes {
		if follows(n) {
			out = append(out, n.Name)
		}
	}
	return out
}

// FollowGroups — повторители для nikki-proxy-groups миксина. REJECT
// последним и всегда: без пула повторитель — [REJECT], и цепочка «⇢ авто»
// отказывает, а не идёт мимо (kill switch). DIRECT здесь не бывает.
func FollowGroups(nodes []Node, hops []string) []map[string]any {
	var out []map[string]any
	for _, name := range FollowNodes(nodes) {
		out = append(out, FollowGroup(name, hops))
	}
	return out
}

// FollowGroup — повторитель одной ноды.
func FollowGroup(node string, hops []string) map[string]any {
	members := make([]string, 0, len(hops)+1)
	for _, h := range hops {
		members = append(members, HopGroupName(h))
	}
	members = append(members, "REJECT")
	return map[string]any{"name": FollowGroupName(node), "type": "select", "proxies": members}
}

// RenderProvider — тело файла провайдера exits для mihomo. Для каждой
// ноды и выхода — прямой узел «<нода>-<выход>». Цепочки:
//   - измеряемый выход (dc) — по одной на каждый хоп, «<нода>-dc ⇢ <хоп>»
//     с dialer-proxy на группу-однохопник: их меряет url-test профиля;
//   - остальные выходы ноды с dc — одна «<нода>-<выход> ⇢ авто» через
//     повторитель, всегда, даже без пула;
//   - у ноды без dc мерить нечем — цепочки на каждый хоп у всех выходов.
//
// JSON, а не YAML, — по той же причине, что happ.Render: YAML 1.2 —
// надмножество JSON, а stdlib YAML не умеет.
//
// Порядок стабильный: ноды по имени (Load), выходы по имени, хопы в
// порядке пула. От порядка зависит, что панель покажет первым.
func RenderProvider(nodes []Node, hops []string) []byte {
	proxies := make([]map[string]any, 0, len(nodes)*2*(1+len(hops)))
	for _, n := range nodes {
		follow := follows(n)
		for _, exit := range n.Exits() {
			base := n.Name + "-" + exit
			proxies = append(proxies, proxy(n, exit, base, ""))
			if follow && exit != MeasuredExit {
				proxies = append(proxies, proxy(n, exit, base+ChainSep+AutoHop, FollowGroupName(n.Name)))
				continue
			}
			for _, h := range hops {
				proxies = append(proxies, proxy(n, exit, base+ChainSep+h, HopGroupName(h)))
			}
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Proxies []map[string]any `json:"proxies"`
	}{proxies}); err != nil {
		// Недостижимо: внутри только строки, числа, булевы и карты.
		panic("exits: узел не сериализуется: " + err.Error())
	}
	return buf.Bytes()
}

// proxy — узел mihomo, как в templates/mihomo-proxies.yaml.j2 репо нод.
func proxy(n Node, exit, name, dialer string) map[string]any {
	p := map[string]any{
		"name":    name,
		"type":    "vless",
		"server":  n.Address,
		"port":    n.Port,
		"uuid":    n.Users[exit],
		"network": "tcp",
		// true — ради kill switch, а не ради UDP: правило, чей узел не умеет
		// UDP, mihomo не отклоняет, а пропускает (tunnel.go, match), и QUIC к
		// домену из набора уходил бы в хвост MATCH,DIRECT — с домашнего IP.
		// С true пакет доходит до ноды, а там UDP 443 и весь UDP resi режутся.
		"udp":                true,
		"tls":                true,
		"flow":               n.Flow,
		"servername":         n.SNI,
		"client-fingerprint": n.Fingerprint,
		"reality-opts": map[string]any{
			"public-key": n.PublicKey,
			"short-id":   n.ShortID,
			// Без него mihomo получает «REALITY authentication failed»,
			// если сайт маскировки поддерживает X25519MLKEM768.
			"support-x25519mlkem768": true,
		},
	}
	if dialer != "" {
		p["dialer-proxy"] = dialer
	}
	return p
}
