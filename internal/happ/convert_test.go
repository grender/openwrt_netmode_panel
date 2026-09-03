package happ

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// oracleFields — поля, по которым наш перевод обязан совпасть с
// Clash-профилем, собранным САМИМ провайдером.
//
// reality-opts.short-id в список НЕ входит, и это не послабление: сервер
// reality принимает несколько коротких идентификаторов, а какой из них
// положить в профиль, решает тот, кто профиль собирает. У 9 узлов из 12
// провайдер прислал в YAML не тот shortId, что в JSON, — законно с обеих
// сторон. У оставшихся трёх он совпал случайно, и опираться на это тоже
// нельзя.
var oracleFields = []string{
	"type", "server", "port", "uuid", "network", "flow", "tls",
	"servername", "client-fingerprint", "reality-opts.public-key",
	"password", "sni", "alpn",
}

// TestConvertMatchesProviderProfile — оракул.
//
// Тот же провайдер на User-Agent clash-verge/2.0 отдаёт готовый
// Clash-профиль на 15 узлов. Это не половина подписки, а другой продукт (в
// нём нет ни одного xhttp-узла, ни записей «обхода», ни разделителя, и
// порядок свой) — источником данных он не годится. Зато он идеален как
// эталон для конвертера: он написан НЕ НАМИ, и потому проверяет нашу
// догадку не саму против себя.
func TestConvertMatchesProviderProfile(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ours := map[string]map[string]string{}
	for _, e := range entries {
		if e.Kind == KindNode {
			ours[e.Name] = flatten(e.Proxy)
		}
	}

	oracle := readOracle(t)
	if len(oracle) != 15 {
		t.Fatalf("в эталоне %d узлов, ожидалось 15", len(oracle))
	}

	checked := 0
	for _, want := range oracle {
		name := want["name"]
		got, ok := ours[name]
		if !ok {
			t.Errorf("узла %q нет в нашем переводе", name)
			continue
		}
		checked++
		for _, f := range oracleFields {
			gv, gok := got[f]
			wv, wok := want[f]
			switch {
			case gok != wok:
				t.Errorf("%s: поле %s есть у %s и отсутствует у %s",
					name, f, present(wok, "эталона", "нас"), present(wok, "нас", "эталона"))
			case gok && gv != wv:
				t.Errorf("%s: поле %s = %q, у эталона %q", name, f, gv, wv)
			}
		}
		// Отдельно убеждаемся, что short-id мы всё же кладём: из
		// сравнения он исключён, но пропасть он не должен — без него
		// reality-узел не поднимется.
		if want["type"] == "vless" && got["reality-opts.short-id"] == "" && got["reality-opts.public-key"] != "" {
			t.Errorf("%s: reality-opts.short-id пуст", name)
		}
	}
	if checked != 15 {
		t.Fatalf("сверено %d узлов, ожидалось 15", checked)
	}
	t.Logf("сверено %d узлов по %d полям (short-id исключён осознанно)", checked, len(oracleFields))
}

func present(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// flatten приводит объект узла к плоской карте строк: вложенные карты — в
// точечные ключи, списки — в строку через запятую. Ровно та же форма, в
// которой читается эталон, поэтому сравнение симметрично.
func flatten(proxy map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range proxy {
		switch t := v.(type) {
		case map[string]any:
			for nk, nv := range t {
				out[k+"."+nk] = fmt.Sprint(nv)
			}
		case []string:
			out[k] = strings.Join(t, ",")
		default:
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// readOracle читает узлы из Clash-профиля провайдера.
//
// Разбор построчный и МИНИМАЛЬНЫЙ — ровно под форму этой фикстуры:
// список proxies с элементами «- name: …», плоские ключи с отступом 2,
// вложенные карты (*-opts) с отступом 4 и списки (alpn), у которых дефис
// стоит на том же отступе, что и ключ. Ни якорей, ни блочных скаляров, ни
// кавычек, ни потока — в фикстуре их нет.
//
// Полноценный YAML-парсер здесь не нужен и не может появиться: stdlib YAML
// не умеет, а зависимостей у проекта нет по устройству (scripts/check-stdlib.sh).
// Писать общий парсер ради одного тестового файла значило бы завести в
// тестах вторую подсистему, которую надо отлаживать и которая будет врать
// молча. Если фикстура однажды сменит форму — этот разбор упадёт заметно,
// на количестве узлов, и это правильное поведение.
func readOracle(t *testing.T) []map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "provider-clash.yaml"))
	if err != nil {
		t.Fatalf("эталонный профиль не читается: %v", err)
	}

	var (
		out       []map[string]string
		cur       map[string]string
		inProxies bool
		nested    string // ключ открытой вложенной карты или списка
	)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch {
		case line == "proxies:":
			inProxies = true
			continue
		case !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-"):
			// Любой другой ключ верхнего уровня закрывает список —
			// в фикстуре это proxy-groups и rules, и их узлы
			// («- name: PROXY») нам не нужны.
			inProxies = false
			continue
		}
		if !inProxies {
			continue
		}

		switch {
		case strings.HasPrefix(line, "- "):
			// Новый узел. Первым ключом всегда идёт name.
			cur = map[string]string{}
			nested = ""
			out = append(out, cur)
			k, v := splitKV(t, strings.TrimPrefix(line, "- "))
			cur[k] = v
		case strings.HasPrefix(line, "    "):
			// Вложенная карта: reality-opts.public-key и подобные.
			k, v := splitKV(t, strings.TrimPrefix(line, "    "))
			cur[nested+"."+k] = v
		case strings.HasPrefix(line, "  - "):
			// Элемент списка на отступе ключа: alpn.
			item := strings.TrimPrefix(line, "  - ")
			if prev, ok := cur[nested]; ok && prev != "" {
				cur[nested] = prev + "," + item
			} else {
				cur[nested] = item
			}
		case strings.HasPrefix(line, "  "):
			k, v := splitKV(t, strings.TrimPrefix(line, "  "))
			if v == "" {
				// Ключ без значения открывает вложенную карту
				// или список; что именно — покажет следующая
				// строка по отступу.
				nested = k
				continue
			}
			cur[k] = v
		default:
			t.Fatalf("эталон: строка не разбирается: %q", line)
		}
	}
	return out
}

func splitKV(t *testing.T, s string) (string, string) {
	t.Helper()
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		t.Fatalf("эталон: не пара ключ-значение: %q", s)
	}
	return strings.TrimSpace(k), strings.TrimSpace(v)
}

// xhttpOpts достаёт xhttp-opts узла по имени записи.
func xhttpOpts(t *testing.T, entries []Entry, name string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		if e.Kind != KindNode {
			t.Fatalf("%q имеет вид %q, ожидался узел (%s)", name, e.Kind, e.Reason)
		}
		opts, ok := e.Proxy["xhttp-opts"].(map[string]any)
		if !ok {
			t.Fatalf("%q: xhttp-opts отсутствуют: %+v", name, e.Proxy)
		}
		return opts
	}
	t.Fatalf("записи %q нет", name)
	return nil
}

func assertOpts(t *testing.T, opts map[string]any, want map[string]any) {
	t.Helper()
	for k, w := range want {
		got, ok := opts[k]
		if !ok {
			t.Errorf("ключ %s не доехал (есть: %v)", k, sortedKeys(opts))
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("%s = %v, ожидалось %v", k, got, w)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestConvertXHTTPPadding — САМЫЙ ВАЖНЫЙ ТЕСТ ЭТОГО ФАЙЛА.
//
// xPaddingBytes проверяется СЕРВЕРОМ на каждом запросе, а умолчание mihomo
// ("100-1000") с диапазоном провайдера ("50-150") пересекается лишь на
// 100–150. Потеряв это поле, мы получаем ~94% ответов 400 и узел, который
// «то работает, то нет». Тест стоит здесь, чтобы такую правку нельзя было
// внести как безобидную уборку тюнинга.
func TestConvertXHTTPPadding(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// «🇩🇪⚡Германия» — обычный xhttp + reality, паддинг 50-150.
	opts := xhttpOpts(t, entries, "🇩🇪⚡Германия")
	assertOpts(t, opts, map[string]any{
		"mode":            "stream-one",
		"path":            "/api/v2/stream/by-group-id/deadbeefcafe1234",
		"x-padding-bytes": "50-150",
	})
	if _, ok := opts["host"]; !ok {
		t.Error("host обязан присутствовать: он часть тройки mode/host/path")
	}

	// Ни один xhttp-узел подписки не должен остаться без паддинга: у всех
	// он задан явно, и молчаливая потеря хотя бы одного — тот самый отказ,
	// который ищут неделю.
	for _, e := range entries {
		if e.Kind != KindNode {
			continue
		}
		opts, ok := e.Proxy["xhttp-opts"].(map[string]any)
		if !ok {
			continue
		}
		if opts["x-padding-bytes"] == nil || opts["x-padding-bytes"] == "" {
			t.Errorf("%q: xhttp-узел без x-padding-bytes", e.Name)
		}
	}
}

// TestConvertXHTTPZeroPadding: "0-0" — это осмысленное «паддинга нет», а не
// пустое значение. Отфильтровав его как ноль, мы включили бы узлу чужое
// умолчание "100-1000", то есть 400 на каждом запросе.
func TestConvertXHTTPZeroPadding(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	opts := xhttpOpts(t, entries, "🇩🇪⚪Германия (БС-3)☁️")
	assertOpts(t, opts, map[string]any{
		"x-padding-bytes":    "0-0",
		"uplink-http-method": "GET",
	})
	// Пустые строки соседних ключей ключей не создают: отсутствие ключа и
	// пустая строка для mihomo разные вещи.
	for _, k := range []string{"x-padding-key", "x-padding-header", "session-key", "seq-key", "uplink-data-key"} {
		if _, ok := opts[k]; ok {
			t.Errorf("пустое значение создало ключ %s = %v", k, opts[k])
		}
	}
}

// TestConvertXHTTPObfuscation: у «Турции (БС-4)» сессия и счётчик уезжают в
// cookies, а паддинг — в query-параметр заголовка Referer. Умолчание обеих
// сторон — path, поэтому без переноса сервер не найдёт ни паддинга, ни
// сессии, и узел мёртв на все сто процентов.
func TestConvertXHTTPObfuscation(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	opts := xhttpOpts(t, entries, "🇹🇷⚪Турция (БС-4)☁️")

	assertOpts(t, opts, map[string]any{
		"x-padding-bytes":        "80-600",
		"x-padding-obfs-mode":    true,
		"x-padding-key":          "utm_content",
		"x-padding-header":       "Referer",
		"x-padding-placement":    "queryInHeader",
		"x-padding-method":       "tokenish",
		"session-placement":      "cookie",
		"session-key":            "_hjid",
		"seq-placement":          "cookie",
		"seq-key":                "_ts",
		"uplink-http-method":     "POST",
		"uplink-data-placement":  "body",
		"sc-max-each-post-bytes": "131072-900000",
		"no-grpc-header":         true,
	})

	// headers — вложенный объект, он обязан доехать целиком.
	headers, ok := opts["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers не доехали: %v", opts["headers"])
	}
	if headers["Sec-Fetch-Site"] != "same-origin" || headers["Accept"] != "*/*" {
		t.Errorf("headers доехали не целиком: %v", headers)
	}

	// Серверные параметры НЕ переносятся: у outbound-а mihomo аналога им
	// нет, и подстановка дала бы отказ разбора всего файла провайдера.
	for _, k := range []string{
		"scMaxBufferedPosts", "sc-max-buffered-posts",
		"scStreamUpServerSecs", "sc-stream-up-server-secs",
		"sessionIDTable", "session-id-table",
		"sessionIDLength", "session-id-length",
		"noSSEHeader", "no-sse-header",
	} {
		if _, ok := opts[k]; ok {
			t.Errorf("серверный параметр %s попал в xhttp-opts", k)
		}
	}
	// Ни одного ключа в camelCase: имена у mihomo kebab-case, и
	// непереименованный ключ он молча проигнорирует.
	for k := range opts {
		if strings.ToLower(k) != k {
			t.Errorf("ключ %q не переименован в kebab-case", k)
		}
	}
}

// TestConvertXHTTPReuseSettings: xmux у mihomo называется reuse-settings, и
// ключи внутри тоже переименованы.
func TestConvertXHTTPReuseSettings(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	opts := xhttpOpts(t, entries, "🇹🇷⚪Турция (БС-4)☁️")
	if _, ok := opts["xmux"]; ok {
		t.Error("xmux остался под именем Xray")
	}
	reuse, ok := opts["reuse-settings"].(map[string]any)
	if !ok {
		t.Fatalf("reuse-settings отсутствуют: %v", sortedKeys(opts))
	}
	assertOpts(t, reuse, map[string]any{
		"max-concurrency":     "16-32",
		"h-max-request-times": "600-900",
		"h-max-reusable-secs": "600-1800",
		"h-keep-alive-period": 20,
	})
	// maxConnections: 0 — это умолчание, а не настройка.
	if _, ok := reuse["max-connections"]; ok {
		t.Errorf("нулевое maxConnections создало ключ: %v", reuse["max-connections"])
	}

	// У «Швейцарии (БС-1)» cMaxReuseTimes задан строкой — он обязан
	// доехать, в отличие от нулевого у соседей.
	sw := xhttpOpts(t, entries, "🇨🇭⚪Швейцария (БС-1)☁️")
	swReuse, ok := sw["reuse-settings"].(map[string]any)
	if !ok {
		t.Fatalf("reuse-settings отсутствуют: %v", sortedKeys(sw))
	}
	assertOpts(t, swReuse, map[string]any{"c-max-reuse-times": "256-512"})

	// А у «Германии» он нулевой и ключа создавать не должен.
	de, ok := xhttpOpts(t, entries, "🇩🇪⚡Германия")["reuse-settings"].(map[string]any)
	if !ok {
		t.Fatal("reuse-settings у Германии отсутствуют")
	}
	if _, ok := de["c-max-reuse-times"]; ok {
		t.Errorf("нулевое cMaxReuseTimes создало ключ: %v", de["c-max-reuse-times"])
	}
}

// TestConvertXHTTPSessionSpelling: sessionIDPlacement/sessionIDKey — второе
// написание тех же полей. Побеждает первое непустое, и результат обязан быть
// одинаковым от запуска к запуску (обход карты в Go случаен).
func TestConvertXHTTPSessionSpelling(t *testing.T) {
	node := func(extra string) []byte {
		return []byte(`[{"remarks":"🇩🇪Тест","outbounds":[{
			"tag":"proxy","protocol":"vless",
			"settings":{"vnext":[{"address":"203.0.113.1","port":443,"users":[{"id":"u"}]}]},
			"streamSettings":{"network":"xhttp","xhttpSettings":{"mode":"packet-up","host":"","path":"/p","extra":` + extra + `},
			"security":"tls","tlsSettings":{"serverName":"s","fingerprint":"chrome"}}}]}]`)
	}

	cases := []struct{ name, extra, want string }{
		{"только sessionKey", `{"sessionKey":"_a"}`, "_a"},
		{"только sessionIDKey", `{"sessionIDKey":"_b"}`, "_b"},
		{"пустой sessionKey уступает", `{"sessionKey":"","sessionIDKey":"_b"}`, "_b"},
		{"оба непустых — побеждает без ID", `{"sessionKey":"_a","sessionIDKey":"_b"}`, "_a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Прогоняем несколько раз: случайный обход карты дал бы
			// разный ответ, и одного прогона могло бы не хватить.
			for i := 0; i < 20; i++ {
				entries, err := Parse(node(c.extra))
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				opts := xhttpOpts(t, entries, "🇩🇪Тест")
				if got := opts["session-key"]; got != c.want {
					t.Fatalf("session-key = %v, ожидалось %q", got, c.want)
				}
			}
		})
	}
}

// TestConvertXHTTPSwitch: при погашенном выключателе узел на xhttp не
// исчезает, а остаётся строкой с причиной. Молча пропавший узел неотличим
// от узла, которого у провайдера больше нет, — и владелец ищет пропажу
// не там.
func TestConvertXHTTPSwitch(t *testing.T) {
	entries, err := parse(subscription(t), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := entries[1]
	if e.Kind != KindUnsupported {
		t.Fatalf("вид %q, ожидался %q", e.Kind, KindUnsupported)
	}
	if !strings.Contains(e.Reason, "xhttp") {
		t.Errorf("причина %q не называет транспорт", e.Reason)
	}
	// Узлы на tcp выключателем не задеты.
	if entries[5].Kind != KindNode {
		t.Errorf("tcp-узел получил вид %q", entries[5].Kind)
	}
}

func TestConvertUnsupported(t *testing.T) {
	cases := []struct {
		name     string
		outbound string
		want     string
	}{
		{
			"hysteria версии 1",
			`{"tag":"proxy","protocol":"hysteria",
			  "settings":{"address":"203.0.113.1","port":8449,"version":1},
			  "streamSettings":{"network":"hysteria","hysteriaSettings":{"version":1,"auth":"x"},
			  "security":"tls","tlsSettings":{"serverName":"s"}}}`,
			"hysteria версии 1",
		},
		{
			"vmess",
			`{"tag":"proxy","protocol":"vmess","settings":{},"streamSettings":{}}`,
			"vmess",
		},
		{
			"транспорт ws",
			`{"tag":"proxy","protocol":"vless",
			  "settings":{"vnext":[{"address":"203.0.113.1","port":443,"users":[{"id":"u"}]}]},
			  "streamSettings":{"network":"ws","security":"tls"}}`,
			"ws",
		},
		{
			"vless без шифрования",
			`{"tag":"proxy","protocol":"vless",
			  "settings":{"vnext":[{"address":"203.0.113.1","port":443,"users":[{"id":"u"}]}]},
			  "streamSettings":{"network":"tcp","security":"none"}}`,
			"none",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := []byte(`[{"remarks":"🇩🇪Тест","outbounds":[` + c.outbound + `]}]`)
			entries, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if entries[0].Kind != KindUnsupported {
				t.Fatalf("вид %q, ожидался %q", entries[0].Kind, KindUnsupported)
			}
			if !strings.Contains(entries[0].Reason, c.want) {
				t.Errorf("причина %q не содержит %q", entries[0].Reason, c.want)
			}
		})
	}
}

// TestConvertHysteriaUsesFlatSettings: у hysteria адрес лежит не в vnext, а
// плоско в settings. Перепутать формы легко, и ошибка молчаливая — узел
// соберётся с пустым адресом.
func TestConvertHysteriaUsesFlatSettings(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := entries[15] // 🇨🇭🎮Швейцария GAMING
	if e.Type != "hysteria2" {
		t.Fatalf("тип %q, ожидался hysteria2", e.Type)
	}
	if got := e.Proxy["server"]; got != "203.0.113.14" {
		t.Errorf("server = %v", got)
	}
	if got := e.Proxy["port"]; got != 8449 {
		t.Errorf("port = %v", got)
	}
	if _, ok := e.Proxy["uuid"]; ok {
		t.Error("у hysteria2 не должно быть uuid")
	}
}
