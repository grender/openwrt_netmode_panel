package watch

import (
	"encoding/json"
	"strings"
	"testing"
)

// payloads достаёт payload'ы строк журнала из раздела фикстуры.
func payloads(t *testing.T, header string) []string {
	t.Helper()
	var out []string
	for _, l := range rawSection(t, header) {
		var line struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		if json.Unmarshal([]byte(l), &line) != nil {
			continue
		}
		out = append(out, line.Payload)
	}
	return out
}

// TestParseRecordedLines: все записанные строки разбираются.
//
// Это и есть пин формата: она чужая, без контракта, и привязана к mihomo
// v1.19.27. Сломается она у следующей версии — упадёт здесь, а не молча на
// роутере.
func TestParseRecordedLines(t *testing.T) {
	sections := []string{
		"виды строк: первые 2 каждого вида",
		"строки с RuleSet / DomainSuffix / не-DIRECT",
		"строки dial (ошибки)",
	}
	var total int
	for _, s := range sections {
		for _, p := range payloads(t, s) {
			ev, kind := ParseLine(p)
			if kind != LineParsed {
				t.Errorf("строка не разобрана (%v): %s", kind, p)
				continue
			}
			total++
			if ev.SrcIP == "" || ev.SrcPort == 0 {
				t.Errorf("источник не выделен в %q: %+v", p, ev)
			}
			if ev.Host == "" || ev.Port == 0 {
				t.Errorf("адресат не выделен в %q: %+v", p, ev)
			}
			if ev.Chain == "" {
				t.Errorf("цепочка не выделена в %q: %+v", p, ev)
			}
		}
	}
	if total < 10 {
		t.Fatalf("разобрано %d строк — фикстура читается не та", total)
	}
}

// TestParseRecordedDial: у записанного недозвона на месте всё, из чего
// складывается вердикт «не отвечает».
func TestParseRecordedDial(t *testing.T) {
	ps := payloads(t, "строки dial (ошибки)")
	if len(ps) == 0 {
		t.Fatal("в фикстуре нет строк dial")
	}
	ev, kind := ParseLine(ps[0])
	if kind != LineParsed || ev.Kind != EventDial {
		t.Fatalf("строка dial разобрана как %v/%v: %s", kind, ev.Kind, ps[0])
	}
	if ev.SrcIP != "192.168.9.219" {
		t.Errorf("источник = %q", ev.SrcIP)
	}
	if ev.Host != "194.221.250.50" || !ev.IsIP {
		t.Errorf("адресат = %q (isIP=%v), ожидался голый адрес", ev.Host, ev.IsIP)
	}
	if ev.Rule != "Match" || ev.Payload != "" {
		t.Errorf("правило = %q/%q; пустое значение после «/» — законная форма", ev.Rule, ev.Payload)
	}
	if ev.Chain != "DIRECT" {
		t.Errorf("цепочка = %q", ev.Chain)
	}
	if !strings.Contains(ev.Err, "i/o timeout") {
		t.Errorf("текст ошибки = %q", ev.Err)
	}
}

// TestParseRecordedRuleSet: имя набора и узел туннеля.
func TestParseRecordedRuleSet(t *testing.T) {
	for _, p := range payloads(t, "строки с RuleSet / DomainSuffix / не-DIRECT") {
		if !strings.Contains(p, "192.168.9.219") {
			continue
		}
		ev, kind := ParseLine(p)
		if kind != LineParsed {
			t.Fatalf("не разобрано: %s", p)
		}
		if ev.Rule != "RuleSet" || ev.Payload != "nm-geosite-youtube" {
			t.Errorf("правило = %q/%q", ev.Rule, ev.Payload)
		}
		if ev.Chain != "BYPASS[🇫🇷⚡Франция]" {
			t.Errorf("цепочка = %q — она обязана совпасть с nikki.ChainString снимка", ev.Chain)
		}
		return
	}
	t.Fatal("в разделе нет строки устройства 192.168.9.219")
}

// TestParseFormsNotSeenOnRouter — формы из ИСХОДНИКА mihomo, которых за обе
// части пробы не случилось. Взяты из tunnel/tunnel.go v1.19.27 по разбору в
// docs/recon/nikki-watch.md, и это не наблюдение, а чтение кода: если
// разборщик их не держит, первый же UDP на роутере станет «не разобрано».
func TestParseFormsNotSeenOnRouter(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		kind    LineKind
		want    Event
		wantErr string
	}{
		{
			name: "UDP match",
			in:   "[UDP] 192.168.9.219:51820 --> stun.l.google.com:19302 match Match using DIRECT",
			kind: LineParsed,
			want: Event{Kind: EventMatch, Net: "udp", SrcIP: "192.168.9.219", SrcPort: 51820,
				Host: "stun.l.google.com", Port: 19302, Rule: "Match", Chain: "DIRECT"},
		},
		{
			name: "суффикс процесса у источника",
			in:   "[TCP] 192.168.9.219:50446(firefox, uid=1000) --> www.google.com:443 match Match using DIRECT",
			kind: LineParsed,
			want: Event{Kind: EventMatch, Net: "tcp", SrcIP: "192.168.9.219", SrcPort: 50446,
				Host: "www.google.com", Port: 443, Rule: "Match", Chain: "DIRECT"},
		},
		{
			name: "IPv6 в скобках",
			in:   "[TCP] [fd00::2]:5000 --> [2606:4700:4700::1111]:443 match Match using DIRECT",
			kind: LineParsed,
			want: Event{Kind: EventMatch, Net: "tcp", SrcIP: "fd00::2", SrcPort: 5000,
				Host: "2606:4700:4700::1111", Port: 443, IsIP: true, Rule: "Match", Chain: "DIRECT"},
		},
		{
			name: "значение правила со слэшами",
			in:   "[TCP] dial DIRECT (match IPCIDR/10.0.0.0/8) 192.168.9.219:50440 --> 10.1.2.3:80 error: dial tcp 10.1.2.3:80: i/o timeout",
			kind: LineParsed,
			want: Event{Kind: EventDial, Net: "tcp", SrcIP: "192.168.9.219", SrcPort: 50440,
				Host: "10.1.2.3", Port: 80, IsIP: true, Rule: "IPCIDR", Payload: "10.0.0.0/8", Chain: "DIRECT"},
			wantErr: "dial tcp 10.1.2.3:80: i/o timeout",
		},
		{
			name: "правило со значением в скобках",
			in:   "[TCP] 192.168.9.219:50526 --> www.youtube.com:443 match DomainSuffix(youtube.com) using BYPASS[🇫🇷⚡Франция]",
			kind: LineParsed,
			want: Event{Kind: EventMatch, Net: "tcp", SrcIP: "192.168.9.219", SrcPort: 50526,
				Host: "www.youtube.com", Port: 443, Rule: "DomainSuffix", Payload: "youtube.com",
				Chain: "BYPASS[🇫🇷⚡Франция]"},
		},
		{
			name: "нет совпавшего правила",
			in:   "[TCP] 192.168.9.219:50446 --> www.google.com:443 doesn't match any rule using DIRECT",
			kind: LineParsed,
			want: Event{Kind: EventMatch, Net: "tcp", SrcIP: "192.168.9.219", SrcPort: 50446,
				Host: "www.google.com", Port: 443, Chain: "DIRECT"},
		},
		{
			name: "режим global без правила",
			in:   "[TCP] 192.168.9.219:50446 --> www.google.com:443 using GLOBAL",
			kind: LineParsed,
			want: Event{Kind: EventMatch, Net: "tcp", SrcIP: "192.168.9.219", SrcPort: 50446,
				Host: "www.google.com", Port: 443, Chain: "GLOBAL"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, kind := ParseLine(c.in)
			if kind != c.kind {
				t.Fatalf("вид строки = %v, ожидался %v", kind, c.kind)
			}
			got := ev
			got.Err = ""
			if got != c.want {
				t.Errorf("разобрано %+v\nожидалось  %+v", got, c.want)
			}
			if ev.Err != c.wantErr {
				t.Errorf("ошибка = %q, ожидалась %q", ev.Err, c.wantErr)
			}
		})
	}
}

// TestUnparsedVsForeign: чужое отбрасывается молча, своё непонятное —
// считается.
//
// Иначе «часть событий не разобрана» загоралось бы после каждого «Применить»:
// движок перезапускается и пишет свои стартовые строки тем же уровнем info.
func TestUnparsedVsForeign(t *testing.T) {
	foreign := []string{
		"[DNS] www.google.com --> 198.18.0.63",
		"[Process] find process firefox",
		"[Rule] update rule-set nm-geosite-youtube",
		"Start initial provider nm-geosite-youtube",
		"inbound tproxy://0.0.0.0:7892 listening at: [::]:7892",
		"",
	}
	for _, p := range foreign {
		if _, kind := ParseLine(p); kind != LineForeign {
			t.Errorf("строка %q принята за свою (%v)", p, kind)
		}
	}
	unknown := []string{
		"[TCP] что-то совсем другое",
		"[UDP] 192.168.9.219 --> без порта match Match using DIRECT",
		"[TCP] 192.168.9.219:50446 --> www.google.com:443 match Match",
	}
	for _, p := range unknown {
		if _, kind := ParseLine(p); kind != LineUnknown {
			t.Errorf("строка %q не попала в неразобранные (%v)", p, kind)
		}
	}
}

// TestForeignSourceIsParsedNotDropped: трафик самого роутера разбирается как
// обычная строка, а отсекает его фильтр по адресу устройства — иначе он
// попал бы в «не разобрано» и выглядел бы как смена формата.
func TestForeignSourceIsParsedNotDropped(t *testing.T) {
	const p = "[TCP] 192.168.0.234:37984 --> api.anthropic.com:443 match RuleSet(nm-geosite-anthropic) using BYPASS[🇫🇷⚡Франция]"
	ev, kind := ParseLine(p)
	if kind != LineParsed {
		t.Fatalf("вид строки = %v", kind)
	}
	if ev.SrcIP != "192.168.0.234" {
		t.Errorf("источник = %q", ev.SrcIP)
	}
}
