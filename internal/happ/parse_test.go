package happ

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// subscription читает снятую подписку. Фикстура — ответ провайдера на
// User-Agent Happ, 30 записей, секреты заменены с сохранением ФОРМЫ:
// на разбор влияет форма значения, а не его содержимое.
func subscription(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "subscription.json"))
	if err != nil {
		t.Fatalf("фикстура подписки не читается: %v", err)
	}
	return raw
}

func TestParseKeepsProviderOrder(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(entries) != 30 {
		t.Fatalf("записей %d, ожидалось 30", len(entries))
	}

	// Порядок — главная ценность разбора: Clash API отдаёт узлы объектом
	// и восстановить авторскую раскладку потом неоткуда.
	if got, want := entries[0].Name, "🇪🇺 🚀Авто | Лучший сервер ⚡⚡"; got != want {
		t.Errorf("первая запись %q, ожидалась %q", got, want)
	}
	if entries[0].Kind != KindAuto {
		t.Errorf("вид первой записи %q, ожидался %q", entries[0].Kind, KindAuto)
	}
	if got, want := entries[24].Name, "⬇️ Обходы белых списков ⬇️"; got != want {
		t.Errorf("запись 25 %q, ожидался разделитель %q", got, want)
	}
	if got, want := entries[29].Name, "🇨🇭⚪Швейцария (БС-2)☁️"; got != want {
		t.Errorf("последняя запись %q, ожидалась %q", got, want)
	}
}

// TestParseKindLayout закрепляет раскладку видов на снятой подписке —
// в обоих положениях выключателя xhttp.
//
// Числа выведены из состава фикстуры: 1 «Авто», 13 vless/tcp/reality,
// 7 vless/xhttp/reality, 3 hysteria2, 6 записей «обхода» (из них одна —
// разделитель, у остальных пяти wl-узлы: 4 vless/xhttp и 1 hysteria2).
func TestParseKindLayout(t *testing.T) {
	raw := subscription(t)

	cases := []struct {
		name        string
		xhttp       bool
		nodes       int
		auto        int
		separators  int
		unsupported int
	}{
		// 13 tcp + 7 xhttp + 3 hysteria2 + 5 wl = 28 узлов.
		{"xhttp принят", true, 28, 1, 1, 0},
		// Гаснут 7 обычных xhttp-узлов и 4 wl-узла на xhttp = 11.
		{"xhttp погашен", false, 17, 1, 1, 11},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries, err := parse(raw, c.xhttp)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			counts := map[Kind]int{}
			for _, e := range entries {
				counts[e.Kind]++
			}
			if counts[KindNode] != c.nodes {
				t.Errorf("узлов %d, ожидалось %d", counts[KindNode], c.nodes)
			}
			if counts[KindAuto] != c.auto {
				t.Errorf("«Авто» %d, ожидалось %d", counts[KindAuto], c.auto)
			}
			if counts[KindSeparator] != c.separators {
				t.Errorf("разделителей %d, ожидалось %d", counts[KindSeparator], c.separators)
			}
			if counts[KindUnsupported] != c.unsupported {
				t.Errorf("неподдержанных %d, ожидалось %d", counts[KindUnsupported], c.unsupported)
			}

			// Summarize обязан считать то же самое.
			s := Summarize(entries)
			if s.Nodes != c.nodes || s.Separators != c.separators || s.Unsupported != c.unsupported {
				t.Errorf("Summarize вернул %+v, ожидалось nodes=%d separators=%d unsupported=%d",
					s, c.nodes, c.separators, c.unsupported)
			}

			// Вид разделителя не зависит от выключателя: выключатель
			// говорит про то, что примет движок, а не про то, чем
			// запись является.
			if entries[24].Kind != KindSeparator {
				t.Errorf("запись 25 имеет вид %q, ожидался %q", entries[24].Kind, KindSeparator)
			}
			// У неподдержанных обязана быть причина — иначе панель
			// покажет строку без объяснения.
			for i, e := range entries {
				if e.Kind == KindUnsupported && e.Reason == "" {
					t.Errorf("запись %d (%q) неподдержана без причины", i, e.Name)
				}
				if e.Kind == KindNode && e.Proxy == nil {
					t.Errorf("запись %d (%q) — узел без объекта", i, e.Name)
				}
				if e.Kind != KindNode && e.Proxy != nil {
					t.Errorf("запись %d (%q) не узел, но несёт объект", i, e.Name)
				}
			}
		})
	}
}

// TestParsePicksWhitelistOutbound проверяет, что у записи «обхода» узлом
// становится wl-outbound, а не decoy.
//
// Decoy — копия узла, который в списке УЖЕ есть отдельной строкой; взяв
// его, мы завели бы дубликат и потеряли единственный сервер, ради которого
// запись существует.
func TestParsePicksWhitelistOutbound(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// «🇨🇭⚪Швейцария (БС-1)☁️»: decoy на 203.0.113.14, wl на 203.0.113.15.
	e := entries[28]
	if e.Kind != KindNode {
		t.Fatalf("запись 29 имеет вид %q, ожидался узел", e.Kind)
	}
	if got, want := e.Proxy["server"], "203.0.113.15"; got != want {
		t.Errorf("сервер %v, ожидался wl-адрес %v (а не decoy 203.0.113.14)", got, want)
	}
}

// TestSeparatorNeedsTwin — вторая ступень правила разделителя.
//
// Это главный тест раздела. Первая ступень (имя без флага) отбирает
// кандидатов, но одной её мало: провайдер может завести НАСТОЯЩИЙ узел с
// именем без флага в любой день, и спрятать его под видом заголовка значило
// бы молча отнять у владельца работающий сервер. Поэтому кандидат без
// двойника остаётся узлом.
func TestSeparatorNeedsTwin(t *testing.T) {
	node := func(name, server string) string {
		return fmt.Sprintf(`{
			"remarks": %q,
			"outbounds": [{
				"tag": "proxy",
				"protocol": "vless",
				"settings": {"vnext": [{"address": %q, "port": 443,
					"users": [{"id": "11111111-2222-4333-8444-555555555555", "flow": "xtls-rprx-vision"}]}]},
				"streamSettings": {"network": "tcp", "security": "reality",
					"realitySettings": {"serverName": "example.com", "publicKey": "PK", "shortId": "aa", "fingerprint": "firefox"}}
			}]
		}`, name, server)
	}

	t.Run("копия соседа становится разделителем", func(t *testing.T) {
		raw := []byte("[" +
			node("🇩🇪Германия", "203.0.113.1") + "," +
			node("⬇️ Заголовок ⬇️", "203.0.113.1") + "]")

		entries, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if entries[1].Kind != KindSeparator {
			t.Errorf("вид %q, ожидался %q", entries[1].Kind, KindSeparator)
		}
		if entries[1].Proxy != nil || entries[1].Type != "" {
			t.Errorf("разделитель не должен нести узел: %+v", entries[1])
		}
		// Оригинал остаётся узлом: разделителем становится только
		// кандидат без флага, а не обе половины пары.
		if entries[0].Kind != KindNode {
			t.Errorf("оригинал получил вид %q, ожидался узел", entries[0].Kind)
		}
	})

	t.Run("уникальный узел без флага остаётся узлом", func(t *testing.T) {
		raw := []byte("[" +
			node("🇩🇪Германия", "203.0.113.1") + "," +
			node("Резервный сервер", "203.0.113.2") + "]")

		entries, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if entries[1].Kind != KindNode {
			t.Fatalf("вид %q, ожидался %q: узел без флага, но со своим сервером, "+
				"прятать нельзя", entries[1].Kind, KindNode)
		}
		if got := entries[1].Proxy["server"]; got != "203.0.113.2" {
			t.Errorf("сервер %v, ожидался 203.0.113.2", got)
		}
	})

	t.Run("двойник ищется и до записи, и после", func(t *testing.T) {
		// В снятой подписке заголовок стоит 25-м, а близнец 28-м, то
		// есть ПОСЛЕ него: односторонний поиск заголовок пропустил бы.
		raw := []byte("[" +
			node("⬇️ Заголовок ⬇️", "203.0.113.1") + "," +
			node("🇩🇪Германия", "203.0.113.1") + "]")

		entries, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if entries[0].Kind != KindSeparator {
			t.Errorf("вид %q, ожидался %q", entries[0].Kind, KindSeparator)
		}
	})
}

// TestParseDedupesNames — имя у нас ключ и в манифесте, и в выборе узла
// через Clash API, а mihomo держит узлы объектом. Два одинаковых имени —
// это молча потерянный узел и выбор, попадающий не туда.
func TestParseDedupesNames(t *testing.T) {
	node := func(name, server string) string {
		return fmt.Sprintf(`{
			"remarks": %q,
			"outbounds": [{
				"tag": "proxy", "protocol": "hysteria",
				"settings": {"address": %q, "port": 8449, "version": 2},
				"streamSettings": {"network": "hysteria",
					"hysteriaSettings": {"version": 2, "auth": "secret"},
					"security": "tls", "tlsSettings": {"serverName": "sni.example.net", "alpn": ["h3"]}}
			}]
		}`, name, server)
	}

	raw := []byte("[" +
		node("🇩🇪Германия", "203.0.113.1") + "," +
		node("🇩🇪Германия", "203.0.113.2") + "," +
		node("🇩🇪Германия", "203.0.113.3") + "]")

	entries, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"🇩🇪Германия", "🇩🇪Германия (2)", "🇩🇪Германия (3)"}
	for i, w := range want {
		if entries[i].Name != w {
			t.Errorf("имя записи %d — %q, ожидалось %q", i, entries[i].Name, w)
		}
		// Имя внутри объекта узла обязано ехать за именем записи:
		// именно оно попадёт в файл провайдера.
		if got := entries[i].Proxy["name"]; got != w {
			t.Errorf("имя в объекте узла %d — %v, ожидалось %q", i, got, w)
		}
	}
}

func TestParseNamesAreUnique(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	seen := map[string]int{}
	for i, e := range entries {
		if prev, dup := seen[e.Name]; dup {
			t.Errorf("имя %q повторяется у записей %d и %d", e.Name, prev, i)
		}
		seen[e.Name] = i
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"битый JSON", `[{"remarks":`, ErrBadJSON},
		{"не массив", `{"error":"subscription expired"}`, ErrNotArray},
		{"строка вместо массива", `"nope"`, ErrNotArray},
		{"пустой массив", `[]`, ErrEmpty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.raw))
			if !errors.Is(err, c.want) {
				t.Fatalf("ошибка %v, ожидалась %v", err, c.want)
			}
		})
	}
}

// TestParseZeroNodesIsNotAnError: ноль узлов при непустом массиве —
// не отказ разбора, а факт про подписку. Вызывающий узнаёт о нём из
// Summary и показывает владельцу список с причинами, а не пустую панель.
func TestParseZeroNodesIsNotAnError(t *testing.T) {
	raw := []byte(`[{"remarks": "🇩🇪Германия", "outbounds": [
		{"tag": "proxy", "protocol": "vmess", "settings": {}, "streamSettings": {}}]}]`)

	entries, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s := Summarize(entries); s.Nodes != 0 || s.Unsupported != 1 {
		t.Fatalf("Summarize вернул %+v, ожидалось nodes=0 unsupported=1", s)
	}
	if entries[0].Reason == "" {
		t.Error("причина непригодности пуста — панели нечего показать")
	}
}

// TestParseSurvivesBrokenRecord: одна кривая запись не роняет остальные.
// Владельцу полезнее список с одной строкой-объяснением, чем пустая панель.
func TestParseSurvivesBrokenRecord(t *testing.T) {
	raw := []byte(`[
		{"remarks": "🇩🇪Германия", "outbounds": "не объект"},
		{"remarks": "🇭🇺Венгрия", "outbounds": [{
			"tag": "proxy", "protocol": "hysteria",
			"settings": {"address": "203.0.113.1", "port": 8449, "version": 2},
			"streamSettings": {"network": "hysteria",
				"hysteriaSettings": {"version": 2, "auth": "secret"},
				"security": "tls", "tlsSettings": {"serverName": "sni.example.net"}}}]}
	]`)

	entries, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("записей %d, ожидалось 2", len(entries))
	}
	if entries[0].Kind != KindUnsupported || entries[0].Name != "🇩🇪Германия" {
		t.Errorf("кривая запись разобрана как %+v", entries[0])
	}
	if entries[1].Kind != KindNode {
		t.Errorf("исправная запись получила вид %q", entries[1].Kind)
	}
}

func TestHasFlagPrefix(t *testing.T) {
	// Одиночный regional indicator рисуется буквой в рамке и флагом не
	// является: проверять только первую руну нельзя.
	cases := map[string]bool{
		"🇩🇪⚡Германия":                true,
		"🇪🇺 🚀Авто | Лучший ⚡⚡":       true,
		"⬇️ Обходы белых списков ⬇️": false,
		"Резервный сервер":           false,
		"":                           false,
		"\U0001F1E9Германия":         false, // одна руна, не пара
	}
	for name, want := range cases {
		if got := hasFlagPrefix(name); got != want {
			t.Errorf("hasFlagPrefix(%q) = %v, ожидалось %v", name, got, want)
		}
	}
}
