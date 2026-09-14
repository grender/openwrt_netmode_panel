package watch

import (
	"net"
	"sort"
	"strings"
)

// Список устройств LAN.
//
// Три источника, и ни один не требует новых пакетов OpenWrt
// (docs/recon/nikki-watch.md, «Устройства в LAN»):
//
//  1. /tmp/dhcp.leases — имя и адрес; имени нет у большинства устройств;
//  2. ubus call iwinfo assoclist домашней точки — кто подключён по Wi-Fi;
//  3. снимок /connections — кто говорит прямо сейчас.
//
// Кабель от Wi-Fi отличается вычитанием, и вычитания МАЛО: есть в аренде и
// нет в assoclist — это не Wi-Fi этой точки ПРЯМО СЕЙЧАС, но аренда живёт
// часами дольше ассоциации. Поэтому «кабель» ставится только тому, кто при
// этом говорит: говорит и не через точку — значит по проводу. Молчащий
// остаётся «неизвестно». Устройство без аренды (статика, виртуалка за
// relayd) попадает в список из снимка, без имени.

// Вид подключения.
//
// Беспроводное называется wireless, а не wifi, и это не вкус: литерал
// "wifi" в Go запрещён гейтом scripts/check-wireless-write.sh (R5), потому
// что команда `wifi` целиком ИЗМЕРЕННО роняет домашнюю точку (ADR-0025), и
// гейт ловит её во всех написаниях сразу. Пробивать в нём дыру ради
// значения перечисления — плохой размен: правило стережёт настоящий отказ,
// а слово в контракте ничего не стоит.
const (
	KindWireless = "wireless"
	KindWired    = "wired"
	KindUnknown  = "unknown"
)

// Lease — одна аренда DHCP.
type Lease struct {
	MAC, IP, Name string
}

// Host — строка списка устройств.
type Host struct {
	IP   string `json:"ip"`
	MAC  string `json:"mac"`
	Name string `json:"name"`
	// Kind — wifi | wired | unknown. «Неизвестно» и «нет» — разные вещи:
	// unknown означает, что точка доступа не ответила или аренды нет вовсе,
	// а не что устройство подключено как-то особенно.
	Kind string `json:"kind"`
	// Active — говорит прямо сейчас, по снимку соединений.
	Active bool `json:"active"`
}

// ParseLeases разбирает /tmp/dhcp.leases.
//
// Строка: «истечение MAC IPv4 имя client-id». Имя «*» означает, что клиент
// его не прислал, — это пустое имя, а не имя из одной звёздочки.
func ParseLeases(b []byte) []Lease {
	var out []Lease
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		ip := net.ParseIP(f[2])
		if ip == nil || ip.To4() == nil {
			continue
		}
		name := f[3]
		if name == "*" {
			name = ""
		}
		out = append(out, Lease{MAC: strings.ToLower(f[1]), IP: f[2], Name: name})
	}
	return out
}

// MergeHosts склеивает три источника в один список.
//
// wifiMACs == nil означает «точка доступа не ответила» и отличается от
// пустого списка: во втором случае она ответила, и по воздуху никого нет.
//
// MAC сравниваются без учёта регистра: assoclist пишет их прописными, а
// dnsmasq строчными, и прямое сравнение развело бы Wi-Fi и кабель наугад.
func MergeHosts(leases []Lease, wifiMACs []string, activeIPs []string) []Host {
	wifi := make(map[string]bool, len(wifiMACs))
	for _, m := range wifiMACs {
		wifi[strings.ToLower(m)] = true
	}
	active := make(map[string]bool, len(activeIPs))
	for _, ip := range activeIPs {
		active[ip] = true
	}

	byIP := make(map[string]*Host, len(leases)+len(activeIPs))
	var order []string
	add := func(h Host) *Host {
		if p, ok := byIP[h.IP]; ok {
			return p
		}
		p := &h
		byIP[h.IP] = p
		order = append(order, h.IP)
		return p
	}

	for _, l := range leases {
		kind := KindUnknown
		switch {
		case wifiMACs == nil:
			// Точка доступа не ответила: про способ подключения не известно
			// НИЧЕГО. Записать всех в кабель значило бы соврать про каждое
			// устройство сразу.
		case wifi[l.MAC]:
			kind = KindWireless
		case active[l.IP]:
			// В аренде, НЕ в assoclist и при этом говорит прямо сейчас —
			// значит говорит не через нашу точку, то есть по проводу.
			kind = KindWired
		default:
			// В аренде, не в assoclist и молчит. Это может быть ПК с
			// выдернутым кабелем, а может быть телефон, ушедший из дома:
			// аренда живёт часами дольше ассоциации. Вычитание здесь не
			// различает ничего, и «кабель» у телефона — ровно та ложь,
			// которую нашли на живом роутере 2026-09-14.
		}
		h := add(Host{IP: l.IP, MAC: l.MAC, Name: l.Name, Kind: kind})
		h.Active = active[l.IP]
	}

	for _, ip := range activeIPs {
		h := add(Host{IP: ip, Kind: KindUnknown})
		h.Active = true
	}

	out := make([]Host, 0, len(order))
	for _, ip := range order {
		out = append(out, *byIP[ip])
	}
	sort.Slice(out, func(i, j int) bool { return less4(out[i].IP, out[j].IP) })
	return out
}

// less4 — порядок по числовому значению адреса, а не по строке: иначе
// 192.168.9.9 встал бы после 192.168.9.124.
func less4(a, b string) bool {
	x, y := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if x == nil || y == nil {
		return a < b
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}
