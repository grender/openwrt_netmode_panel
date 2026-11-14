package mixin

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"netmoded/internal/exits"
)

const (
	hopDE = "🇩🇪⚡Германия"
	hopRU = "🇷🇺💳Россия"
	hopTR = "🇹🇷⚪Турция (БС-3)☁️"
)

func hopBase() Config {
	return Config{Policy: PolicyDirect, Download: DownloadDirect,
		Sets: []Set{{Name: "openai", Action: ActionExit, Group: "EXIT-RESI"}}}
}

// Без пула хопов и без своих нод файл байт-в-байт прежний: обновление
// демона не вправе переписать mixin.yaml тем, кто цепочек не заводил.
func TestHopsDefaultChangesNothing(t *testing.T) {
	base := hopBase()
	with := base
	with.Hops = HopConfig{}
	with.SkipSNI = nil
	if got, want := string(Render(with)), string(Render(base)); got != want {
		t.Fatalf("умолчание изменило файл:\n%s\nожидалось:\n%s", got, want)
	}
	for _, f := range []string{hopStateField, hopPoolField, skipSNIField} {
		if strings.Contains(string(Render(with)), f) {
			t.Errorf("в состоянии поле %q при умолчательном выборе", f)
		}
	}
}

// Группа на хоп: имя — exits.HopGroupName (на него ссылаются цепочки в
// exits.yaml), фильтр — ровно этот узел провайдера sub.
func TestHopsRenderGroups(t *testing.T) {
	c := hopBase()
	c.Hops = HopConfig{Mode: AutoAllow, Nodes: []string{hopDE, hopTR}, Pool: []string{hopDE, hopTR}}
	out := string(Render(c))
	if !strings.Contains(out, "nikki-proxy-groups:\n") {
		t.Fatalf("нет nikki-proxy-groups:\n%s", out)
	}
	for _, h := range []string{hopDE, hopTR} {
		// empty-fallback REJECT: хоп пропал из подписки — группа пуста, а пустая
		// группа у mihomo это COMPATIBLE = DIRECT, и цепочка молча пошла бы на
		// VPS без хопа (замерено на VM). Kill switch: отказ, а не другой путь.
		line := "  - {name: '" + exits.HopGroupName(h) + "', type: select, use: ['" + exits.SubProvider + "'], filter: '" +
			"^" + regexp.QuoteMeta(h) + "$" + "', empty-fallback: 'REJECT'}\n"
		if !strings.Contains(out, line) {
			t.Errorf("нет строки группы для %q:\n%s\nв файле:\n%s", h, line, out)
		}
	}
}

func TestHopsSkipSNI(t *testing.T) {
	c := hopBase()
	c.SkipSNI = []string{"krisha.kz", "www.example.kg"}
	out := string(Render(c))
	if !strings.Contains(out, "sniffer:\n  skip-domain:\n    - 'krisha.kz'\n    - 'www.example.kg'\n") {
		t.Fatalf("нет sniffer.skip-domain:\n%s", out)
	}
}

func TestHopsRoundTrip(t *testing.T) {
	c := hopBase()
	c.Hops = HopConfig{Mode: AutoDeny, Nodes: []string{"узел с ' апострофом"}, Pool: []string{hopDE, hopRU, hopTR}}
	c.SkipSNI = []string{"krisha.kz"}
	got, foreign, err := Parse(Render(c))
	if err != nil || foreign {
		t.Fatalf("Parse: %v foreign=%v", err, foreign)
	}
	if got.Hops.Mode != c.Hops.Mode || strings.Join(got.Hops.Nodes, "|") != strings.Join(c.Hops.Nodes, "|") ||
		strings.Join(got.Hops.Pool, "|") != strings.Join(c.Hops.Pool, "|") {
		t.Errorf("пул: %+v, ждали %+v", got.Hops, c.Hops)
	}
	if strings.Join(got.SkipSNI, ",") != "krisha.kz" {
		t.Errorf("SkipSNI: %v", got.SkipSNI)
	}
	// Наборы и пул — независимые секции одного файла.
	if len(got.Sets) != 1 || got.Sets[0].Group != "EXIT-RESI" {
		t.Errorf("наборы потерялись: %+v", got.Sets)
	}
}

// Профильная политика наборов пул хопов не отменяет — как и авто-пул.
func TestHopsSurviveProfilePolicy(t *testing.T) {
	c := Config{Policy: PolicyProfile, Hops: HopConfig{Mode: AutoAllow, Nodes: []string{hopDE}, Pool: []string{hopDE}}}
	got, _, err := Parse(Render(c))
	if err != nil || len(got.Hops.Pool) != 1 {
		t.Fatalf("Parse: %v, пул %v", err, got.Hops.Pool)
	}
}

// Строка группы, вычеркнутая руками, — порча: иначе цепочка в exits.yaml
// ссылалась бы на исчезнувшую группу и молча отказывала.
func TestHopsParseDetectsEditedBody(t *testing.T) {
	c := hopBase()
	c.Hops = HopConfig{Mode: AutoAllow, Nodes: []string{hopDE, hopRU}, Pool: []string{hopDE, hopRU}}
	out := string(Render(c))
	i := strings.Index(out, "  - {name: 'nm-hop-")
	j := strings.Index(out[i:], "\n")
	broken := out[:i] + out[i+j+1:]
	if _, _, err := Parse([]byte(broken)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("вычеркнутая группа не замечена: %v", err)
	}

	c2 := hopBase()
	c2.SkipSNI = []string{"krisha.kz"}
	out2 := strings.Replace(string(Render(c2)), "    - 'krisha.kz'\n", "", 1)
	if _, _, err := Parse([]byte(out2)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("вычеркнутый skip-domain не замечен: %v", err)
	}
}

func TestValidateHops(t *testing.T) {
	ok := HopConfig{Mode: AutoAllow, Nodes: []string{hopDE}, Pool: []string{hopDE}}
	if err := ValidateHops(ok); err != nil {
		t.Fatalf("годный пул отвергнут: %v", err)
	}
	if err := ValidateHops(HopConfig{}); err != nil {
		t.Fatalf("пустой пул (цепочек нет) отвергнут: %v", err)
	}
	many := make([]string, MaxHopPool+1)
	for i := range many {
		many[i] = strings.Repeat("x", i+1)
	}
	for name, h := range map[string]HopConfig{
		"режим":                 {Mode: "любой", Pool: []string{hopDE}},
		"пул больше потолка":    {Mode: AutoDeny, Pool: many},
		"повтор в пуле":         {Mode: AutoAllow, Nodes: []string{hopDE}, Pool: []string{hopDE, hopDE}},
		"битое имя":             {Mode: AutoAllow, Nodes: []string{" x"}, Pool: []string{" x"}},
		"allow без узлов":       {Mode: AutoAllow, Pool: []string{hopDE}},
		"пул шире выбора":       {Mode: AutoAllow, Nodes: []string{hopDE}, Pool: []string{hopDE, hopRU}},
		"исключённый в пуле":    {Mode: AutoDeny, Nodes: []string{hopRU}, Pool: []string{hopDE, hopRU}},
		"режим задан, пул пуст": {Mode: AutoDeny, Pool: nil},
	} {
		if err := ValidateHops(h); err == nil {
			t.Errorf("%s: принят %+v", name, h)
		}
	}
}

func TestValidateSkipSNI(t *testing.T) {
	if err := ValidateSkipSNI([]string{"krisha.kz"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{""}, {"a b"}, {"krisha.kz", "krisha.kz"}, {"x'y"}} {
		if err := ValidateSkipSNI(bad); err == nil {
			t.Errorf("принят %q", bad)
		}
	}
}

// Отпечаток свой: правка пула не должна отбивать If-Match наборов и
// авто-пула, и наоборот.
func TestHopFingerprintIndependent(t *testing.T) {
	a := HopConfig{Mode: AutoAllow, Nodes: []string{hopDE}, Pool: []string{hopDE}}
	b := HopConfig{Mode: AutoAllow, Nodes: []string{hopRU}, Pool: []string{hopRU}}
	if HopFingerprint(a) == HopFingerprint(b) {
		t.Fatal("разные пулы — один отпечаток")
	}
	c1, c2 := hopBase(), hopBase()
	c2.Hops = a
	if Fingerprint(c1) != Fingerprint(c2) {
		t.Fatal("пул хопов изменил отпечаток наборов")
	}
}

// Повторители — по одной группе на ноду с dc (exits.FollowGroup): select из
// групп-однохопников пула и REJECT. Пишутся и без пула — тогда [REJECT],
// и цепочка «⇢ авто» отказывает (kill switch).
func TestFollowGroupsRendered(t *testing.T) {
	c := hopBase()
	c.Hops = HopConfig{Mode: AutoAllow, Nodes: []string{hopDE, hopRU}, Pool: []string{hopDE, hopRU}}
	c.FollowNodes = []string{"kz-nqz-exit-01"}
	out := string(Render(c))
	line := "  - {name: '" + exits.FollowGroupName("kz-nqz-exit-01") + "', type: select, proxies: ['" +
		exits.HopGroupName(hopDE) + "', '" + exits.HopGroupName(hopRU) + "', 'REJECT']}\n"
	if !strings.Contains(out, line) {
		t.Fatalf("нет повторителя:\n%s\nв файле:\n%s", line, out)
	}

	off := hopBase()
	off.FollowNodes = []string{"kz-nqz-exit-01"}
	if !strings.Contains(string(Render(off)), "proxies: ['REJECT']}") {
		t.Fatalf("без пула повторитель не [REJECT]:\n%s", Render(off))
	}
}

func TestFollowRoundTripAndCorruption(t *testing.T) {
	c := hopBase()
	c.Hops = HopConfig{Mode: AutoAllow, Nodes: []string{hopDE}, Pool: []string{hopDE}}
	c.FollowNodes = []string{"kz-nqz-exit-01", "kz-ala-exit-02"}
	got, _, err := Parse(Render(c))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.Join(got.FollowNodes, ",") != "kz-nqz-exit-01,kz-ala-exit-02" {
		t.Errorf("FollowNodes %v", got.FollowNodes)
	}
	out := string(Render(c))
	i := strings.Index(out, "  - {name: 'nm-follow-")
	j := strings.Index(out[i:], "\n")
	if _, _, err := Parse([]byte(out[:i] + out[i+j+1:])); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("вычеркнутый повторитель не замечен: %v", err)
	}
	if ValidateFollowNodes([]string{"KZ bad"}) == nil {
		t.Error("битое имя ноды принято")
	}
}

// Потолок поднят: хоп выбирается по дешёвым проверкам dc, resi не
// проверяется вовсе — 40 хопов не нагружают ISP-прокси.
func TestMaxHopPoolRaised(t *testing.T) {
	if MaxHopPool < 40 {
		t.Fatalf("MaxHopPool = %d", MaxHopPool)
	}
}
