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

// RenderProvider — тело файла провайдера exits для mihomo: для каждой
// ноды и выхода прямой узел «<нода>-<выход>» и по цепочке на каждый хоп
// «<нода>-<выход> ⇢ <хоп>» с dialer-proxy на группу-однохопник.
//
// JSON, а не YAML, — по той же причине, что happ.Render: YAML 1.2 —
// надмножество JSON, а stdlib YAML не умеет.
//
// Порядок стабильный: ноды по имени (Load), выходы по имени, хопы в
// порядке пула. От порядка зависит, что панель покажет первым.
func RenderProvider(nodes []Node, hops []string) []byte {
	proxies := make([]map[string]any, 0, len(nodes)*2*(1+len(hops)))
	for _, n := range nodes {
		for _, exit := range n.Exits() {
			base := n.Name + "-" + exit
			proxies = append(proxies, proxy(n, exit, base, ""))
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
		"name":               name,
		"type":               "vless",
		"server":             n.Address,
		"port":               n.Port,
		"uuid":               n.Users[exit],
		"network":            "tcp",
		"udp":                false,
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
