package watch

import (
	"strings"
	"testing"
)

// TestParseLeasesFromRaw91: аренды разбираются из записанного файла dnsmasq.
//
// Четыре аренды из семи — без имени: клиент его не прислал, и dnsmasq пишет
// звёздочку. Это пустое имя, а не имя из одной звёздочки, и панель показывает
// такое устройство по адресу.
func TestParseLeasesFromRaw91(t *testing.T) {
	raw := strings.Join(rawSection(t, "cat /tmp/dhcp.leases"), "\n")
	got := ParseLeases([]byte(raw))
	if len(got) != 7 {
		t.Fatalf("аренд %d, в фикстуре 7", len(got))
	}
	var unnamed int
	for _, l := range got {
		if l.Name == "" {
			unnamed++
		}
		if l.MAC != strings.ToLower(l.MAC) {
			t.Errorf("MAC %q не приведён к строчным", l.MAC)
		}
		if l.IP == "" {
			t.Errorf("аренда без адреса: %+v", l)
		}
	}
	if unnamed != 4 {
		t.Errorf("безымянных аренд %d, в фикстуре 4", unnamed)
	}
	if got[0].Name != "neural" || got[0].IP != "192.168.9.207" {
		t.Errorf("первая аренда = %+v", got[0])
	}
}

// TestParseLeasesIgnoresGarbage: обрезанный файл не роняет список.
//
// /tmp/dhcp.leases лежит в tmpfs и переписывается dnsmasq на живую: прочитать
// его в момент записи — обычное дело, и половина строки не повод показать
// владельцу пустой список устройств.
func TestParseLeasesIgnoresGarbage(t *testing.T) {
	in := "1789370216 02:00:00:00:00:01 192.168.9.207 neural 01:02\n" +
		"мусор\n" +
		"1789370216 02:00:00:00:00:02 нетадреса имя id\n" +
		"1789370216 02:00:00:00:00:03\n" +
		"1789370216 02:00:00:00:00:04 192.168.9.163 * 01:02\n"
	got := ParseLeases([]byte(in))
	if len(got) != 2 {
		t.Fatalf("разобрано %d аренд, ожидалось 2: %+v", len(got), got)
	}
	if got[1].Name != "" {
		t.Errorf("звёздочка не стала пустым именем: %q", got[1].Name)
	}
}

// wifiMACsFromRaw достаёт MAC из записанного assoclist. В фикстуре он прошёл
// через grep, поэтому строки вида «"mac": "…",».
func wifiMACsFromRaw(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, l := range rawSection(t, "ubus call iwinfo assoclist") {
		i := strings.Index(l, `"mac":`)
		if i < 0 {
			continue
		}
		rest := l[i+len(`"mac":`):]
		a := strings.Index(rest, `"`)
		b := strings.LastIndex(rest, `"`)
		if a < 0 || b <= a {
			continue
		}
		out = append(out, rest[a+1:b])
	}
	return out
}

// TestMergeHostsFromRaw91: кабель от Wi-Fi отличается вычитанием.
//
// Пять MAC из семи аренд есть в assoclist — это Wi-Fi. ПК neural в
// LAN-порту и одна пережившая устройство аренда остаются кабелем: их в
// assoclist нет, и другого способа их различить у роутера нет.
func TestMergeHostsFromRaw91(t *testing.T) {
	leases := ParseLeases([]byte(strings.Join(rawSection(t, "cat /tmp/dhcp.leases"), "\n")))
	wifi := wifiMACsFromRaw(t)
	if len(wifi) != 5 {
		t.Fatalf("в фикстуре %d MAC Wi-Fi, ожидалось 5", len(wifi))
	}
	// Регистр разный намеренно: assoclist пишет прописными, dnsmasq —
	// строчными, и прямое сравнение развело бы Wi-Fi и кабель наугад.
	for i := range wifi {
		wifi[i] = strings.ToUpper(wifi[i])
	}

	hosts := MergeHosts(leases, wifi, []string{"192.168.9.219", "192.168.9.99"})
	byIP := map[string]Host{}
	for _, h := range hosts {
		byIP[h.IP] = h
	}
	if len(hosts) != 8 {
		t.Fatalf("устройств %d, ожидалось 8 (семь аренд плюс говорящий без аренды)", len(hosts))
	}
	if got := byIP["192.168.9.207"]; got.Kind != KindWired || got.Name != "neural" {
		t.Errorf("neural = %+v, ожидался кабель", got)
	}
	if got := byIP["192.168.9.219"]; got.Kind != KindWireless || !got.Active || got.Name != "" {
		t.Errorf("192.168.9.219 = %+v, ожидался Wi-Fi, говорит, без имени", got)
	}
	if got := byIP["192.168.9.99"]; got.Kind != KindUnknown || !got.Active {
		t.Errorf("адрес без аренды = %+v, ожидался unknown и «говорит»", got)
	}
	// Порядок числовой: иначе .99 встал бы между .219 и .226.
	prev := ""
	for _, h := range hosts {
		if prev != "" && !less4(prev, h.IP) {
			t.Fatalf("порядок не числовой: %s после %s", h.IP, prev)
		}
		prev = h.IP
	}
}

// TestMergeHostsWithoutAssoclist: точка доступа не ответила — вид связи
// НЕИЗВЕСТЕН, а не «кабель».
//
// Записать всех в кабель значило бы соврать про каждое устройство сразу, и
// соврать правдоподобно: строка «кабель» у телефона не выглядит ошибкой.
func TestMergeHostsWithoutAssoclist(t *testing.T) {
	leases := []Lease{{MAC: "aa:bb:cc:dd:ee:01", IP: "192.168.9.10", Name: "x"}}
	hosts := MergeHosts(leases, nil, nil)
	if len(hosts) != 1 || hosts[0].Kind != KindUnknown {
		t.Fatalf("без assoclist получилось %+v", hosts)
	}
}
