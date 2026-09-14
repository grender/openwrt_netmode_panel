// Package watch — наблюдатель трафика одного устройства (ADR-0043).
//
// Источников два, и они дополняют друг друга. Поток GET /logs ловит то, чего
// нет в снимке: соединения короче секунды и неудавшиеся дозвоны — дозвон, не
// удавшийся вовсе, соединением не стал. Снимок GET /connections даёт то, чего
// нет в журнале: байты, продолжительность и живость.
//
// Форма строк — чужая и без контракта: она снята с mihomo v1.19.27
// (docs/recon/nikki-watch.md, «Форма строк», порождается tunnel/tunnel.go)
// и держится этими тестами. Её смену владелец видит по счётчику unparsed, а
// не по пустому экрану, и никакой гейт этого не стережёт.
package watch

import (
	"net"
	"strconv"
	"strings"
)

// LineKind — что делать со строкой журнала.
//
// Корзины ТРИ, а не две, и это важно. При level=info mihomo пишет и свои
// собственные строки — «Start initial provider», «inbound … listening», — и
// пишет их после каждого перезапуска движка, который владелец запускает с
// этого же экрана. Считай мы их неразобранными, панель говорила бы «движок
// сменил формат» после первого же «Применить».
type LineKind int

const (
	// LineForeign — строка не про соединение: чужая подсистема или
	// собственный вывод движка. Молча отбрасывается.
	LineForeign LineKind = iota
	// LineParsed — строка про соединение, разобрана.
	LineParsed
	// LineUnknown — строка ПРО СОЕДИНЕНИЕ ([TCP]/[UDP]), но грамматике не
	// поддалась. Только она увеличивает unparsed.
	LineUnknown
)

// EventKind — какая из двух функций mihomo породила строку.
//
// logMetadata зовётся ПОСЛЕ удачного дозвона: TCP до цели (или до узла
// туннеля) установлен, рукопожатие прошло. logMetadataErr — вместо него.
// Отсюда два разных вердикта, и оба — факт, а не догадка.
type EventKind int

const (
	// EventMatch — соединение установлено.
	EventMatch EventKind = iota
	// EventDial — дозвон не удался.
	EventDial
)

// Event — разобранная строка журнала.
//
// Времени в строке нет вовсе (формат по умолчанию несёт только type и
// payload), поэтому At проставляет читатель по СВОИМ часам.
type Event struct {
	Kind    EventKind
	Net     string // tcp | udp
	SrcIP   string
	SrcPort int
	// Host — имя или адрес назначения: mihomo пишет домен, если он известен
	// из fake-ip или от sniffer, иначе адрес.
	Host string
	Port int
	// IsIP — имени нет, правило для такого адресата будет cidr.
	IsIP bool
	// Rule, Payload — тип правила и его значение: RuleSet +
	// nm-geosite-youtube, DomainSuffix + домен, Match + пусто.
	Rule, Payload string
	// Chain — цепочка в отображаемой форме: DIRECT либо группа[узел].
	Chain string
	// Err — текст ошибки дозвона, только у EventDial.
	Err string
}

// SourceKey — «адрес:порт источника». Ключ склейки со снимком: id соединения
// в журнал не попадает.
func (e Event) SourceKey() string {
	return e.SrcIP + ":" + strconv.Itoa(e.SrcPort)
}

const (
	arrow     = " --> "
	sepUsing  = " using "
	sepMatch  = " match "
	sepError  = " error: "
	sepDial   = "dial "
	sepParen  = " (match "
	noRuleTag = " doesn't match any rule"
)

// ParseLine разбирает payload строки журнала.
//
// Payload уже прошёл json.Unmarshal, поэтому стрелка в нём «-->»: в теле
// она приезжает как >, и разбирать её по сырому байту нельзя.
func ParseLine(payload string) (Event, LineKind) {
	var e Event
	rest, ok := cutNet(payload, &e)
	if !ok {
		return e, LineForeign
	}
	if strings.HasPrefix(rest, sepDial) {
		if parseDial(rest[len(sepDial):], &e) {
			return e, LineParsed
		}
		return e, LineUnknown
	}
	if parseMatch(rest, &e) {
		return e, LineParsed
	}
	return e, LineUnknown
}

// cutNet снимает «[TCP] » или «[UDP] ». Всё прочее в скобках — чужое:
// [DNS], [Process], [Rule] на уровне debug и собственные строки движка,
// у которых скобок нет вовсе.
func cutNet(p string, e *Event) (string, bool) {
	switch {
	case strings.HasPrefix(p, "[TCP] "):
		e.Net = "tcp"
	case strings.HasPrefix(p, "[UDP] "):
		e.Net = "udp"
	default:
		return "", false
	}
	return p[6:], true
}

// parseDial разбирает «PROXY (match TYPE/PAYLOAD) SRC --> DST error: ERR».
func parseDial(s string, e *Event) bool {
	e.Kind = EventDial

	// « error: » ищется ПЕРВЫМ вхождением: сам текст ошибки полон
	// двоеточий («dial tcp 194.221.250.50:80: i/o timeout»), но второго
	// « error: » в нём не бывает.
	i := strings.Index(s, sepError)
	if i < 0 {
		return false
	}
	e.Err = s[i+len(sepError):]
	head := s[:i]

	j := strings.Index(head, arrow)
	if j < 0 {
		return false
	}
	dst := head[j+len(arrow):]
	left := head[:j]

	// PROXY до « (match » — это цепочка, через которую дозванивались.
	k := strings.Index(left, sepParen)
	if k < 0 {
		return false
	}
	e.Chain = left[:k]
	rem := left[k+len(sepParen):]

	// «TYPE/PAYLOAD) SRC». Закрывающая скобка с пробелом — первая: значение
	// правила скобок не содержит, а суффикс процесса у SRC идёт после неё.
	m := strings.Index(rem, ") ")
	if m < 0 {
		return false
	}
	// Тип и значение делятся ПЕРВЫМ «/»: значение бывает пустым («Match/»)
	// и бывает само со слэшами («IPCIDR/10.0.0.0/8»).
	rule := rem[:m]
	if p := strings.IndexByte(rule, '/'); p >= 0 {
		e.Rule, e.Payload = rule[:p], rule[p+1:]
	} else {
		e.Rule = rule
	}

	return fillSrc(rem[m+2:], e) && fillDst(dst, e)
}

// parseMatch разбирает «SRC --> DST match RULE using CHAIN» и её родню.
func parseMatch(s string, e *Event) bool {
	e.Kind = EventMatch

	// « using » — ПОСЛЕДНЕЕ вхождение: имя узла приходит из подписки и
	// содержать может что угодно, в том числе слово using.
	i := strings.LastIndex(s, sepUsing)
	if i < 0 {
		return false
	}
	e.Chain = s[i+len(sepUsing):]
	head := s[:i]

	switch {
	case strings.HasSuffix(head, noRuleTag):
		// «doesn't match any rule» — правила нет вовсе, и искать в этой
		// фразе « match » значило бы принять «any rule» за правило.
		head = head[:len(head)-len(noRuleTag)]
	default:
		if j := strings.LastIndex(head, sepMatch); j >= 0 {
			rule := head[j+len(sepMatch):]
			head = head[:j]
			// «TYPE(PAYLOAD)» либо голый TYPE.
			if strings.HasSuffix(rule, ")") {
				if p := strings.IndexByte(rule, '('); p >= 0 {
					e.Rule, e.Payload = rule[:p], rule[p+1:len(rule)-1]
				} else {
					e.Rule = rule
				}
			} else {
				e.Rule = rule
			}
		}
		// Правила нет — это режим global или direct. Не ошибка строки.
	}

	j := strings.Index(head, arrow)
	if j < 0 {
		return false
	}
	return fillSrc(head[:j], e) && fillDst(head[j+len(arrow):], e)
}

// fillSrc разбирает «ip:port» или «ip:port(proc, uid=N)».
//
// Суффикс у TProxy на роутере пуст всегда (process и uid не известны), но
// исходник его печатает, и допускать его надо: иначе первая же сборка
// mihomo с другим входящим сделала бы все строки неразобранными.
func fillSrc(s string, e *Event) bool {
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	host, port, ok := splitHostPort(s)
	if !ok {
		return false
	}
	e.SrcIP, e.SrcPort = host, port
	return true
}

func fillDst(s string, e *Event) bool {
	host, port, ok := splitHostPort(s)
	if !ok {
		return false
	}
	e.Host, e.Port = host, port
	e.IsIP = net.ParseIP(host) != nil
	return true
}

// splitHostPort — «host:port» либо «[v6]:port».
func splitHostPort(s string) (string, int, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, false
	}
	return host, n, true
}
