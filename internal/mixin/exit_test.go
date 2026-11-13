package mixin

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// exitCfg — выбор из эталона mixin-exit.yaml: наборы и правила трёх видов
// целей вперемешку. Порядок намеренно смешанный — у mihomo побеждает первое
// совпадение, и цель-группа не должна сдвигать строки в отдельный ярус.
func exitCfg() Config {
	return Config{Policy: PolicyDirect, Download: DownloadDirect,
		Sets: []Set{
			{Name: "openai", Action: ActionExit, Group: "EXIT-RESI"},
			{Name: "youtube", Action: ActionTunnel},
			{Name: "telegram", IP: true, Action: ActionExit, Group: "EXIT-DC"},
		},
		Rules: []Rule{
			{Kind: RuleSuffix, Value: "ipinfo.io", Action: ActionExit, Group: "EXIT-RESI", Comment: "geo check"},
			{Kind: RuleCIDR, Value: "149.154.160.0/20", Action: ActionExit, Group: "EXIT-DC"},
			{Kind: RuleDomain, Value: "api.netbird.io", Action: ActionDirect},
		}}
}

// TestRenderExitGolden — цель-группа уходит в правило именем группы, на том
// месте, где набор стоит в выборе.
func TestRenderExitGolden(t *testing.T) {
	if got, want := string(Render(exitCfg())), string(golden(t, "mixin-exit.yaml")); got != want {
		t.Errorf("Render расходится с эталоном.\n--- получено ---\n%s\n--- эталон ---\n%s", got, want)
	}
}

// TestParseExitRoundTrip — то, что записано, читается обратно тем же.
func TestParseExitRoundTrip(t *testing.T) {
	got, foreign, err := Parse(golden(t, "mixin-exit.yaml"))
	if err != nil || foreign {
		t.Fatalf("Parse: %v, foreign=%v", err, foreign)
	}
	if !reflect.DeepEqual(got, exitCfg()) {
		t.Errorf("прочитано %+v,\nожидалось %+v", got, exitCfg())
	}
}

// TestExitKeepsOldFingerprints — выбор без целей-групп даёт прежний токен и
// прежний отпечаток: обновление демона не должно делать чужие If-Match
// протухшими. Литерал тот же, что в TestFingerprintSensitive.
func TestExitKeepsOldFingerprints(t *testing.T) {
	base := Config{Policy: PolicyDirect, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube", Action: ActionTunnel}, {Name: "telegram", IP: true, Action: ActionTunnel},
	}}
	if got := Fingerprint(base); got != "sha256:3152c5b169d15b70" {
		t.Errorf("отпечаток %s, закреплён sha256:3152c5b169d15b70", got)
	}
	// А смена группы — смена выбора.
	a, b := exitCfg(), exitCfg()
	b.Sets[0].Group = "EXIT-DC"
	if Fingerprint(a) == Fingerprint(b) {
		t.Error("смена группы набора не изменила отпечаток")
	}
	c := exitCfg()
	c.Rules[0].Group = "EXIT-DC"
	if Fingerprint(a) == Fingerprint(c) {
		t.Error("смена группы правила не изменила отпечаток")
	}
}

// TestParseExitCorrupt — всё, чего мы сами не пишем, — порча, а не догадка.
func TestParseExitCorrupt(t *testing.T) {
	head := "# netmoded: шапка\n# вторая строка\n"
	set := func(tok string) []byte {
		return []byte(head + stateMark + " policy=direct download=direct sets=" + tok + "\n" +
			"nikki-rules:\n" + ruleSiteLinePrefix + "openai,EXIT-RESI'\n")
	}
	rule := func(tok string) []byte {
		return []byte(head + stateMark + " policy=direct download=direct sets= rules=" + tok + "\n" +
			"nikki-rules:\n  - 'DOMAIN-SUFFIX,x.com,EXIT-RESI'\n")
	}
	cases := []struct {
		name string
		in   []byte
	}{
		{"группа без приставки EXIT-", set("openai>@PROXY")},
		{"группа строчными", set("openai>@exit-resi")},
		{"пустая группа", set("openai>@")},
		{"одна приставка", set("openai>@EXIT-")},
		{"апостроф в группе", set("openai>@EXIT-it's")},
		{"группа длиннее потолка", set("openai>@EXIT-" + strings.Repeat("A", 33))},
		// Старый файл направления не писал вовсе — тем более группу.
		{"группа при старой политике", []byte(head + stateMark + " policy=only download=direct sets=openai>@EXIT-RESI\n" +
			"nikki-rules:\n" + ruleSiteLinePrefix + "openai,EXIT-RESI'\n")},
		{"правило с плохой группой", rule("suffix:x.com>@EXIT-a;b")},
		{"правило с группой без приставки", rule("suffix:x.com>@BYPASS")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Parse(tc.in); !errors.Is(err, ErrCorrupt) {
				t.Errorf("получено %v, ожидалось ErrCorrupt", err)
			}
		})
	}
}

// TestValidateExit — форма цели-группы на входе PUT.
func TestValidateExit(t *testing.T) {
	cat := testCatalog()
	set := func(a RuleAction, g string) Config {
		return Config{Policy: PolicyDirect, Download: DownloadDirect, Sets: []Set{{Name: "youtube", Action: a, Group: g}}}
	}
	rule := func(a RuleAction, g string) Config {
		return Config{Policy: PolicyDirect, Download: DownloadDirect,
			Rules: []Rule{{Kind: RuleSuffix, Value: "x.com", Action: a, Group: g}}}
	}

	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"набор в группу", set(ActionExit, "EXIT-RESI")},
		{"правило в группу", rule(ActionExit, "EXIT-DC")},
		{"группа с точкой и подчёркиванием", set(ActionExit, "EXIT-de_1.x")},
	} {
		if err := Validate(tc.cfg, cat, nil); err != nil {
			t.Errorf("%s: %v, ожидалось разрешение", tc.name, err)
		}
	}

	for _, tc := range []struct {
		name   string
		cfg    Config
		wantIn string
	}{
		{"набор в группу без имени", set(ActionExit, ""), "группа"},
		{"набор в группу без приставки", set(ActionExit, "PROXY"), "EXIT-"},
		{"группа у туннельного набора", set(ActionTunnel, "EXIT-RESI"), "EXIT-RESI"},
		{"правило в группу без имени", rule(ActionExit, ""), "группа"},
		{"правило с плохой группой", rule(ActionExit, "EXIT-a,b"), "EXIT-"},
		{"группа у прямого правила", rule(ActionDirect, "EXIT-DC"), "EXIT-DC"},
	} {
		err := Validate(tc.cfg, cat, nil)
		if err == nil {
			t.Errorf("%s: разрешено, ожидался отказ", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: в тексте %q нет %q", tc.name, err.Error(), tc.wantIn)
		}
	}
	// Плохая группа у правила — *RuleError с номером строки, как и прочие
	// отказы правил: панель подсвечивает строку.
	var re *RuleError
	if err := Validate(rule(ActionExit, "nope"), cat, nil); !errors.As(err, &re) || re.Index != 1 {
		t.Errorf("ошибка %v не *RuleError с номером 1", err)
	}
}

// TestExitGroups — список групп без дублей, в порядке первого появления:
// сначала правила, потом наборы — так же, как строки в файле.
func TestExitGroups(t *testing.T) {
	c := exitCfg()
	c.Sets = append(c.Sets, Set{Name: "anthropic", Action: ActionExit, Group: "EXIT-RESI"})
	if got, want := ExitGroups(c), []string{"EXIT-RESI", "EXIT-DC"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ExitGroups = %v, ожидалось %v", got, want)
	}
	if got := ExitGroups(Config{Policy: PolicyDirect}); len(got) != 0 {
		t.Errorf("без целей-групп ExitGroups = %v", got)
	}
}

func TestValidExitGroup(t *testing.T) {
	for name, want := range map[string]bool{
		"EXIT-RESI": true, "EXIT-DC": true, "EXIT-kz.1_a": true, "EXIT-" + strings.Repeat("A", 32): true,
		"EXIT-": false, "exit-resi": false, "PROXY": false, "BYPASS": false, "EXIT-a b": false,
		"EXIT-a>b": false, "EXIT-a@b": false, "EXIT-" + strings.Repeat("A", 33): false, "EXIT-кз": false,
	} {
		if got := ValidExitGroup(name); got != want {
			t.Errorf("ValidExitGroup(%q) = %v, ожидалось %v", name, got, want)
		}
	}
}
