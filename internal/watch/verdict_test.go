package watch

import (
	"testing"
	"time"

	"netmoded/internal/nikki"
)

// TestVerdictTable — таблица ADR-0043, строка за строкой.
func TestVerdictTable(t *testing.T) {
	cases := []struct {
		name string
		e    *entry
		want Verdict
	}{
		{"ничего не случилось", &entry{}, VerdictOK},
		{
			"недозвон в окне и без удачи после",
			&entry{lastDialAt: t0.Add(-30 * time.Second)},
			VerdictUnreachable,
		},
		{
			"недозвон старше окна",
			&entry{lastDialAt: t0.Add(-unreachableFor - time.Second)},
			VerdictOK,
		},
		{
			"после недозвона было удачное соединение",
			&entry{lastDialAt: t0.Add(-30 * time.Second), lastOKAt: t0.Add(-10 * time.Second)},
			VerdictOK,
		},
		{"молчит", &entry{silentTick: true}, VerdictSilent},
		{
			"и недозвон, и молчание — это прежде всего недозвон",
			&entry{lastDialAt: t0.Add(-time.Second), silentTick: true},
			VerdictUnreachable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdictOf(c.e, t0); got != c.want {
				t.Errorf("вердикт = %q, ожидался %q", got, c.want)
			}
		})
	}
}

// TestSilentNeedsAgeUploadAndDirect — «молчит» складывается из четырёх
// условий сразу, и каждое из них обязательно.
func TestSilentNeedsAgeUploadAndDirect(t *testing.T) {
	base := func(mut func(*nikki.Conn)) Verdict {
		c := conn("a", "x.example.com", "1.2.3.4", 443, 225, 0, t0.Add(-10*time.Second))
		mut(&c)
		tb := newTable()
		tb.applySnapshot(snap(c), devIP, time.Second, t0)
		return find(t, tb, c.Name(), t0).Verdict
	}
	if got := base(func(*nikki.Conn) {}); got != VerdictSilent {
		t.Errorf("запрос ушёл, ответа нет, идёт напрямую → %q, ожидалось silent", got)
	}
	if got := base(func(c *nikki.Conn) { c.Start = t0.Add(-time.Second) }); got != VerdictOK {
		t.Errorf("соединению секунда → %q; порог в 5 с и есть разница между «молчит» и «ещё думает»", got)
	}
	if got := base(func(c *nikki.Conn) { c.Upload = 0 }); got != VerdictOK {
		t.Errorf("запроса не было → %q; молчать в ответ на молчание не грех", got)
	}
	if got := base(func(c *nikki.Conn) { c.Download = 1 }); got != VerdictOK {
		t.Errorf("ответ пришёл → %q", got)
	}
	if got := base(func(c *nikki.Conn) { c.Chains = []string{"🇫🇯⚡Фокстрот", "PROXY", "BYPASS"} }); got != VerdictOK {
		t.Errorf("то же самое через туннель → %q; там это значит другое и лечится другим", got)
	}
}

// TestUnreachableKeepsChainOfFailedDial — цепочка неудачи лежит отдельным
// полем.
//
// У недозвона через туннель подсказка другая: правило не поможет, там либо
// мёртв узел, либо адресат закрыт с той стороны. Панель обязана узнавать
// этот случай по полю, а не искать квадратные скобки в имени узла.
func TestUnreachableKeepsChainOfFailedDial(t *testing.T) {
	tb := newTable()
	ev, kind := ParseLine("[TCP] dial BYPASS[🇫🇯⚡Фокстрот] (match RuleSet/nm-geosite-discord) " +
		devIP + ":50440 --> 162.159.136.232:443 error: dial tcp 162.159.136.232:443: i/o timeout")
	if kind != LineParsed {
		t.Fatalf("строка не разобрана")
	}
	tb.applyEvent(ev, t0)

	got := find(t, tb, "162.159.136.232", t0)
	if got.Verdict != VerdictUnreachable {
		t.Fatalf("вердикт = %q", got.Verdict)
	}
	if got.ErrorChain != "BYPASS[🇫🇯⚡Фокстрот]" {
		t.Errorf("цепочка неудачи = %q", got.ErrorChain)
	}
	if got.LastError == "" || got.DialErrors != 1 {
		t.Errorf("ошибка = %q, недозвонов %d", got.LastError, got.DialErrors)
	}
}

// TestUnreachableClearedBySuccess — удачное соединение снимает вердикт.
func TestUnreachableClearedBySuccess(t *testing.T) {
	tb := newTable()
	dial, _ := ParseLine("[TCP] dial DIRECT (match Match/) " + devIP +
		":50440 --> 194.221.250.50:80 error: dial tcp 194.221.250.50:80: i/o timeout")
	tb.applyEvent(dial, t0)
	if find(t, tb, "194.221.250.50", t0).Verdict != VerdictUnreachable {
		t.Fatal("недозвон не дал вердикта")
	}

	ok, _ := ParseLine("[TCP] " + devIP + ":50450 --> 194.221.250.50:80 match Match using DIRECT")
	tb.applyEvent(ok, t0.Add(time.Second))

	if got := find(t, tb, "194.221.250.50", t0.Add(time.Second)).Verdict; got != VerdictOK {
		t.Errorf("после удачного соединения вердикт = %q", got)
	}
}
