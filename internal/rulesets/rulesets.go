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
package rulesets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"netmoded/internal/geosite"
	"netmoded/internal/nikki"
)

// Policy — что делать с выбранными наборами.
type Policy string

const (
	// PolicyProfile — наборов нет, правила целиком из профиля nikki.
	PolicyProfile Policy = "profile"
	// PolicyOnly — в туннель идут только выбранные наборы.
	PolicyOnly Policy = "only"
	// PolicyExcept — в туннель идёт всё, кроме выбранных наборов.
	PolicyExcept Policy = "except"
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
}

// Config — весь выбор целиком. Порядок Sets значим: он же порядок правил.
type Config struct {
	Policy   Policy
	Download Download
	Sets     []Set
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
)

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

// groups — какая группа достаётся наборам и какая хвосту MATCH.
func groups(p Policy) (set, tail string) {
	if p == PolicyExcept {
		return directGroup, TunnelGroup
	}
	return TunnelGroup, directGroup
}

// Render собирает файл целиком. Детерминирован: одинаковый Config даёт
// одинаковые байты — на этом держится и golden-тест, и отпечаток.
func Render(c Config) []byte {
	var b strings.Builder

	b.WriteString(headerLine1 + "\n")
	b.WriteString(headerLine2 + "\n")
	b.WriteString(stateLine(c) + "\n")

	if c.Policy == PolicyProfile {
		// Профиль — это «нас тут нет»: ни провайдеров, ни правил, чтобы
		// склейка yq ничего не добавила к правилам профиля.
		return []byte(b.String())
	}

	setGroup, tailGroup := groups(c.Policy)

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
	for _, s := range c.Sets {
		// Порядок внутри набора: сначала домены, потом подсети. Правило по
		// подсетям идёт с no-resolve — иначе mihomo резолвил бы каждый
		// домен, чтобы сверить адрес, и платил бы за это задержкой DNS.
		fmt.Fprintf(&b, "  - 'RULE-SET,%s%s,%s'\n", ProviderSitePrefix, s.Name, setGroup)
		if s.IP {
			fmt.Fprintf(&b, "  - 'RULE-SET,%s%s,%s,no-resolve'\n", ProviderIPPrefix, s.Name, setGroup)
		}
	}
	// См. документацию пакета: без этой строки политика «кроме» уводит в
	// туннель локальную сеть.
	b.WriteString("  - 'GEOIP,PRIVATE," + directGroup + ",no-resolve'\n")
	b.WriteString("  - 'MATCH," + tailGroup + "'\n")

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
		return stateMark + " policy=" + string(PolicyProfile)
	}
	names := make([]string, 0, len(c.Sets))
	for _, s := range c.Sets {
		names = append(names, setToken(s))
	}
	// Поле sets пишется даже пустым: его отсутствие значило бы «строка
	// оборвалась», а пустое значение — «выбрано ноль наборов», и это
	// разные вещи.
	return fmt.Sprintf("%s policy=%s download=%s sets=%s",
		stateMark, c.Policy, c.Download, strings.Join(names, ","))
}

// setToken — набор в строке состояния: «имя» или «имя+ip».
func setToken(s Set) string {
	if s.IP {
		return s.Name + "+ip"
	}
	return s.Name
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
	rules := 0
	for _, ln := range lines[1:] {
		switch {
		case state == "" && strings.HasPrefix(ln, stateMark):
			state = strings.TrimSpace(strings.TrimPrefix(ln, stateMark))
		case strings.HasPrefix(ln, ruleSiteLinePrefix):
			rules++
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

	for _, field := range strings.Fields(s) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			return Config{}, fmt.Errorf("%w: поле %q без значения", ErrCorrupt, field)
		}
		switch key {
		case "policy":
			switch Policy(val) {
			case PolicyProfile, PolicyOnly, PolicyExcept:
				cfg.Policy = Policy(val)
				seenPolicy = true
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
		default:
			// Незнакомое поле — это не наш файл нашей же версии. Молча
			// пропустить его значило бы применить непонятно что.
			return Config{}, fmt.Errorf("%w: неизвестное поле %q", ErrCorrupt, key)
		}
	}
	if !seenPolicy {
		return Config{}, fmt.Errorf("%w: в состоянии нет политики", ErrCorrupt)
	}
	return cfg, nil
}

// parseSets разбирает список наборов «имя» / «имя+ip» через запятую.
func parseSets(val string) ([]Set, error) {
	if val == "" {
		// nil, а не пустой срез: так Parse(Render(c)) сходится с c, у
		// которого наборов не было вовсе.
		return nil, nil
	}
	tokens := strings.Split(val, ",")
	sets := make([]Set, 0, len(tokens))
	for _, tok := range tokens {
		name, ip := strings.CutSuffix(tok, "+ip")
		if name == "" {
			return nil, fmt.Errorf("%w: пустое имя набора в %q", ErrCorrupt, val)
		}
		sets = append(sets, Set{Name: name, IP: ip})
	}
	return sets, nil
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
	case PolicyProfile, PolicyOnly, PolicyExcept:
	default:
		return fmt.Errorf("rulesets: неизвестная политика %q", c.Policy)
	}
	switch c.Download {
	case DownloadDirect, DownloadTunnel:
	default:
		return fmt.Errorf("rulesets: неизвестный способ скачивания %q", c.Download)
	}
	if c.Policy == PolicyProfile && len(c.Sets) > 0 {
		return fmt.Errorf("rulesets: политика %q не может идти с наборами (их %d)", PolicyProfile, len(c.Sets))
	}

	seen := make(map[string]bool, len(c.Sets))
	known := appliedNames(applied)
	var unknown []string
	noCatalog := false

	for _, s := range c.Sets {
		if seen[s.Name] {
			return fmt.Errorf("rulesets: набор %q выбран дважды", s.Name)
		}
		seen[s.Name] = true

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
