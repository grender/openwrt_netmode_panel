// Package rulesets — выбор наборов geosite/geoip: как он превращается в
// /etc/nikki/mixin.yaml и как читается обратно из того же файла.
//
// Почему состояние живёт в собственном файле демона, а не рядом с ним.
// Наборы должны работать и тогда, когда демона нет вовсе: после перезагрузки
// роутера nikki стартует сам и склеивает свой профиль с mixin.yaml — выбор
// владельца переживает и перезагрузку, и остановленный netmoded. Значит файл
// писать всё равно придётся. А раз он пишется, второе хранилище (секции UCI,
// свой json в /etc) было бы ВТОРОЙ правдой о том же самом: любая правка
// mixin.yaml по ssh, любая оборванная запись — и панель показывала бы одно, а
// mihomo применял другое, причём молча. Поэтому хранилище одно, и оно же
// источник ответа панели: третья строка шапки — машинное состояние
// (политика, откуда качать, наборы с признаком подсетей), а всё тело файла
// порождается из неё. Читать обратно тело не нужно — только сверить, что
// число правил ему соответствует (иначе файл правили руками, см. ErrCorrupt).
//
// Почему GEOIP,PRIVATE стоит перед MATCH. Правила mihomo — это список «до
// первого совпадения», и последним в нём стоит MATCH, который забирает
// вообще всё. При политике «кроме этих» хвост — MATCH,BYPASS, то есть «всё
// остальное в туннель»; без строки GEOIP,PRIVATE,DIRECT,no-resolve под
// «остальное» попадёт и локальная сеть — 192.168.0.0/16, 10.0.0.0/8, — и
// роутер вместе с панелью перестанет быть доступен с домашних машин. Поэтому
// строка пишется ВСЕГДА, при обеих политиках: при «только эти» она ничего не
// меняет, а при «кроме» спасает LAN. Отдельного выключателя для неё нет
// намеренно — это не настройка, а условие работоспособности.
//
// Почему свои правила владельца живут здесь же, а не в профиле. Профиль
// main.yml склеивается ПОСЛЕ нашего блока, а наш блок кончается MATCH, — до
// правил профиля очередь не доходит никогда. Домен, который владелец вписал
// в профиль руками, молча идёт по нашему хвосту; так и не открывался
// paddock.worldsimseries.com (ADR-0040). Поэтому свои домены и подсети
// хранятся в том же файле и стоят ПЕРВЫМИ в рубрике: воля владельца
// побеждает набор, в котором тот же домен есть. Группа у правила берётся из
// его действия, а не из политики — «этот домен напрямую» значит одно и то же
// при обеих политиках.
package mixin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"

	"netmoded/internal/geosite"
	"netmoded/internal/nikki"
)

// Policy — что делать с выбранными наборами.
type Policy string

const (
	// PolicyProfile — наборов нет, правила целиком из профиля nikki.
	PolicyProfile Policy = "profile"
	// PolicyDirect — остальной трафик (не совпавший ни с правилом, ни с
	// набором) идёт напрямую: хвост MATCH,DIRECT.
	PolicyDirect Policy = "direct"
	// PolicyTunnel — остальной трафик идёт в туннель: хвост MATCH,BYPASS.
	PolicyTunnel Policy = "tunnel"

	// legacyOnly и legacyExcept — политики до ADR-0041, когда направление
	// всем наборам задавалось разом. Читаются (старый файл на диске обязан
	// открыться), не пишутся никогда: only = наборы в туннель, остальное
	// напрямую; except = наборы напрямую, остальное в туннель.
	legacyOnly   Policy = "only"
	legacyExcept Policy = "except"
)

// Download — откуда mihomo качает сами файлы правил.
type Download string

const (
	// DownloadDirect — напрямую, мимо туннеля.
	DownloadDirect Download = "direct"
	// DownloadTunnel — через туннель: у провайдера появляется proxy: PROXY.
	// Нужно там, где raw.githubusercontent.com недоступен у провайдера.
	DownloadTunnel Download = "tunnel"
)

// Set — один выбранный набор.
//
// IP не выбирается владельцем: признак ставит каталог (Resolve), если имя
// есть ещё и в дереве geoip. Он хранится в файле, чтобы при чтении обратно
// каталог был не нужен — иначе панель без интернета не смогла бы показать
// применённое.
type Set struct {
	Name string `json:"name"`
	IP   bool   `json:"ip"`
	// Action — направление набора: в туннель или напрямую. Своё у
	// каждого набора, а не одно на всех (ADR-0041): «банки напрямую, а
	// YouTube в туннель» в одном списке иначе не выразить. Обязательно —
	// Parse всегда его заполняет, Validate пустое отбивает.
	Action RuleAction `json:"action"`
}

// RuleKind — вид своего правила: по чему сопоставлять.
type RuleKind string

const (
	// RuleSuffix — домен и все его поддомены (DOMAIN-SUFFIX).
	RuleSuffix RuleKind = "suffix"
	// RuleDomain — ровно этот хост (DOMAIN).
	RuleDomain RuleKind = "domain"
	// RuleCIDR — подсеть (IP-CIDR / IP-CIDR6), всегда с no-resolve: правило
	// про адреса не должно заставлять mihomo резолвить каждый домен.
	RuleCIDR RuleKind = "cidr"
)

// RuleAction — куда отправить совпавшее.
type RuleAction string

const (
	// ActionTunnel — в туннель, группа TunnelGroup.
	ActionTunnel RuleAction = "tunnel"
	// ActionDirect — напрямую, мимо туннеля.
	ActionDirect RuleAction = "direct"
)

// Rule — одно своё правило владельца.
//
// Значение после проверки состоит только из [a-z0-9.:/-]: так ему безопасно
// и в одинарных кавычках YAML, и в строке состояния, где списки разделяются
// запятой и точкой с запятой, а поля — пробелом.
type Rule struct {
	Kind   RuleKind   `json:"kind"`
	Value  string     `json:"value"`
	Action RuleAction `json:"action"`
	// Comment — пояснение для человека: «серверы WSS, Hetzner». Хранится
	// в строке состояния (url.QueryEscape, хвост «|…» токена) и пишется
	// в тело строкой «# …» над правилом — читать mixin.yaml по ssh иначе
	// нельзя: голый 157.90.0.0/16 через месяц ничего не значит. Обратно
	// из тела не читается, как и всё тело. Пустой — хвоста нет, и токен
	// байт-в-байт прежний: отпечатки правил без комментария не меняются.
	Comment string `json:"comment"`
}

// MaxRules — потолок своих правил. Не техническое ограничение, а защита от
// того, чтобы вкладка превратилась в текстовый редактор профиля.
const MaxRules = 64

// MaxCommentRunes — потолок комментария. Одна строка над правилом, а не
// абзац: длиннее — уже не пометка, а документ, которому место не здесь.
const MaxCommentRunes = 80

// Config — весь выбор целиком. Порядок Sets и Rules значим: он же порядок
// правил в файле, а у mihomo побеждает первое совпадение. Policy — только
// про хвост MATCH, то есть про остальной трафик.
type Config struct {
	Policy   Policy
	Download Download
	Sets     []Set
	Rules    []Rule
	// Auto — вторая, независимая секция того же файла: чем наполняется
	// группа AUTO. С наборами её роднит только файл, поэтому у неё свой
	// отпечаток и свои проверки (autopool.go).
	Auto AutoConfig
}

const (
	// ProviderSitePrefix — приставка имени провайдера доменов.
	//
	// Своя приставка, а не голое имя набора: в config.yaml наши провайдеры
	// лежат рядом с провайдерами профиля владельца, и совпадение имён
	// затёрло бы чужое. По ней же видно из ssh, что провайдер наш.
	ProviderSitePrefix = "nm-geosite-"
	// ProviderIPPrefix — приставка имени провайдера подсетей.
	ProviderIPPrefix = "nm-geoip-"
	// TunnelGroup — группа mihomo, означающая «в туннель».
	TunnelGroup = "BYPASS"
	// DownloadProxy — через что качать при download=tunnel.
	DownloadProxy = "PROXY"
	// Interval — как часто mihomo перекачивает .mrs, секунды.
	Interval = "86400"

	// directGroup — «мимо туннеля». Не константа Nikki, а имя встроенной
	// политики mihomo, поэтому рядом с TunnelGroup, но не экспортируется.
	directGroup = "DIRECT"

	// headerMark — по нему файл опознаётся как наш. ПЕРВАЯ строка должна
	// начинаться с него: комментарий в середине чужого файла ничего не
	// доказывает, а вот шапка — доказывает.
	headerMark = "# netmoded:"
	// stateMark — строка машинного состояния.
	stateMark = "# netmoded-rulesets:"

	headerLine1 = headerMark + " файл пишет панель (раздел «Узлы Nikki» → «Что в туннель · наборы»)."
	headerLine2 = "# Не правьте руками — при следующем применении он переписывается целиком."

	// ruleSiteLinePrefix — начало строки правила доменного набора. По нему
	// Parse считает правила, сверяя их число с состоянием.
	ruleSiteLinePrefix = "  - 'RULE-SET," + ProviderSitePrefix

	// Начала строк своих правил — по ним Parse считает их так же, как
	// наборы. Запятая в конце обязательна: без неё DOMAIN совпал бы и с
	// DOMAIN-SUFFIX.
	customSuffixPrefix = "  - 'DOMAIN-SUFFIX,"
	customDomainPrefix = "  - 'DOMAIN,"
	customCIDRPrefix   = "  - 'IP-CIDR,"
	customCIDR6Prefix  = "  - 'IP-CIDR6,"
)

// RuleError — своё правило не принято.
//
// Тип, а не текст, по той же причине, что и UnknownSetsError: панели нужен
// номер строки, чтобы подсветить её, а не искать значение в сообщении.
// Index — с единицы: он уходит в текст для владельца, а не в код.
type RuleError struct {
	Index  int
	Rule   Rule
	Reason string
}

func (e *RuleError) Error() string {
	return fmt.Sprintf("rulesets: правило %d (%s %s): %s", e.Index, e.Rule.Kind, e.Rule.Value, e.Reason)
}

// ErrCorrupt — файл наш (шапка на месте), но его содержимому верить нельзя.
var ErrCorrupt = errors.New("rulesets: mixin.yaml повреждён — примените наборы заново")

// ErrNoCatalog — среди имён есть новые, а сверить их не с чем.
//
// Отдельный сентинел, а не «имя неизвестно»: причина у отказа противоположная
// (не владелец ошибся, а мы не смогли), и наверх она уезжает как 503, а не 400.
var ErrNoCatalog = errors.New("rulesets: каталог наборов не загружен — новые имена нечем проверить")

// UnknownSetsError — имена, которых нет ни в каталоге, ни среди применённых.
//
// Тип, а не текст, потому что перечень имён нужен обработчику PUT целиком:
// панель подсвечивает именно эти чипы, и вытаскивать их разбором строки было
// бы издевательством.
type UnknownSetsError struct {
	Names []string
}

func (e *UnknownSetsError) Error() string {
	return "rulesets: таких наборов нет в каталоге: " + strings.Join(e.Names, ", ")
}

// ProviderNames — имена провайдеров, которые набор порождает в config.yaml.
//
// Одно имя без подсетей, два с ними. Сверка «загрузился ли набор» идёт
// именно по этому списку: набор с подсетями, у которого скачались только
// домены, загруженным не считается.
func ProviderNames(s Set) []string {
	if s.IP {
		return []string{ProviderSitePrefix + s.Name, ProviderIPPrefix + s.Name}
	}
	return []string{ProviderSitePrefix + s.Name}
}

// tailGroup — группа хвоста MATCH: куда идёт остальной трафик.
func tailGroup(p Policy) string {
	if p == PolicyTunnel {
		return TunnelGroup
	}
	return directGroup
}

// groupFor — группа своего правила или набора. Из действия, не из политики.
func groupFor(a RuleAction) string {
	if a == ActionTunnel {
		return TunnelGroup
	}
	return directGroup
}

// ruleLine — строка своего правила в рубрике nikki-rules.
//
// Значение считается проверенным (Validate или parseRules): Render, как и с
// именами наборов, не проверяет вход второй раз.
func ruleLine(r Rule) string {
	switch r.Kind {
	case RuleSuffix:
		return fmt.Sprintf("%s%s,%s'", customSuffixPrefix, r.Value, groupFor(r.Action))
	case RuleDomain:
		return fmt.Sprintf("%s%s,%s'", customDomainPrefix, r.Value, groupFor(r.Action))
	default:
		prefix := customCIDRPrefix
		if p, err := netip.ParsePrefix(r.Value); err == nil && p.Addr().Is6() {
			prefix = customCIDR6Prefix
		}
		return fmt.Sprintf("%s%s,%s,no-resolve'", prefix, r.Value, groupFor(r.Action))
	}
}

// Render собирает файл целиком. Детерминирован: одинаковый Config даёт
// одинаковые байты — на этом держится и golden-тест, и отпечаток.
func Render(c Config) []byte {
	var b strings.Builder

	b.WriteString(headerLine1 + "\n")
	b.WriteString(headerLine2 + "\n")
	b.WriteString(stateLine(c) + "\n")

	// Авто-пул — первым: рубрика proxy-providers логически старше
	// rule-providers, а главное, она обязана пережить ранний выход ниже.
	// При умолчательном пуле строка пустая, и файл байт-в-байт прежний.
	b.WriteString(autoBody(c.Auto))

	if c.Policy == PolicyProfile {
		// Профиль — это «нас тут нет»: ни провайдеров правил, ни правил,
		// чтобы склейка yq ничего не добавила к правилам профиля. Про
		// авто-пул это не говорит ничего: он уже записан выше.
		return []byte(b.String())
	}

	// Рубрика провайдеров пишется, только если наборы есть: пустая
	// «rule-providers:» — это rule-providers: null, и склейка затёрла бы
	// провайдеров профиля владельца.
	if len(c.Sets) > 0 {
		b.WriteString("rule-providers:\n")
		for _, s := range c.Sets {
			writeProvider(&b, c.Download, ProviderSitePrefix+s.Name, "domain", geosite.FileURL(geosite.KindSite, s.Name))
			if s.IP {
				writeProvider(&b, c.Download, ProviderIPPrefix+s.Name, "ipcidr", geosite.FileURL(geosite.KindIP, s.Name))
			}
		}
	}

	b.WriteString("nikki-rules:\n")
	// Свои правила — первыми: см. документацию пакета. Комментарий —
	// строкой над правилом, как есть: это YAML-комментарий для человека,
	// yq его не трогает, а Parse не считает (не начинается с «  - '»).
	for _, r := range c.Rules {
		if r.Comment != "" {
			b.WriteString("  # " + r.Comment + "\n")
		}
		b.WriteString(ruleLine(r) + "\n")
	}
	for _, s := range c.Sets {
		// Порядок внутри набора: сначала домены, потом подсети. Правило по
		// подсетям идёт с no-resolve — иначе mihomo резолвил бы каждый
		// домен, чтобы сверить адрес, и платил бы за это задержкой DNS.
		fmt.Fprintf(&b, "  - 'RULE-SET,%s%s,%s'\n", ProviderSitePrefix, s.Name, groupFor(s.Action))
		if s.IP {
			fmt.Fprintf(&b, "  - 'RULE-SET,%s%s,%s,no-resolve'\n", ProviderIPPrefix, s.Name, groupFor(s.Action))
		}
	}
	// См. документацию пакета: без этой строки политика «кроме» уводит в
	// туннель локальную сеть.
	b.WriteString("  - 'GEOIP,PRIVATE," + directGroup + ",no-resolve'\n")
	b.WriteString("  - 'MATCH," + tailGroup(c.Policy) + "'\n")

	return []byte(b.String())
}

// writeProvider — один блок rule-providers.
func writeProvider(b *strings.Builder, d Download, name, behavior, url string) {
	fmt.Fprintf(b, "  %s:\n", name)
	b.WriteString("    type: http\n")
	fmt.Fprintf(b, "    behavior: %s\n", behavior)
	b.WriteString("    format: mrs\n")
	fmt.Fprintf(b, "    url: %s\n", url)
	fmt.Fprintf(b, "    path: ./rules/%s.mrs\n", name)
	b.WriteString("    interval: " + Interval + "\n")
	if d == DownloadTunnel {
		b.WriteString("    proxy: " + DownloadProxy + "\n")
	}
}

// stateLine — третья строка шапки, машинное состояние.
//
// У profile пишется только политика: остальные поля там ничего не значат, а
// download=direct рядом с «правила из профиля» читался бы как обещание
// что-то качать.
func stateLine(c Config) string {
	if c.Policy == PolicyProfile {
		// Авто-пул от политики наборов не зависит: «правила из профиля»
		// и «чем наполнять AUTO» — разные решения, и профильная политика
		// не вправе отменять второе.
		return stateMark + " policy=" + string(PolicyProfile) + autoStateFields(c.Auto)
	}
	names := make([]string, 0, len(c.Sets))
	for _, s := range c.Sets {
		names = append(names, setToken(s))
	}
	// Поле sets пишется даже пустым: его отсутствие значило бы «строка
	// оборвалась», а пустое значение — «выбрано ноль наборов», и это
	// разные вещи.
	line := fmt.Sprintf("%s policy=%s download=%s sets=%s",
		stateMark, c.Policy, c.Download, strings.Join(names, ","))
	// А поле rules — наоборот, только при наличии правил. Демон прежней
	// версии незнакомое поле считает порчей (parseState) — и это желаемое
	// поведение для файла С правилами: громкий отказ вместо молчаливой
	// потери. Но файл БЕЗ правил он обязан читать как раньше, иначе
	// обновление демона ломало бы всем выбор, где своих правил и не было.
	if len(c.Rules) > 0 {
		tokens := make([]string, 0, len(c.Rules))
		for _, r := range c.Rules {
			tokens = append(tokens, ruleToken(r))
		}
		line += " rules=" + strings.Join(tokens, ";")
	}
	// Поля авто-пула — последними и по тому же правилу, что rules: только
	// при не-умолчательном выборе. Старый демон отвергнет такой файл
	// громко, а файл без авто-пула прочитает как раньше.
	return line + autoStateFields(c.Auto)
}

// setToken — набор в строке состояния: «имя[+ip]>действие».
//
// «+ip» стоит РАНЬШЕ «>»: разбор сначала отрезает действие по «>», потом
// суффикс «+ip» — в обратном порядке «telegram+ip>direct» не разобрался бы.
// «>» в именах каталога не бывает (geosite.ValidName).
func setToken(s Set) string {
	tok := s.Name
	if s.IP {
		tok += "+ip"
	}
	return tok + ">" + string(s.Action)
}

// ruleToken — правило в строке состояния: «вид:значение>действие», при
// непустом комментарии — «…|комментарий», закодированный url.QueryEscape.
//
// Двоеточие и «>» в значении не встречаются, кроме двоеточий IPv6 — поэтому
// вид отрезается по первому двоеточию, а действие по последнему «>» в части
// до «|». QueryEscape экранирует пробел, «;», «>», «|», «#», «%» и всё
// не-ASCII, так что комментарий не может порвать ни поля состояния (через
// пробел), ни список правил (через «;»), ни сам токен.
func ruleToken(r Rule) string {
	tok := string(r.Kind) + ":" + r.Value + ">" + string(r.Action)
	if r.Comment != "" {
		tok += "|" + url.QueryEscape(r.Comment)
	}
	return tok
}

// Parse читает состояние из файла.
//
// Три исхода, и различать их обязательно:
//   - файла нет, он пуст или состоит из одних комментариев (в том числе
//     комментарный mixin.yaml из поставки nikki) — «правила из профиля», наш;
//   - есть непустое содержимое, а нашей шапки нет — файл владельца: трогать
//     его нельзя, foreign=true;
//   - шапка есть — разбираем состояние; расхождение с телом → ErrCorrupt.
func Parse(b []byte) (Config, bool, error) {
	profile := Config{Policy: PolicyProfile, Download: DownloadDirect}

	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if !strings.HasPrefix(lines[0], headerMark) {
		return profile, hasContent(lines), nil
	}

	state := ""
	rules, custom, autoFilters := 0, 0, 0
	for _, ln := range lines[1:] {
		switch {
		case state == "" && strings.HasPrefix(ln, stateMark):
			state = strings.TrimSpace(strings.TrimPrefix(ln, stateMark))
		case strings.HasPrefix(ln, ruleSiteLinePrefix):
			rules++
		case strings.HasPrefix(ln, customSuffixPrefix), strings.HasPrefix(ln, customDomainPrefix),
			strings.HasPrefix(ln, customCIDRPrefix), strings.HasPrefix(ln, customCIDR6Prefix):
			custom++
		case strings.HasPrefix(ln, autoFilterKey), strings.HasPrefix(ln, autoExcludeKey):
			autoFilters++
		}
	}
	if state == "" {
		return profile, false, fmt.Errorf("%w: нет строки %s", ErrCorrupt, stateMark)
	}

	cfg, err := parseState(state)
	if err != nil {
		return profile, false, err
	}
	// Тело не разбирается — оно порождается из состояния. Но если число
	// правил ему не соответствует, файл правили руками или запись
	// оборвалась: показывать владельцу выбор, которого в правилах нет,
	// хуже, чем попросить применить заново.
	if rules != len(cfg.Sets) {
		return profile, false, fmt.Errorf("%w: наборов %d, правил %d", ErrCorrupt, len(cfg.Sets), rules)
	}
	// То же для своих правил, и в обе стороны: строка, дописанная в тело
	// руками, при следующей записи пропала бы молча — лучше отказать сейчас.
	if custom != len(cfg.Rules) {
		return profile, false, fmt.Errorf("%w: своих правил %d, строк %d", ErrCorrupt, len(cfg.Rules), custom)
	}
	// И то же для авто-пула. Строк фильтра ровно одна при настроенном
	// пуле и ни одной при умолчательном: приписанная руками исчезла бы
	// при следующей записи молча, а пропавшая означала бы, что пул в
	// состоянии есть, а в теле его нет.
	wantFilters := 0
	if !cfg.Auto.IsDefault() {
		wantFilters = 1
	}
	if autoFilters != wantFilters {
		return profile, false, fmt.Errorf("%w: строк фильтра авто-пула %d, ожидалась %d",
			ErrCorrupt, autoFilters, wantFilters)
	}
	return cfg, false, nil
}

// hasContent — есть ли в файле хоть одна строка, которая не комментарий и не
// пустая. Только такая строка делает файл чужим.
func hasContent(lines []string) bool {
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t != "" && !strings.HasPrefix(t, "#") {
			return true
		}
	}
	return false
}

// parseState разбирает содержимое строки состояния (без stateMark).
func parseState(s string) (Config, error) {
	cfg := Config{Download: DownloadDirect}
	seenPolicy := false
	// Политика до ADR-0041 (only/except): направление наборам выводится
	// после цикла — поля идут через пробел в любом порядке, и sets= может
	// стоять раньше policy=.
	var legacy Policy

	for _, field := range strings.Fields(s) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			return Config{}, fmt.Errorf("%w: поле %q без значения", ErrCorrupt, field)
		}
		switch key {
		case "policy":
			switch Policy(val) {
			case PolicyProfile, PolicyDirect, PolicyTunnel:
				cfg.Policy = Policy(val)
				seenPolicy = true
			case legacyOnly:
				legacy, cfg.Policy, seenPolicy = legacyOnly, PolicyDirect, true
			case legacyExcept:
				legacy, cfg.Policy, seenPolicy = legacyExcept, PolicyTunnel, true
			default:
				return Config{}, fmt.Errorf("%w: неизвестная политика %q", ErrCorrupt, val)
			}
		case "download":
			switch Download(val) {
			case DownloadDirect, DownloadTunnel:
				cfg.Download = Download(val)
			default:
				return Config{}, fmt.Errorf("%w: неизвестное скачивание %q", ErrCorrupt, val)
			}
		case "sets":
			sets, err := parseSets(val)
			if err != nil {
				return Config{}, err
			}
			cfg.Sets = sets
		case "rules":
			rules, err := parseRules(val)
			if err != nil {
				return Config{}, err
			}
			cfg.Rules = rules
		case "auto":
			switch AutoMode(val) {
			case AutoDeny, AutoAllow, AutoProvider:
				cfg.Auto.Mode = AutoMode(val)
			default:
				return Config{}, fmt.Errorf("%w: неизвестный режим авто-пула %q", ErrCorrupt, val)
			}
		case "auto-nodes":
			nodes, err := parseAutoNodes(val)
			if err != nil {
				return Config{}, err
			}
			for _, n := range nodes {
				if problem := autoNodeProblem(n); problem != "" {
					return Config{}, fmt.Errorf("%w: узел авто-пула %q: %s", ErrCorrupt, n, problem)
				}
			}
			cfg.Auto.Nodes = nodes
		default:
			// Незнакомое поле — это не наш файл нашей же версии. Молча
			// пропустить его значило бы применить непонятно что.
			return Config{}, fmt.Errorf("%w: неизвестное поле %q", ErrCorrupt, key)
		}
	}
	if !seenPolicy {
		return Config{}, fmt.Errorf("%w: в состоянии нет политики", ErrCorrupt)
	}
	// Две грамматики не смешиваются: у старого файла направление выводится
	// из политики, и токен с «>» в нём — правка руками; у нового токен без
	// «>» — оборванная запись. Пустого Action после разбора не бывает.
	for i := range cfg.Sets {
		switch {
		case legacy == "" && cfg.Sets[i].Action == "":
			return Config{}, fmt.Errorf("%w: набор %q без направления", ErrCorrupt, cfg.Sets[i].Name)
		case legacy != "" && cfg.Sets[i].Action != "":
			return Config{}, fmt.Errorf("%w: набор %q с направлением при политике %q", ErrCorrupt, cfg.Sets[i].Name, legacy)
		case legacy == legacyOnly:
			cfg.Sets[i].Action = ActionTunnel
		case legacy == legacyExcept:
			cfg.Sets[i].Action = ActionDirect
		}
	}
	return cfg, nil
}

// parseSets разбирает список наборов «имя[+ip][>действие]» через запятую.
//
// Действие здесь может отсутствовать — это форма файла до ADR-0041; сверку
// «есть ли оно там, где должно» делает parseState, когда известна политика.
func parseSets(val string) ([]Set, error) {
	if val == "" {
		// nil, а не пустой срез: так Parse(Render(c)) сходится с c, у
		// которого наборов не было вовсе.
		return nil, nil
	}
	tokens := strings.Split(val, ",")
	sets := make([]Set, 0, len(tokens))
	for _, tok := range tokens {
		head, act, hasAct := strings.Cut(tok, ">")
		name, ip := strings.CutSuffix(head, "+ip")
		if name == "" {
			return nil, fmt.Errorf("%w: пустое имя набора в %q", ErrCorrupt, val)
		}
		if hasAct && act != string(ActionTunnel) && act != string(ActionDirect) {
			return nil, fmt.Errorf("%w: у набора %q неизвестное направление %q", ErrCorrupt, name, act)
		}
		// Посторонний знак в имени — это файл, правленный руками: сами мы
		// таких имён не пишем (их отсеивает каталог). Принять его значило
		// бы вернуть имя обратно в mixin.yaml при следующей записи и
		// повалить nikki на разборе YAML. Запятая внутри имени сюда и не
		// доедет — она уже разъехалась на два набора выше.
		if !geosite.ValidName(name) {
			return nil, fmt.Errorf("%w: в имени набора %q недопустимые знаки", ErrCorrupt, name)
		}
		sets = append(sets, Set{Name: name, IP: ip, Action: RuleAction(act)})
	}
	return sets, nil
}

// parseRules разбирает список своих правил «вид:значение>действие» через
// точку с запятой. Проверки те же, что у Validate: файл, правленный руками,
// не должен вернуть в mixin.yaml то, что PUT бы отбил.
func parseRules(val string) ([]Rule, error) {
	if val == "" {
		return nil, nil
	}
	tokens := strings.Split(val, ";")
	rules := make([]Rule, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		kind, rest, ok := strings.Cut(tok, ":")
		if !ok {
			return nil, fmt.Errorf("%w: правило %q без вида", ErrCorrupt, tok)
		}
		// Комментарий отрезается раньше действия: закодированный хвост
		// «>» не содержит, но искать действие по последнему «>» надёжнее
		// в части, где комментария заведомо нет.
		rest, encoded, hasComment := strings.Cut(rest, "|")
		i := strings.LastIndex(rest, ">")
		if i < 0 {
			return nil, fmt.Errorf("%w: правило %q без действия", ErrCorrupt, tok)
		}
		r := Rule{Kind: RuleKind(kind), Value: rest[:i], Action: RuleAction(rest[i+1:])}
		if hasComment {
			c, err := url.QueryUnescape(encoded)
			if err != nil {
				return nil, fmt.Errorf("%w: комментарий правила %q не раскодируется: %v", ErrCorrupt, tok, err)
			}
			r.Comment = c
		}
		if reason := ruleProblem(r); reason != "" {
			return nil, fmt.Errorf("%w: правило %q: %s", ErrCorrupt, tok, reason)
		}
		key := string(r.Kind) + ":" + r.Value
		if seen[key] {
			return nil, fmt.Errorf("%w: правило %q повторяется", ErrCorrupt, tok)
		}
		seen[key] = true
		rules = append(rules, r)
	}
	return rules, nil
}

// ruleProblem — почему правило не годится; пустая строка — годится.
//
// Один источник правды для Validate (тело PUT) и parseRules (файл): что не
// принимается на входе, не должно приниматься и с диска.
func ruleProblem(r Rule) string {
	switch r.Kind {
	case RuleSuffix, RuleDomain, RuleCIDR:
	default:
		return fmt.Sprintf("неизвестный вид %q — бывают suffix, domain, cidr", r.Kind)
	}
	switch r.Action {
	case ActionTunnel, ActionDirect:
	default:
		return fmt.Sprintf("неизвестное действие %q — бывают tunnel, direct", r.Action)
	}
	if r.Value == "" {
		return "пустое значение"
	}
	if reason := commentProblem(r.Comment); reason != "" {
		return reason
	}
	if r.Kind == RuleCIDR {
		return cidrProblem(r.Value)
	}
	return domainProblem(r.Value)
}

// commentProblem — комментарий: одна строка печатного текста до 80 рун.
//
// Перевод строки порвал бы и строку состояния, и YAML-комментарий (вторая
// половина стала бы правилом или мусором для yq); управляющие символы в
// файле, который читают по ssh, — тоже мусор. Пробелы по краям не срезаются,
// а отбиваются: панель обрезает их при вводе, на глазах, как и регистр у
// домена.
func commentProblem(c string) string {
	if c == "" {
		return ""
	}
	if !utf8.ValidString(c) {
		return "комментарий — не UTF-8"
	}
	if utf8.RuneCountInString(c) > MaxCommentRunes {
		return fmt.Sprintf("комментарий длиннее %d знаков", MaxCommentRunes)
	}
	for _, ch := range c {
		if ch < 0x20 || ch == 0x7f {
			return "в комментарии перевод строки или управляющий символ"
		}
	}
	if strings.TrimSpace(c) != c {
		return "пробелы по краям комментария"
	}
	return ""
}

// cidrProblem — подсеть обязана быть канонической: ровно той строкой, какую
// mihomo и сам бы напечатал. Иначе одна и та же подсеть в двух написаниях
// прошла бы проверку на дубль.
func cidrProblem(v string) string {
	if !strings.Contains(v, "/") {
		return "подсеть записывается с маской, например 10.0.0.0/8 или 10.0.0.1/32"
	}
	p, err := netip.ParsePrefix(v)
	if err != nil {
		return "не разбирается как подсеть: нужны адрес и маска, например 100.64.0.0/10"
	}
	if p.Addr().Is4In6() {
		return "IPv4 внутри IPv6 (::ffff:…) не поддерживается — запишите как IPv4"
	}
	if p.Masked() != p {
		return fmt.Sprintf("в адресе есть биты вне маски — имелось в виду %s?", p.Masked())
	}
	if p.String() != v {
		return fmt.Sprintf("запись не каноническая — напишите %s", p)
	}
	return ""
}

// domainProblem — имя хоста: строчные метки из латиницы, цифр и дефиса через
// точку. Приводить к нижнему регистру или отрезать схему мы не беремся:
// молчаливая правка входа — это правка, которой владелец не видел.
func domainProblem(v string) string {
	if _, err := netip.ParseAddr(v); err == nil {
		return "это адрес, а не имя — выберите вид cidr и добавьте маску"
	}
	if strings.Contains(v, "/") {
		return "укажите только имя хоста, без схемы и пути"
	}
	if strings.Contains(v, ":") {
		return "укажите только имя хоста, без порта"
	}
	if strings.HasSuffix(v, ".") {
		return "точка в конце имени не нужна"
	}
	for _, c := range v {
		switch {
		case c > 0x7f:
			return "только латиница; кириллические имена — в punycode (xn--…)"
		case c >= 'A' && c <= 'Z':
			return "только строчные буквы"
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return "не похоже на имя хоста: бывают латиница, цифры, дефис и точка"
		}
	}
	if len(v) > 253 {
		return "имя длиннее 253 знаков"
	}
	for _, label := range strings.Split(v, ".") {
		switch {
		case label == "":
			return "пустая метка между точками"
		case len(label) > 63:
			return "метка длиннее 63 знаков"
		case label[0] == '-' || label[len(label)-1] == '-':
			return "метка не может начинаться или кончаться дефисом"
		}
	}
	return ""
}

// Fingerprint — отпечаток выбора для If-Match.
//
// Считается по канонической форме, а не по байтам файла: шапка может
// поменяться при обновлении демона, а выбор при этом тот же — перезаписывать
// файл и отбивать чужой PUT из-за смены формулировки в комментарии незачем.
// Восьми байт хватает: отпечаток защищает от гонки двух вкладок владельца, а
// не от подбора.
func Fingerprint(c Config) string {
	var b strings.Builder
	b.WriteString(string(c.Policy) + "\n")
	b.WriteString(string(c.Download) + "\n")
	for _, s := range c.Sets {
		b.WriteString(setToken(s) + "\n")
	}
	// Правила — после наборов и только при наличии: у выбора без правил
	// вход байт-в-байт прежний, и отпечатки, посчитанные до их появления,
	// остаются верными. Двоеточия и «>» в именах наборов не бывает, так что
	// токен правила с токеном набора не спутать.
	for _, r := range c.Rules {
		b.WriteString(ruleToken(r) + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// Validate проверяет форму выбора и существование имён.
//
// applied — то, что уже применено (прочитано из файла). Применённое имя
// валидно и без каталога: иначе повторное применение без интернета — скажем,
// одна лишь смена «откуда качать» — отбивалось бы на именах, которые сам же
// демон и записал.
func Validate(c Config, cat *geosite.Catalog, applied []Set) error {
	switch c.Policy {
	case PolicyProfile, PolicyDirect, PolicyTunnel:
	default:
		// Сюда же попадают only/except: тело PUT прежней панели. Текст
		// называет новую форму, чтобы владелец с curl не гадал.
		return fmt.Errorf("rulesets: неизвестная политика %q — бывают direct, tunnel, profile", c.Policy)
	}
	switch c.Download {
	case DownloadDirect, DownloadTunnel:
	default:
		return fmt.Errorf("rulesets: неизвестный способ скачивания %q", c.Download)
	}
	if c.Policy == PolicyProfile && len(c.Sets) > 0 {
		return fmt.Errorf("rulesets: политика %q не может идти с наборами (их %d)", PolicyProfile, len(c.Sets))
	}
	// Свои правила — раньше наборов и независимо от каталога: им каталог
	// не нужен, и «каталог недоступен» не должен заслонять опечатку в
	// домене — владелец пошёл бы чинить не то.
	if err := ValidateRules(c); err != nil {
		return err
	}

	seen := make(map[string]bool, len(c.Sets))
	known := appliedNames(applied)
	var unknown []string
	noCatalog := false

	for _, s := range c.Sets {
		// Знаки проверяются раньше каталога и НЕЗАВИСИМО от него, в том
		// числе у уже применённого имени. Имя приходит телом PUT, а
		// применённым мог стать через правку файла руками — и уедет в
		// одинарные кавычки правила и в строку состояния через запятую,
		// где посторонний знак ломает не наш разбор, а старт nikki.
		// Каталог такое имя отсеивает у себя (geosite.ValidName), но он
		// на этом пути не обязателен, поэтому проверка стоит и здесь.
		if !geosite.ValidName(s.Name) {
			return fmt.Errorf("rulesets: в имени набора %q недопустимые знаки — "+
				"бывают латиница, цифры и . _ - @ !", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("rulesets: набор %q выбран дважды", s.Name)
		}
		seen[s.Name] = true
		// Направление обязательно: groupFor("") молча дал бы DIRECT, и
		// набор без направления уехал бы в файл мимо туннеля.
		if s.Action != ActionTunnel && s.Action != ActionDirect {
			return fmt.Errorf("rulesets: у набора %q неизвестное направление %q — бывают tunnel, direct", s.Name, s.Action)
		}

		if _, ok := known[s.Name]; ok {
			continue
		}
		if cat == nil {
			noCatalog = true
			continue
		}
		if !cat.Has(s.Name) {
			unknown = append(unknown, s.Name)
		}
	}

	// Порядок исходов: без каталога перечислить «неизвестные» нельзя — мы
	// не знаем, каких имён нет, мы вообще ничего не знаем. Отсюда отдельный
	// сентинел и приоритет у него.
	if noCatalog {
		return ErrNoCatalog
	}
	if len(unknown) > 0 {
		return &UnknownSetsError{Names: unknown}
	}
	return nil
}

// ValidateRules — форма своих правил: политика, потолок, каждое правило,
// дубли. Отказ — всегда *RuleError с номером строки.
//
// Экспортирована отдельно от Validate, потому что обработчик PUT зовёт её
// ДО похода за каталогом: каталог стоит секунд и может быть недоступен, а
// опечатка в домене ни от него, ни от интернета не зависит.
func ValidateRules(c Config) error {
	if len(c.Rules) == 0 {
		return nil
	}
	if c.Policy == PolicyProfile {
		return &RuleError{Index: 1, Rule: c.Rules[0],
			Reason: fmt.Sprintf("политика %q не может идти со своими правилами", PolicyProfile)}
	}
	if len(c.Rules) > MaxRules {
		return &RuleError{Index: MaxRules + 1, Rule: c.Rules[MaxRules],
			Reason: fmt.Sprintf("своих правил больше %d", MaxRules)}
	}
	seen := make(map[string]int, len(c.Rules))
	for i, r := range c.Rules {
		if reason := ruleProblem(r); reason != "" {
			return &RuleError{Index: i + 1, Rule: r, Reason: reason}
		}
		// Дубль — по виду и значению, действие не в счёт: второе такое
		// правило недостижимо при любом действии, а при другом — ещё и
		// спорит с первым.
		key := string(r.Kind) + ":" + r.Value
		if at, dup := seen[key]; dup {
			return &RuleError{Index: i + 1, Rule: r,
				Reason: fmt.Sprintf("значение %q указано дважды (первый раз — правило %d)", r.Value, at)}
		}
		seen[key] = i + 1
	}
	return nil
}

// appliedNames — применённые наборы по имени, с признаком подсетей.
func appliedNames(applied []Set) map[string]bool {
	out := make(map[string]bool, len(applied))
	for _, s := range applied {
		out[s.Name] = s.IP
	}
	return out
}

// Resolve проставляет признак подсетей.
//
// Панель признак не присылает: знать, есть ли имя в дереве geoip, может
// только каталог. Когда каталога нет, а имя уже применено, признак берётся из
// файла — иначе повторное применение без интернета молча потеряло бы половину
// набора (домены остались бы, подсети исчезли).
//
// Возвращает копию: c принадлежит вызывающему, и правка его среза на месте
// была бы правкой чужой памяти.
func Resolve(c Config, cat *geosite.Catalog, applied []Set) Config {
	if len(c.Sets) == 0 {
		return c
	}
	known := appliedNames(applied)

	out := c
	out.Sets = make([]Set, len(c.Sets))
	for i, s := range c.Sets {
		switch {
		case cat != nil && cat.Has(s.Name):
			s.IP = cat.HasIP(s.Name)
		default:
			s.IP = known[s.Name]
		}
		out.Sets[i] = s
	}
	return out
}

// Verify — какие наборы движок реально скачал.
//
// Набор загружен, когда загружены ВСЕ его провайдеры: у набора с подсетями
// скачавшиеся домены без подсетей — это половина набора, и показывать её как
// готовую значит врать. Число правил критерием НЕ является: три набора в
// репозитории пусты и загружаются с нулём правил — по ruleCount они были бы
// вечно «не загрузились».
//
// Оба среза непустые (пусть и нулевой длины) и в порядке входа: они уезжают в
// JSON панели, где nil стал бы null вместо [].
func Verify(sets []Set, live map[string]nikki.RuleProvider) (loaded, missing []Set) {
	loaded, missing = []Set{}, []Set{}
	for _, s := range sets {
		ok := true
		for _, name := range ProviderNames(s) {
			p, found := live[name]
			if !found || p.UpdatedAt.IsZero() {
				ok = false
				break
			}
		}
		if ok {
			loaded = append(loaded, s)
		} else {
			missing = append(missing, s)
		}
	}
	return loaded, missing
}
