package mixin

// Авто-пул: чем наполняется группа AUTO.
//
// Вторая секция того же файла. С наборами её роднит только файл, поэтому
// состояние, отпечаток и проверки у неё свои; общее — шапка, порядок
// записи и правило «тело порождается из состояния, обратно не
// разбирается».
//
// Зачем это вообще. Раньше группа PROXY была url-test поверх всего списка
// подписки, и «Авто» в панели означало снятие ручного закрепления. Отсюда
// два следствия: пул для авто и пул для ручного выбора были одним
// списком, а движок выбирал «быстрейшего» из всех — и на живом роутере
// выбрал российский узел, уведя туда трафик, который гнали в туннель
// ради обхода (docs/recon/raw/93-autopool-spike.txt).
//
// Разделить пулы можно только фильтром на отдельном провайдере: группы
// mihomo при склейке КОНКАТЕНИРУЮТСЯ (.nikki-proxy-groups + .proxy-groups),
// и дописать поле в группу профиля нельзя — вышло бы две группы с одним
// именем. А proxy-providers — карта, и yq сливает её по ключу. Поэтому
// профиль объявляет провайдера sub-auto БЕЗ фильтра (то есть весь
// список), а панель дописывает ему filter или exclude-filter. Выключенная
// склейка оставляет профильного sub-auto целым, и движок продолжает
// работать по всему списку — сломать сеть настройкой пула нельзя.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// AutoMode — чем наполняется группа AUTO.
type AutoMode string

const (
	// AutoDeny — все узлы, кроме отмеченных. Умолчание: с пустым списком
	// это весь список, то есть поведение до появления авто-пула.
	AutoDeny AutoMode = "deny"
	// AutoAllow — только отмеченные.
	AutoAllow AutoMode = "allow"
	// AutoProvider — состав берётся из балансировщика подписки.
	//
	// От AutoAllow отличается не формой, а происхождением списка: имена
	// пересобираются при каждом обновлении подписки, а не правятся руками.
	// Поэтому режим хранится отдельно — иначе обновление подписки молча
	// затирало бы ручной выбор владельца.
	AutoProvider AutoMode = "provider"
)

const (
	// AutoProviderName — имя провайдера, которому дописывается фильтр.
	//
	// Это договор с профилем владельца, а не наша выдумка: провайдер с
	// таким именем объявлен там, и демон только дополняет его. Имя
	// зафиксировано по той же причине, что и BYPASS у наборов, — сделать
	// его настройкой значило бы дать способ отфильтровать чужого
	// провайдера, не заметив этого.
	AutoProviderName = "sub-auto"
	// AutoGroup — группа, которую наполняет провайдер. Наружу нужна как
	// имя участника PROXY: выбор «Авто» в панели это выбор этого имени.
	AutoGroup = "AUTO"

	// autoStateField — поле режима в строке состояния.
	//
	// Пишется ТОЛЬКО при не-умолчательном пуле, и это не экономия места.
	// Старый демон, не знающий авто-пула, по контракту отвергает
	// незнакомое поле состояния как ErrCorrupt. Значит файл с настроенным
	// пулом он отвергнет громко (500 mixin_corrupt), а не перепишет, молча
	// потеряв пул; файл без пула прочитает как раньше. Тот же приём, что
	// у rules= (docs/contracts/nikki-mixin-rulesets.md).
	autoStateField  = "auto="
	autoNodesField  = "auto-nodes="
	autoFilterKey   = "    filter: "
	autoExcludeKey  = "    exclude-filter: "
	autoProvidersKV = "proxy-providers:\n"

	// MaxAutoNodes — потолок списка. Подписка отдаёт четыре десятка имён;
	// сотня с запасом закрывает рост, а заодно ограничивает длину
	// регулярки, которую движок будет применять к каждому узлу.
	MaxAutoNodes = 128
)

// AutoConfig — выбор владельца по авто-пулу.
//
// Nodes у AutoDeny — что ИСКЛЮЧИТЬ, у AutoAllow и AutoProvider — что
// оставить. Одно поле на оба смысла, потому что смысл задаёт Mode, а две
// пары «список + режим» разъехались бы при первой же смене режима.
type AutoConfig struct {
	Mode  AutoMode `json:"mode"`
	Nodes []string `json:"nodes"`
}

// IsDefault — пул не настроен: AUTO собирается из всего списка.
//
// Пустой Mode считается умолчанием наравне с AutoDeny: нулевое значение
// структуры обязано означать «ничего не выбрано», иначе Config{} писал бы
// в файл секцию.
func (a AutoConfig) IsDefault() bool {
	return (a.Mode == "" || a.Mode == AutoDeny) && len(a.Nodes) == 0
}

// autoBody — рубрика proxy-providers или пусто.
func autoBody(a AutoConfig) string {
	if a.IsDefault() {
		return ""
	}
	key := autoFilterKey
	if a.Mode == AutoDeny {
		key = autoExcludeKey
	}
	var b strings.Builder
	b.WriteString(autoProvidersKV)
	fmt.Fprintf(&b, "  %s:\n", AutoProviderName)
	b.WriteString(key + yamlSingleQuoted(autoRegexp(a.Nodes)) + "\n")
	return b.String()
}

// autoRegexp собирает RE2 из имён узлов.
//
// QuoteMeta обязателен: в именах подписки есть скобки и дефисы —
// «🇨🇭⚪Швейцария (БС-1)☁️» без экранирования стала бы группой захвата.
// Якоря обязательны по другой причине: mihomo применяет фильтр как
// «содержит», и без них «Россия» совпала бы и с «Россия-2», а «Англия-1»
// поймала бы «Англия-10».
func autoRegexp(nodes []string) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, regexp.QuoteMeta(n))
	}
	return "^(?:" + strings.Join(parts, "|") + ")$"
}

// yamlSingleQuoted — значение в одинарных кавычках YAML.
//
// Одинарные, а не двойные: в регулярке полно обратных слэшей от
// QuoteMeta, и в двойных кавычках YAML съел бы их как экранирование.
// Внутри одинарных кавычек значим только сам апостроф, и удваивается он.
func yamlSingleQuoted(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// autoStateFields — хвост строки состояния или пусто.
func autoStateFields(a AutoConfig) string {
	if a.IsDefault() {
		return ""
	}
	out := " " + autoStateField + string(a.Mode)
	if len(a.Nodes) > 0 {
		esc := make([]string, 0, len(a.Nodes))
		for _, n := range a.Nodes {
			// QueryEscape по той же причине, что у комментариев правил:
			// имя может содержать пробел, «;» и «=», то есть порвать и
			// список, и саму строку состояния.
			esc = append(esc, url.QueryEscape(n))
		}
		out += " " + autoNodesField + strings.Join(esc, ";")
	}
	return out
}

// parseAutoNodes разбирает список имён из состояния.
func parseAutoNodes(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	toks := strings.Split(v, ";")
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		n, err := url.QueryUnescape(t)
		if err != nil {
			return nil, fmt.Errorf("%w: имя узла авто-пула не раскодируется: %q", ErrCorrupt, t)
		}
		out = append(out, n)
	}
	return out, nil
}

// AutoFingerprint — отпечаток авто-пула для If-Match.
//
// Отдельный от Fingerprint намеренно: наборы и пул — независимые решения
// в одном файле, и правка одного не должна отбивать чужой PUT другого.
func AutoFingerprint(a AutoConfig) string {
	var b strings.Builder
	b.WriteString(string(a.Mode) + "\n")
	for _, n := range a.Nodes {
		b.WriteString(n + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// ValidateAuto проверяет форму выбора.
//
// Существование имён здесь не проверяется: список узлов знает манифест
// подписки, а не этот пакет. Проверку «имя ещё есть у провайдера» делает
// обработчик — по той же границе, по которой Validate не ходит за
// каталогом geosite сам.
func ValidateAuto(a AutoConfig) error {
	switch a.Mode {
	case "", AutoDeny, AutoAllow, AutoProvider:
	default:
		return fmt.Errorf("неизвестный режим авто-пула %q", a.Mode)
	}
	if len(a.Nodes) > MaxAutoNodes {
		return fmt.Errorf("узлов в авто-пуле %d, больше %d нельзя", len(a.Nodes), MaxAutoNodes)
	}
	// Пустой список у «только отмеченные» — пул из нуля узлов. Движок это
	// переживёт (в профиле стоит empty-fallback: REJECT), но записывать
	// заведомо пустую группу нельзя: владелец увидит мёртвое «Авто» и не
	// поймёт, чем он его опустошил.
	if (a.Mode == AutoAllow || a.Mode == AutoProvider) && len(a.Nodes) == 0 {
		return fmt.Errorf("в авто-пуле не осталось ни одного узла")
	}
	seen := make(map[string]bool, len(a.Nodes))
	for i, n := range a.Nodes {
		if problem := autoNodeProblem(n); problem != "" {
			return fmt.Errorf("узел %d (%q) не принят: %s", i+1, n, problem)
		}
		if seen[n] {
			return fmt.Errorf("узел %q указан дважды", n)
		}
		seen[n] = true
	}
	return nil
}

// autoNodeProblem — один источник правды для ValidateAuto (тело PUT) и
// разбора состояния с диска.
//
// Имя приходит от провайдера подписки и содержать может почти что угодно,
// поэтому проверяется не алфавит, а пригодность: непустое, валидный
// UTF-8, в одну строку и без управляющих символов — последние порвали бы
// и строку состояния, и YAML.
func autoNodeProblem(n string) string {
	switch {
	case n == "":
		return "пустое имя"
	case !utf8.ValidString(n):
		return "имя не является валидным UTF-8"
	case len(n) > 256:
		return "имя длиннее 256 байт"
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return "в имени управляющий символ"
		}
	}
	if strings.TrimSpace(n) != n {
		return "имя с пробелами по краям"
	}
	return ""
}
