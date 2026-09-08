package rulesets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"netmoded/internal/geosite"
	"netmoded/internal/nikki"
)

// golden читает эталон из testdata.
//
// Эталоны написаны руками по §2 спеки и НЕ порождаются кодом: иначе golden
// проверял бы только то, что Render дважды подряд делает одно и то же, а не
// то, что он делает обещанное владельцу и понятное mihomo.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("эталон %s: %v", name, err)
	}
	return b
}

// TestRenderGolden — вывод Render байт-в-байт совпадает с эталонами.
//
// Байт-в-байт, а не «разбирается как YAML»: файл склеивает yq на роутере, и
// цена лишнего пробела в отступе — не косметика, а провал склейки, который
// виден только в core.log после перезапуска движка.
func TestRenderGolden(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		file string
	}{
		{
			name: "только эти наборы, качать напрямую",
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "youtube"}, {Name: "telegram", IP: true},
			}},
			file: "mixin-only.yaml",
		},
		{
			name: "всё кроме этих наборов, качать через туннель",
			cfg: Config{Policy: PolicyExcept, Download: DownloadTunnel, Sets: []Set{
				{Name: "github"}, {Name: "telegram", IP: true},
			}},
			file: "mixin-except-tunnel.yaml",
		},
		{
			name: "правила из профиля",
			cfg:  Config{Policy: PolicyProfile, Download: DownloadDirect},
			file: "mixin-profile.yaml",
		},
		{
			name: "имена с @ и ! плюс подсети",
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "netflix@ads"}, {Name: "category-ai-!cn", IP: true},
			}},
			file: "mixin-special.yaml",
		},
		{
			name: "свои правила всех видов перед наборами",
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect,
				Sets:  []Set{{Name: "youtube"}, {Name: "telegram", IP: true}},
				Rules: testRules(),
			},
			file: "mixin-rules.yaml",
		},
		{
			name: "своё правило без наборов при политике «кроме»",
			cfg: Config{Policy: PolicyExcept, Download: DownloadTunnel,
				Rules: []Rule{{Kind: RuleSuffix, Value: "netbird.io", Action: ActionDirect}},
			},
			file: "mixin-rules-nosets.yaml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := golden(t, tc.file)
			got := Render(tc.cfg)
			if string(got) != string(want) {
				t.Errorf("Render не совпал с %s.\n--- получено ---\n%s\n--- ожидалось ---\n%s",
					tc.file, got, want)
			}
		})
	}
}

// TestRenderKeepsPrivateBeforeMatch — строка GEOIP,PRIVATE стоит ровно перед
// MATCH при обеих политиках.
//
// Отдельная проверка сверх golden потому, что именно эта строка спасает
// локальную сеть: при политике «кроме» хвост MATCH,BYPASS уводит в туннель
// всё подряд, включая 192.168.9.0/24, и роутер перестаёт быть доступен из
// собственного LAN. Убрать её — самая дешёвая на вид правка и самая дорогая
// по последствиям.
func TestRenderKeepsPrivateBeforeMatch(t *testing.T) {
	for _, policy := range []Policy{PolicyOnly, PolicyExcept} {
		// Свои правила здесь тоже есть: они добавляются в ту же рубрику, и
		// проверка обязана доказать, что хвост от них не сдвинулся.
		lines := strings.Split(strings.TrimRight(string(Render(Config{
			Policy: policy, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}},
			Rules: []Rule{{Kind: RuleCIDR, Value: "100.64.0.0/10", Action: ActionDirect}},
		})), "\n"), "\n")

		last, prev := lines[len(lines)-1], lines[len(lines)-2]
		if prev != "  - 'GEOIP,PRIVATE,DIRECT,no-resolve'" {
			t.Errorf("политика %s: перед хвостом %q, ожидалась строка GEOIP,PRIVATE", policy, prev)
		}
		if !strings.HasPrefix(last, "  - 'MATCH,") {
			t.Errorf("политика %s: последняя строка %q, ожидался MATCH", policy, last)
		}
	}
}

// TestRenderEmptySetsHasNoProviders — «только эти наборы», но наборов ноль:
// файл состоит из шапки и двух строк хвоста, без рубрики rule-providers.
//
// Пустая рубрика превратилась бы в rule-providers: null и при склейке yq
// затёрла бы провайдеров профиля — то есть безобидный на вид «ничего не
// выбрано» сломал бы чужие правила.
func TestRenderEmptySetsHasNoProviders(t *testing.T) {
	got := string(Render(Config{Policy: PolicyOnly, Download: DownloadDirect}))
	if strings.Contains(got, "rule-providers") {
		t.Errorf("при пустом выборе появилась рубрика rule-providers:\n%s", got)
	}
	want := "nikki-rules:\n" +
		"  - 'GEOIP,PRIVATE,DIRECT,no-resolve'\n" +
		"  - 'MATCH,DIRECT'\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("хвост файла:\n%s\nожидался:\n%s", got, want)
	}
}

// testRules — по одному правилу каждого вида, включая IPv6.
func testRules() []Rule {
	return []Rule{
		{Kind: RuleSuffix, Value: "worldsimseries.com", Action: ActionTunnel, Comment: "Лига WSS: паддок"},
		{Kind: RuleDomain, Value: "api.netbird.io", Action: ActionDirect},
		{Kind: RuleCIDR, Value: "100.64.0.0/10", Action: ActionDirect, Comment: "CGNAT netbird"},
		{Kind: RuleCIDR, Value: "fd00::/8", Action: ActionDirect},
	}
}

// TestRenderCommentLine — комментарий стоит строкой «  # …» ровно над своим
// правилом, и Parse его правилом не считает.
//
// Комментарий — для человека, читающего файл по ssh; правда о нём живёт в
// строке состояния. Строка с решёткой не должна ни сбивать счёт правил, ни
// попадать в другое место файла: над чужим правилом она врала бы.
func TestRenderCommentLine(t *testing.T) {
	cfg := Config{Policy: PolicyOnly, Download: DownloadDirect, Rules: []Rule{
		{Kind: RuleSuffix, Value: "a.com", Action: ActionTunnel},
		{Kind: RuleCIDR, Value: "10.0.0.0/8", Action: ActionDirect, Comment: "домашняя сеть; см. wiki > vpn"},
		{Kind: RuleDomain, Value: "b.com", Action: ActionDirect},
	}}
	out := string(Render(cfg))
	want := "  # домашняя сеть; см. wiki > vpn\n  - 'IP-CIDR,10.0.0.0/8,DIRECT,no-resolve'\n"
	if !strings.Contains(out, want) {
		t.Errorf("комментарий не над своим правилом:\n%s", out)
	}
	if strings.Count(out, "  # ") != 1 {
		t.Errorf("строк комментария %d, ожидалась одна:\n%s", strings.Count(out, "  # "), out)
	}
	got, _, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("прочитано %+v, записано %+v", got, cfg)
	}
}

// TestRenderRulesPrecedeSets — свои правила стоят раньше первого RULE-SET, и
// группа у них берётся из действия, а не из политики.
//
// Смысл своих правил — воля владельца поверх набора: «этот домен напрямую»
// должно побеждать набор, в котором домен есть. Стой они после наборов, набор
// забирал бы соединение первым, и правило было бы мёртвым — ровно та беда,
// из-за которой они появились (правила профиля за нашим MATCH).
func TestRenderRulesPrecedeSets(t *testing.T) {
	for _, policy := range []Policy{PolicyOnly, PolicyExcept} {
		lines := strings.Split(strings.TrimRight(string(Render(Config{
			Policy: policy, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}},
			Rules: []Rule{
				{Kind: RuleSuffix, Value: "worldsimseries.com", Action: ActionTunnel},
				{Kind: RuleDomain, Value: "api.netbird.io", Action: ActionDirect},
			},
		})), "\n"), "\n")

		firstSet, lastCustom := -1, -1
		for i, ln := range lines {
			switch {
			case strings.HasPrefix(ln, "  - 'RULE-SET,") && firstSet < 0:
				firstSet = i
			case strings.HasPrefix(ln, "  - 'DOMAIN"):
				lastCustom = i
			}
		}
		if firstSet < 0 || lastCustom < 0 || lastCustom > firstSet {
			t.Errorf("политика %s: своё правило (строка %d) не раньше первого набора (строка %d):\n%s",
				policy, lastCustom, firstSet, strings.Join(lines, "\n"))
		}
		want := "  - 'DOMAIN-SUFFIX,worldsimseries.com," + TunnelGroup + "'"
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Errorf("политика %s: нет строки %q — группа взята из политики, а не из действия", policy, want)
		}
	}
}

// TestParseRoundTrip — Parse(Render(c)) возвращает ровно c.
//
// Это и есть смысл всей затеи: второго хранилища выбора нет, состояние
// читается из того же файла, который демон написал. Если пара не сходится,
// панель после перезапуска покажет не то, что применено.
func TestParseRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"только эти", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "youtube"}, {Name: "telegram", IP: true},
		}}},
		{"кроме этих через туннель", Config{Policy: PolicyExcept, Download: DownloadTunnel, Sets: []Set{
			{Name: "github"}, {Name: "telegram", IP: true},
		}}},
		{"из профиля", Config{Policy: PolicyProfile, Download: DownloadDirect}},
		{"имена с @ и !", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "netflix@ads"}, {Name: "category-ai-!cn", IP: true},
		}}},
		{"ничего не выбрано", Config{Policy: PolicyExcept, Download: DownloadTunnel}},
		{"свои правила с наборами", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Sets: []Set{{Name: "youtube"}, {Name: "telegram", IP: true}}, Rules: testRules()}},
		{"своё правило без наборов", Config{Policy: PolicyExcept, Download: DownloadTunnel,
			Rules: []Rule{{Kind: RuleSuffix, Value: "netbird.io", Action: ActionDirect}}}},
		// Комментарий со всеми знаками, которые что-то значат в строке
		// состояния: пробел (разделитель полей), ; (правил), > (действия),
		// | (комментария), # (комментарий YAML), плюс и процент (кодирование).
		{"комментарий с опасными знаками", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel,
				Comment: "a;b>c#d|e f+g%h — кириллица"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, foreign, err := Parse(Render(tc.cfg))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if foreign {
				t.Error("свой же файл опознан как чужой")
			}
			if !reflect.DeepEqual(got, tc.cfg) {
				t.Errorf("прочитано %+v, записано %+v", got, tc.cfg)
			}
		})
	}
}

// TestParseTreatsMissingAndCommentsAsProfile — пустого файла нет, файл из
// поставки nikki комментарный: и то и другое — «правила из профиля», и НЕ
// чужой файл.
//
// Иначе первое же открытие вкладки на свежем роутере отвечало бы
// «файл чужой, ничего не трогаю», и наборы нельзя было бы применить вовсе.
func TestParseTreatsMissingAndCommentsAsProfile(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"файла нет", nil},
		{"файл пуст", []byte{}},
		{"одни переводы строк", []byte("\n\n   \n")},
		{"комментарный из поставки nikki", golden(t, "mixin-nikki-default.yaml")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, foreign, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if foreign {
				t.Error("опознан как чужой, ожидалось «наш, но пустой»")
			}
			if want := (Config{Policy: PolicyProfile, Download: DownloadDirect}); !reflect.DeepEqual(cfg, want) {
				t.Errorf("прочитано %+v, ожидалось %+v", cfg, want)
			}
		})
	}
}

// TestParseForeignFile — владелец написал правила сам: шапки нет, содержимое
// есть. Такой файл переписывать нельзя, и отличить его надо от комментарного.
func TestParseForeignFile(t *testing.T) {
	cfg, foreign, err := Parse(golden(t, "mixin-foreign.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !foreign {
		t.Fatal("чужой файл не опознан — панель бы его затёрла")
	}
	if cfg.Policy != PolicyProfile || len(cfg.Sets) != 0 {
		t.Errorf("у чужого файла состояние %+v, ожидалось пустое profile", cfg)
	}
}

// TestParseCorrupt — шапка наша, но тело не сходится с состоянием.
//
// Расхождение значит, что файл правили руками либо запись оборвалась: верить
// строке состояния больше нельзя, и честнее сказать «примените заново», чем
// показать владельцу выбор, которого в правилах нет.
func TestParseCorrupt(t *testing.T) {
	head := "# netmoded: шапка\n# вторая строка\n"

	cases := []struct {
		name string
		in   []byte
	}{
		{"правил меньше, чем наборов", golden(t, "mixin-corrupt.yaml")},
		{"строки состояния нет вовсе", []byte(head + "rule-providers:\n")},
		{"неизвестная политика", []byte(head + stateMark + " policy=maybe download=direct sets=\n")},
		{"неизвестное скачивание", []byte(head + stateMark + " policy=only download=carrier sets=\n")},
		{"неизвестное поле состояния", []byte(head + stateMark + " policy=only mood=good sets=\n")},
		{"пустое имя набора", []byte(head + stateMark + " policy=only download=direct sets=youtube,\n")},
		// Тело здесь СХОДИТСЯ с состоянием — одно правило на один набор,
		// — и без проверки знаков такой файл проезжал бы молча: имя с
		// апострофом уехало бы обратно в mixin.yaml и порвало бы YAML на
		// следующем старте nikki.
		{"имя с апострофом", []byte(head + stateMark + " policy=only download=direct sets=it's\n" +
			"nikki-rules:\n" + ruleSiteLinePrefix + "it's,BYPASS'\n")},

		// Свои правила: строка состояния и тело обязаны сходиться и здесь.
		{"неизвестный вид правила", []byte(head + stateMark + " policy=only download=direct sets= rules=glob:x.com>tunnel\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n")},
		{"неизвестное действие правила", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com>reject\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,REJECT'\n")},
		{"недопустимое значение правила", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:Bad.Com>tunnel\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,Bad.Com,BYPASS'\n")},
		{"дубль правила", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com>tunnel;suffix:x.com>direct\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n  - 'DOMAIN-SUFFIX,x.com,DIRECT'\n")},
		{"токен правила без действия", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n")},
		{"правило в состоянии есть, строки в теле нет", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com>tunnel\n" +
			"nikki-rules:\n  - 'GEOIP,PRIVATE,DIRECT,no-resolve'\n  - 'MATCH,DIRECT'\n")},
		// Строка дописана руками: в состоянии её нет, и молча принять её
		// значило бы потерять при следующей записи то, что владелец считает
		// применённым.
		{"строка в теле есть, правила в состоянии нет", []byte(head + stateMark + " policy=only download=direct sets=\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n  - 'GEOIP,PRIVATE,DIRECT,no-resolve'\n  - 'MATCH,DIRECT'\n")},
		// Комментарий в состоянии: битое кодирование, перевод строки,
		// длиннее потолка — всё это файл, правленный руками.
		{"комментарий с битым процент-кодированием", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com>tunnel|%ZZ\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n")},
		{"комментарий с переводом строки", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com>tunnel|a%0Ab\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n")},
		{"комментарий длиннее 80", []byte(head + stateMark + " policy=only download=direct sets= rules=suffix:x.com>tunnel|" + strings.Repeat("a", 81) + "\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,BYPASS'\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse(tc.in)
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("получено %v, ожидалось ErrCorrupt", err)
			}
		})
	}
}

// TestParseDownloadDefaultsToDirect — старый файл без поля download читается
// как «качать напрямую», а не как повреждённый: умолчание меняться не должно.
func TestParseDownloadDefaultsToDirect(t *testing.T) {
	in := []byte("# netmoded: шапка\n# вторая строка\n" +
		stateMark + " policy=only sets=youtube\n" +
		"nikki-rules:\n  - 'RULE-SET,nm-geosite-youtube,BYPASS'\n")

	cfg, _, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Download != DownloadDirect {
		t.Errorf("скачивание %q, ожидалось %q", cfg.Download, DownloadDirect)
	}
}

// TestParseOldFileWithoutRulesHasNone — файл, записанный до появления своих
// правил, читается как «правил нет», а не как повреждённый и не как «есть
// пустое». nil, а не пустой срез: так Parse(Render(c)) сходится с c без правил.
func TestParseOldFileWithoutRulesHasNone(t *testing.T) {
	in := []byte("# netmoded: шапка\n# вторая строка\n" +
		stateMark + " policy=only download=direct sets=youtube\n" +
		"nikki-rules:\n  - 'RULE-SET,nm-geosite-youtube,BYPASS'\n" +
		"  - 'GEOIP,PRIVATE,DIRECT,no-resolve'\n  - 'MATCH,DIRECT'\n")

	cfg, _, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Rules != nil {
		t.Errorf("правила %+v, ожидался nil", cfg.Rules)
	}
}

// TestFingerprintSensitive — отпечаток различает всё, из-за чего файл надо
// переписать, и совпадает у одинакового выбора.
//
// На нём держится If-Match: если отпечаток не заметит перестановку наборов,
// две открытые вкладки молча затрут выбор друг друга.
func TestFingerprintSensitive(t *testing.T) {
	base := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube"}, {Name: "telegram", IP: true},
	}}
	same := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube"}, {Name: "telegram", IP: true},
	}}
	if Fingerprint(base) != Fingerprint(same) {
		t.Error("одинаковый выбор дал разные отпечатки")
	}
	if !strings.HasPrefix(Fingerprint(base), "sha256:") {
		t.Errorf("отпечаток %q без приставки sha256:", Fingerprint(base))
	}
	// Литерал посчитан ДО появления своих правил. Выбор без правил обязан
	// давать тот же отпечаток, что раньше: иначе после обновления демона
	// первый же PUT из открытой вкладки отбивался бы как stale_rulesets, а
	// пример в openapi и nikki-rulesets-only.json стал бы враньём.
	if got := Fingerprint(base); got != "sha256:467b08a81b06cf99" {
		t.Errorf("отпечаток выбора без правил %s, до своих правил был sha256:467b08a81b06cf99", got)
	}

	rule := Rule{Kind: RuleSuffix, Value: "worldsimseries.com", Action: ActionTunnel}
	others := []struct {
		name string
		cfg  Config
	}{
		{"другой порядок наборов", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "telegram", IP: true}, {Name: "youtube"},
		}}},
		{"другое скачивание", Config{Policy: PolicyOnly, Download: DownloadTunnel, Sets: base.Sets}},
		{"другая политика", Config{Policy: PolicyExcept, Download: DownloadDirect, Sets: base.Sets}},
		{"снят признак подсетей", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "youtube"}, {Name: "telegram"},
		}}},
		{"набор убран", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "youtube"},
		}}},
		{"добавлено своё правило", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: base.Sets,
			Rules: []Rule{rule}}},
	}
	for _, tc := range others {
		if Fingerprint(tc.cfg) == Fingerprint(base) {
			t.Errorf("%s: отпечаток не изменился", tc.name)
		}
	}

	// Среди правил различаются действие и порядок: «домен напрямую» и
	// «домен в туннель» — разные выборы, а порядок — это кто побеждает.
	withRule := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: base.Sets, Rules: []Rule{
		rule, {Kind: RuleCIDR, Value: "100.64.0.0/10", Action: ActionDirect},
	}}
	flipped := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: base.Sets, Rules: []Rule{
		{Kind: RuleSuffix, Value: "worldsimseries.com", Action: ActionDirect}, withRule.Rules[1],
	}}
	swapped := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: base.Sets, Rules: []Rule{
		withRule.Rules[1], withRule.Rules[0],
	}}
	if Fingerprint(withRule) == Fingerprint(flipped) {
		t.Error("смена действия правила не изменила отпечаток")
	}
	if Fingerprint(withRule) == Fingerprint(swapped) {
		t.Error("перестановка правил не изменила отпечаток")
	}

	// Комментарий — тоже часть выбора: сменился текст, файл надо переписать.
	// А правило БЕЗ комментария даёт тот же токен, что до появления поля:
	// литерал посчитан до него, и отпечатки правил на роутере не меняются.
	commented := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: base.Sets, Rules: []Rule{
		{Kind: RuleSuffix, Value: "worldsimseries.com", Action: ActionTunnel, Comment: "лига"}, withRule.Rules[1],
	}}
	if Fingerprint(withRule) == Fingerprint(commented) {
		t.Error("смена комментария не изменила отпечаток")
	}
	noComments := Config{Policy: PolicyOnly, Download: DownloadDirect,
		Sets: []Set{{Name: "youtube"}, {Name: "telegram", IP: true}},
		Rules: []Rule{
			{Kind: RuleSuffix, Value: "worldsimseries.com", Action: ActionTunnel},
			{Kind: RuleDomain, Value: "api.netbird.io", Action: ActionDirect},
			{Kind: RuleCIDR, Value: "100.64.0.0/10", Action: ActionDirect},
			{Kind: RuleCIDR, Value: "fd00::/8", Action: ActionDirect},
		}}
	if got := Fingerprint(noComments); got != "sha256:0e5545a52d1a0040" {
		t.Errorf("отпечаток правил без комментариев %s, до поля был sha256:0e5545a52d1a0040", got)
	}
}

// testCatalog — каталог для проверок имён.
func testCatalog() *geosite.Catalog {
	return geosite.NewForTest(
		[]string{"youtube", "telegram", "github", "netflix@ads"},
		[]string{"telegram", "netflix@ads"},
	)
}

// TestValidate — таблица отказов и разрешений.
func TestValidate(t *testing.T) {
	cat := testCatalog()
	// Второе имя применено, но написано непозволительно: попасть в файл
	// оно могло только правкой руками, а PUT с ним придёт как «уже
	// применённое», то есть мимо сверки с каталогом.
	applied := []Set{{Name: "already-applied", IP: true}, {Name: "bad'name"}}

	cases := []struct {
		name    string
		cfg     Config
		cat     *geosite.Catalog
		wantErr bool
		// wantIs — какой сентинел обязан опознаваться (nil — любой отказ).
		wantIs error
		// wantUnknown — отказ обязан быть *UnknownSetsError.
		wantUnknown bool
		// wantIn — что обязано быть в тексте ошибки.
		wantIn []string
	}{
		{name: "только эти, имена из каталога", cat: cat,
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}}}},
		{name: "кроме этих, наборов ноль", cat: cat,
			cfg: Config{Policy: PolicyExcept, Download: DownloadTunnel}},
		{name: "из профиля без наборов", cat: cat,
			cfg: Config{Policy: PolicyProfile, Download: DownloadDirect}},
		{name: "уже применённое имя валидно и без каталога", cat: nil,
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "already-applied"}}}},

		{name: "неизвестная политика", cat: cat, wantErr: true,
			cfg: Config{Policy: "maybe", Download: DownloadDirect}},
		{name: "неизвестное скачивание", cat: cat, wantErr: true,
			cfg: Config{Policy: PolicyOnly, Download: "carrier"}},
		{name: "из профиля с наборами", cat: cat, wantErr: true,
			cfg: Config{Policy: PolicyProfile, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}}}},
		{name: "дубль имени", cat: cat, wantErr: true, wantIn: []string{"youtube"},
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "youtube"}, {Name: "telegram"}, {Name: "youtube"},
			}}},
		{name: "имён нет в каталоге", cat: cat, wantErr: true,
			wantUnknown: true, wantIn: []string{"nosuchset", "norsuchone"},
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "youtube"}, {Name: "nosuchset"}, {Name: "norsuchone"},
			}}},
		// Текст обязан назвать причину именно знаками: имя с запятой есть
		// в списке «неизвестных» и без проверки, но чинится оно не поиском
		// по каталогу.
		{name: "посторонний знак в имени", cat: cat, wantErr: true,
			wantIn: []string{"a,b", "недопустим"},
			cfg:    Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "a,b"}}}},
		{name: "посторонний знак у уже применённого имени", cat: cat, wantErr: true,
			wantIn: []string{"недопустим"},
			cfg:    Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "bad'name"}}}},
		{name: "каталога нет, а имя новое", cat: nil, wantErr: true, wantIs: ErrNoCatalog,
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg, tc.cat, applied)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate: %v, ожидалось разрешение", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate разрешил, ожидался отказ")
			}
			if tc.wantUnknown {
				var got *UnknownSetsError
				if !errors.As(err, &got) {
					t.Fatalf("ошибка %v не *UnknownSetsError", err)
				}
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("ошибка %v не опознана как %v", err, tc.wantIs)
			}
			for _, s := range tc.wantIn {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("в тексте %q нет %q", err.Error(), s)
				}
			}
		})
	}
}

// TestValidateRules — что принимается и что отбивается среди своих правил.
//
// Отказ обязан быть *RuleError с номером строки (с единицы) и значением: панель
// подсвечивает именно эту строку, а владелец читает, что с ней не так. Правила
// проверяются раньше каталога и не зависят от него: домен и подсеть каталогу
// сверять не с чем.
func TestValidateRules(t *testing.T) {
	ok := func(k RuleKind, v string, a RuleAction) Config {
		return Config{Policy: PolicyOnly, Download: DownloadDirect, Rules: []Rule{{Kind: k, Value: v, Action: a}}}
	}

	accept := []struct {
		name string
		cfg  Config
	}{
		{"домен с поддоменами", ok(RuleSuffix, "worldsimseries.com", ActionTunnel)},
		{"точный хост", ok(RuleDomain, "api.netbird.io", ActionDirect)},
		{"подсеть IPv4", ok(RuleCIDR, "100.64.0.0/10", ActionDirect)},
		{"подсеть IPv6", ok(RuleCIDR, "fd00::/8", ActionDirect)},
		{"один адрес как подсеть", ok(RuleCIDR, "10.0.0.1/32", ActionTunnel)},
		{"домен из одной метки", ok(RuleSuffix, "lan", ActionDirect)},
		{"punycode", ok(RuleSuffix, "xn--e1afmkfd.xn--p1ai", ActionTunnel)},
		{"дефис внутри метки и цифры", ok(RuleSuffix, "my-site1.co.uk", ActionTunnel)},
		{"правила при политике «кроме» без наборов", Config{Policy: PolicyExcept, Download: DownloadTunnel,
			Rules: []Rule{{Kind: RuleSuffix, Value: "netbird.io", Action: ActionDirect}}}},
		{"правила без каталога", ok(RuleSuffix, "example.com", ActionTunnel)},
		{"комментарий в 80 рун кириллицей", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel, Comment: strings.Repeat("ё", 80)}}}},
		{"комментарий с пробелами внутри и знаками", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleCIDR, Value: "157.90.0.0/16", Action: ActionTunnel, Comment: "серверы WSS; Hetzner (выделенные) > туннель #1"}}}},
	}
	for _, tc := range accept {
		t.Run("принято: "+tc.name, func(t *testing.T) {
			// cat=nil: правилам каталог не нужен, и ErrNoCatalog их не касается.
			if err := Validate(tc.cfg, nil, nil); err != nil {
				t.Fatalf("Validate: %v, ожидалось разрешение", err)
			}
		})
	}

	many := make([]Rule, MaxRules+1)
	for i := range many {
		many[i] = Rule{Kind: RuleCIDR, Value: fmt.Sprintf("10.%d.0.0/16", i), Action: ActionDirect}
	}

	reject := []struct {
		name   string
		cfg    Config
		index  int
		wantIn []string
	}{
		{"пустое значение", ok(RuleSuffix, "", ActionTunnel), 1, nil},
		{"неизвестный вид", ok("glob", "x.com", ActionTunnel), 1, []string{"glob"}},
		{"неизвестное действие", ok(RuleSuffix, "x.com", "reject"), 1, []string{"reject"}},
		{"из профиля с правилами", Config{Policy: PolicyProfile, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel}}}, 1, []string{"profile"}},
		{"больше потолка", Config{Policy: PolicyOnly, Download: DownloadDirect, Rules: many}, MaxRules + 1, []string{"64"}},
		{"дубль вида и значения при разных действиях", Config{Policy: PolicyOnly, Download: DownloadDirect, Rules: []Rule{
			{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel},
			{Kind: RuleCIDR, Value: "10.0.0.0/8", Action: ActionDirect},
			{Kind: RuleSuffix, Value: "x.com", Action: ActionDirect},
		}}, 3, []string{"x.com", "дважды"}},
		{"схема в домене", ok(RuleSuffix, "https://x.com", ActionTunnel), 1, []string{"https://x.com"}},
		{"путь в домене", ok(RuleSuffix, "x.com/path", ActionTunnel), 1, nil},
		{"порт в домене", ok(RuleDomain, "x.com:443", ActionTunnel), 1, nil},
		{"точка в конце", ok(RuleSuffix, "x.com.", ActionTunnel), 1, nil},
		{"звёздочка в начале", ok(RuleSuffix, "*.x.com", ActionTunnel), 1, nil},
		{"верхний регистр", ok(RuleSuffix, "Example.com", ActionTunnel), 1, []string{"Example.com"}},
		{"пробел внутри", ok(RuleSuffix, "x .com", ActionTunnel), 1, nil},
		{"подчёркивание", ok(RuleSuffix, "my_site.com", ActionTunnel), 1, nil},
		{"не-ASCII", ok(RuleSuffix, "пример.рф", ActionTunnel), 1, []string{"punycode"}},
		{"метка начинается с дефиса", ok(RuleSuffix, "-x.com", ActionTunnel), 1, nil},
		{"метка кончается дефисом", ok(RuleSuffix, "x-.com", ActionTunnel), 1, nil},
		{"пустая метка", ok(RuleSuffix, "x..com", ActionTunnel), 1, nil},
		{"метка длиннее 63", ok(RuleSuffix, strings.Repeat("a", 64)+".com", ActionTunnel), 1, nil},
		{"имя длиннее 253", ok(RuleSuffix, strings.Repeat("abcdefghi.", 26)+"com", ActionTunnel), 1, nil},
		{"адрес под видом домена", ok(RuleSuffix, "1.2.3.4", ActionTunnel), 1, []string{"cidr"}},
		{"адрес под видом точного хоста", ok(RuleDomain, "fd00::1", ActionTunnel), 1, []string{"cidr"}},
		{"подсеть без маски", ok(RuleCIDR, "10.0.0.1", ActionDirect), 1, []string{"/"}},
		{"подсеть с битами хоста", ok(RuleCIDR, "100.64.1.0/10", ActionDirect), 1, []string{"100.64.0.0/10"}},
		{"подсеть не канонична", ok(RuleCIDR, "001.2.3.0/24", ActionDirect), 1, nil},
		{"IPv6 в верхнем регистре", ok(RuleCIDR, "FD00::/8", ActionDirect), 1, nil},
		{"IPv4 внутри IPv6", ok(RuleCIDR, "::ffff:1.2.3.0/120", ActionDirect), 1, nil},
		{"домен под видом подсети", ok(RuleCIDR, "x.com", ActionDirect), 1, []string{"x.com"}},
		{"комментарий в 81 руну", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel, Comment: strings.Repeat("ё", 81)}}}, 1, []string{"80"}},
		{"комментарий с переводом строки", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel, Comment: "a\nb"}}}, 1, nil},
		{"комментарий с управляющим символом", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel, Comment: "a\x01b"}}}, 1, nil},
		{"комментарий с пробелом по краю", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel, Comment: " лига"}}}, 1, nil},
		{"комментарий не UTF-8", Config{Policy: PolicyOnly, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: ActionTunnel, Comment: "a\xffb"}}}, 1, nil},
	}
	for _, tc := range reject {
		t.Run("отбито: "+tc.name, func(t *testing.T) {
			err := Validate(tc.cfg, nil, nil)
			if err == nil {
				t.Fatal("Validate разрешил, ожидался отказ")
			}
			var re *RuleError
			if !errors.As(err, &re) {
				t.Fatalf("ошибка %v не *RuleError", err)
			}
			if re.Index != tc.index {
				t.Errorf("номер строки %d, ожидался %d", re.Index, tc.index)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tc.index)) {
				t.Errorf("в тексте %q нет номера строки", err.Error())
			}
			for _, s := range tc.wantIn {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("в тексте %q нет %q", err.Error(), s)
				}
			}
		})
	}
}

// TestValidateRulesBeforeCatalog — плохое правило отбивается даже тогда, когда
// каталога нет и наборы новые: иначе владелец получал бы «каталог недоступен»
// вместо «в правиле опечатка», и чинил бы не то.
func TestValidateRulesBeforeCatalog(t *testing.T) {
	err := Validate(Config{Policy: PolicyOnly, Download: DownloadDirect,
		Sets:  []Set{{Name: "youtube"}},
		Rules: []Rule{{Kind: RuleSuffix, Value: "Bad.Com", Action: ActionTunnel}},
	}, nil, nil)
	var re *RuleError
	if !errors.As(err, &re) {
		t.Fatalf("ошибка %v не *RuleError — правило заслонил каталог", err)
	}
}

// TestValidateListsUnknownNames — неизвестные имена перечислены все и в
// порядке выбора: владелец должен увидеть, какие именно чипы убрать.
func TestValidateListsUnknownNames(t *testing.T) {
	err := Validate(Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "zzz"}, {Name: "youtube"}, {Name: "aaa"},
	}}, testCatalog(), nil)

	var unknown *UnknownSetsError
	if !errors.As(err, &unknown) {
		t.Fatalf("ошибка %v не *UnknownSetsError", err)
	}
	if want := []string{"zzz", "aaa"}; !reflect.DeepEqual(unknown.Names, want) {
		t.Errorf("перечислено %v, ожидалось %v", unknown.Names, want)
	}
}

// TestResolveTakesIPFromCatalog — признак подсетей проставляет каталог, а не
// панель: только он знает, есть ли имя в дереве geoip.
func TestResolveTakesIPFromCatalog(t *testing.T) {
	in := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube", IP: true}, // в geoip его нет — признак обязан слететь
		{Name: "telegram"},          // в geoip есть — признак обязан появиться
		{Name: "netflix@ads", IP: true},
	}}

	got := Resolve(in, testCatalog(), nil)
	want := []Set{{Name: "youtube"}, {Name: "telegram", IP: true}, {Name: "netflix@ads", IP: true}}
	if !reflect.DeepEqual(got.Sets, want) {
		t.Errorf("наборы %+v, ожидались %+v", got.Sets, want)
	}
	if !in.Sets[0].IP {
		t.Error("Resolve испортил исходную конфигурацию — она общая с вызывающим")
	}
}

// TestResolveFallsBackToApplied — каталога нет, но имя уже применено: признак
// подсетей берётся из файла. Иначе повторное применение без интернета молча
// потеряло бы половину набора telegram.
func TestResolveFallsBackToApplied(t *testing.T) {
	applied := []Set{{Name: "telegram", IP: true}, {Name: "youtube"}}
	in := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "telegram"}, {Name: "youtube", IP: true},
	}}

	got := Resolve(in, nil, applied)
	want := []Set{{Name: "telegram", IP: true}, {Name: "youtube"}}
	if !reflect.DeepEqual(got.Sets, want) {
		t.Errorf("наборы %+v, ожидались %+v", got.Sets, want)
	}
}

// TestProviderNames — имена провайдеров: одно без подсетей, два с ними.
func TestProviderNames(t *testing.T) {
	if got, want := ProviderNames(Set{Name: "youtube"}), []string{"nm-geosite-youtube"}; !reflect.DeepEqual(got, want) {
		t.Errorf("без подсетей %v, ожидалось %v", got, want)
	}
	got := ProviderNames(Set{Name: "telegram", IP: true})
	want := []string{"nm-geosite-telegram", "nm-geoip-telegram"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("с подсетями %v, ожидалось %v", got, want)
	}
}

// TestVerify — что считать загруженным.
func TestVerify(t *testing.T) {
	at := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	live := map[string]nikki.RuleProvider{
		"nm-geosite-youtube":  {Name: "nm-geosite-youtube", RuleCount: 1284, UpdatedAt: at},
		"nm-geosite-telegram": {Name: "nm-geosite-telegram", RuleCount: 42, UpdatedAt: at},
		// Подсети telegram движок ни разу не скачал: updatedAt нулевой.
		"nm-geoip-telegram": {Name: "nm-geoip-telegram"},
		// Набор пуст в самом репозитории — но скачан. Это загруженный набор.
		"nm-geosite-пустой": {Name: "nm-geosite-пустой", RuleCount: 0, UpdatedAt: at},
	}

	sets := []Set{
		{Name: "youtube"},
		{Name: "telegram", IP: true},
		{Name: "пустой"},
		{Name: "которого нет"},
	}

	loaded, missing := Verify(sets, live)
	wantLoaded := []Set{{Name: "youtube"}, {Name: "пустой"}}
	wantMissing := []Set{{Name: "telegram", IP: true}, {Name: "которого нет"}}
	if !reflect.DeepEqual(loaded, wantLoaded) {
		t.Errorf("загружены %+v, ожидались %+v", loaded, wantLoaded)
	}
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Errorf("не загружены %+v, ожидались %+v", missing, wantMissing)
	}
}

// TestVerifyNeverReturnsNil — пустые срезы, а не nil: вызывающий кладёт их в
// JSON, и nil стал бы там null вместо [].
func TestVerifyNeverReturnsNil(t *testing.T) {
	loaded, missing := Verify(nil, nil)
	if loaded == nil || missing == nil {
		t.Errorf("Verify вернул nil: loaded=%v missing=%v", loaded, missing)
	}
	if len(loaded) != 0 || len(missing) != 0 {
		t.Errorf("Verify на пустом входе вернул %v/%v", loaded, missing)
	}
}
