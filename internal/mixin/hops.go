package mixin

// Пул хопов: через какие узлы подписки строятся цепочки до своих выходных
// нод, и что sniffer не должен трогать.
//
// Третья секция того же файла — по тем же правилам, что авто-пул: своё
// состояние, свой отпечаток, тело порождается из состояния и обратно не
// разбирается, а при умолчательном выборе файл байт-в-байт прежний.
//
// Почему группы, а не фильтр провайдера, как у авто-пула. Цепочка — узел
// с dialer-proxy, а dialer-proxy в mihomo разрешается только в узел или
// группу ВЕРХНЕГО уровня: узел провайдера по имени он не находит (на
// старте — фатально, в провайдере — отказ на каждом соединении), и
// mihomo -t этого не ловит (docs/recon/exit-chains.md). Поэтому на каждый
// хоп пишется группа-однохопник в nikki-proxy-groups — nikki дописывает их
// в начало proxy-groups. Имена своих групп с профилем не совпадают
// (приставка nm-hop-), а профиль на них не ссылается: на них ссылаются
// только цепочки из файла провайдера exits, который пишет тот же демон из
// того же Pool. Выключенная склейка уносит группы — цепочки отказывают,
// движок стартует, прямые выходы живы.
//
// Почему здесь же sniffer. На живом роутере sniffer переписывает адрес
// назначения по SNI (overwrite_destination, docs/recon/nikki-sniffer.md),
// и Reality-соединение клиента LAN до своей ноды уходит на настоящий сайт
// маскировки. skip-domain с SNI своих нод это отключает только для них.
// ucode nikki пишет skip-domain лишь при sniffer_ignore_domain_name=1 —
// тогда он затрёт наш список, и это видно в панели, а не молча.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"netmoded/internal/exits"
)

// HopConfig — выбор владельца по пулу хопов.
//
// Mode и Nodes значат то же, что у AutoConfig. Pool — уже вычисленный по
// манифесту подписки состав (обработчик и обновление подписки), из него
// строятся и группы здесь, и цепочки в exits.yaml: один список на оба
// файла, иначе они разъедутся.
type HopConfig struct {
	Mode  AutoMode `json:"mode"`
	Nodes []string `json:"nodes"`
	Pool  []string `json:"pool"`
}

// IsDefault — пула нет, цепочек нет.
//
// В отличие от авто-пула умолчание здесь — ноль хопов, а не весь список:
// каждая цепочка — это проверка здоровья через выход, у resi — через
// ISP-прокси, который отказывает при частых новых соединениях
// (docs/recon/exit-chains.md). Сорок цепочек на выход никто не просил.
func (h HopConfig) IsDefault() bool {
	return h.Mode == "" && len(h.Nodes) == 0 && len(h.Pool) == 0
}

const (
	hopStateField = "hop="
	hopNodesField = "hop-nodes="
	hopPoolField  = "hop-pool="
	skipSNIField  = "skip-sni="

	hopGroupsKV    = "nikki-proxy-groups:\n"
	hopLinePrefix  = "  - {name: 'nm-hop-"
	snifferKV      = "sniffer:\n  skip-domain:\n"
	skipLinePrefix = "    - '"

	// MaxHopPool — потолок пула: на каждый хоп — цепочка на каждый выход
	// каждой ноды, и у каждой своя проверка здоровья через выход.
	MaxHopPool = 8
	// MaxSkipSNI — своих нод единицы; потолок ограничивает строку
	// состояния, а не владельца.
	MaxSkipSNI = 32
)

var sniRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// hopBody — рубрики nikki-proxy-groups и sniffer или пусто.
//
// Группа — однострочной записью: построчные ключи под отступом 4 совпали
// бы с autoFilterKey, и Parse посчитал бы их строками авто-пула.
func hopBody(c Config) string {
	var b strings.Builder
	if len(c.Hops.Pool) > 0 {
		b.WriteString(hopGroupsKV)
		for _, h := range c.Hops.Pool {
			// empty-fallback — kill switch: пустая группа иначе COMPATIBLE = DIRECT.
			fmt.Fprintf(&b, "  - {name: %s, type: select, use: [%s], filter: %s, empty-fallback: 'REJECT'}\n",
				yamlSingleQuoted(exits.HopGroupName(h)), yamlSingleQuoted(exits.SubProvider),
				yamlSingleQuoted("^"+regexp.QuoteMeta(h)+"$"))
		}
	}
	if len(c.SkipSNI) > 0 {
		b.WriteString(snifferKV)
		for _, s := range c.SkipSNI {
			b.WriteString("    - " + yamlSingleQuoted(s) + "\n")
		}
	}
	return b.String()
}

// hopStateFields — хвост строки состояния или пусто. Поля пишутся только
// при не-умолчательном выборе: демон прежней версии отвергнет такой файл
// громко (неизвестное поле), а файл без пула прочитает как раньше.
func hopStateFields(c Config) string {
	var out string
	if !c.Hops.IsDefault() {
		out += " " + hopStateField + string(c.Hops.Mode)
		if len(c.Hops.Nodes) > 0 {
			out += " " + hopNodesField + escList(c.Hops.Nodes)
		}
		if len(c.Hops.Pool) > 0 {
			out += " " + hopPoolField + escList(c.Hops.Pool)
		}
	}
	if len(c.SkipSNI) > 0 {
		out += " " + skipSNIField + strings.Join(c.SkipSNI, ",")
	}
	return out
}

func escList(names []string) string {
	esc := make([]string, 0, len(names))
	for _, n := range names {
		esc = append(esc, url.QueryEscape(n))
	}
	return strings.Join(esc, ";")
}

// parseHopField разбирает одно поле секции; ok=false — поле не наше.
func parseHopField(cfg *Config, key, val string) (bool, error) {
	switch key {
	case "hop":
		switch AutoMode(val) {
		case AutoDeny, AutoAllow, AutoProvider:
			cfg.Hops.Mode = AutoMode(val)
		default:
			return true, fmt.Errorf("%w: неизвестный режим пула хопов %q", ErrCorrupt, val)
		}
	case "hop-nodes", "hop-pool":
		nodes, err := parseAutoNodes(val)
		if err != nil {
			return true, err
		}
		for _, n := range nodes {
			if problem := autoNodeProblem(n); problem != "" {
				return true, fmt.Errorf("%w: узел пула хопов %q: %s", ErrCorrupt, n, problem)
			}
		}
		if key == "hop-nodes" {
			cfg.Hops.Nodes = nodes
		} else {
			cfg.Hops.Pool = nodes
		}
	case "skip-sni":
		list := strings.Split(val, ",")
		if err := ValidateSkipSNI(list); err != nil {
			return true, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		cfg.SkipSNI = list
	default:
		return false, nil
	}
	return true, nil
}

// ValidateHops проверяет форму выбора. Существование имён у провайдера —
// забота обработчика (манифест подписки), как у ValidateAuto.
func ValidateHops(h HopConfig) error {
	if h.IsDefault() {
		return nil
	}
	switch h.Mode {
	case AutoDeny, AutoAllow, AutoProvider:
	default:
		return fmt.Errorf("неизвестный режим пула хопов %q", h.Mode)
	}
	// Режим без единого хопа — цепочки пропали бы молча. Нет хопов —
	// значит, пула нет: сброс режима, а не пустой список.
	if len(h.Pool) == 0 {
		return fmt.Errorf("в пуле хопов не осталось ни одного узла")
	}
	if len(h.Pool) > MaxHopPool {
		return fmt.Errorf("хопов в пуле %d, больше %d нельзя: на каждый — цепочка на каждый выход со своей проверкой", len(h.Pool), MaxHopPool)
	}
	if len(h.Nodes) > MaxAutoNodes {
		return fmt.Errorf("узлов в выборе пула хопов %d, больше %d нельзя", len(h.Nodes), MaxAutoNodes)
	}
	if (h.Mode == AutoAllow || h.Mode == AutoProvider) && len(h.Nodes) == 0 {
		return fmt.Errorf("в выборе пула хопов ни одного узла")
	}
	chosen := make(map[string]bool, len(h.Nodes))
	for i, n := range h.Nodes {
		if problem := autoNodeProblem(n); problem != "" {
			return fmt.Errorf("узел %d (%q) не принят: %s", i+1, n, problem)
		}
		if chosen[n] {
			return fmt.Errorf("узел %q указан дважды", n)
		}
		chosen[n] = true
	}
	seen := make(map[string]bool, len(h.Pool))
	for _, n := range h.Pool {
		if problem := autoNodeProblem(n); problem != "" {
			return fmt.Errorf("хоп %q не принят: %s", n, problem)
		}
		if seen[n] {
			return fmt.Errorf("хоп %q в пуле дважды", n)
		}
		seen[n] = true
		// Пул обязан следовать из выбора: иначе панель показывает одно,
		// а цепочки строятся через другое.
		if h.Mode == AutoDeny && chosen[n] {
			return fmt.Errorf("хоп %q исключён, но стоит в пуле", n)
		}
		if h.Mode != AutoDeny && !chosen[n] {
			return fmt.Errorf("хоп %q в пуле, но не выбран", n)
		}
	}
	return nil
}

// ValidateSkipSNI — имена хостов без повторов: они уходят в YAML в
// одинарных кавычках и в строку состояния через запятую.
func ValidateSkipSNI(list []string) error {
	if len(list) > MaxSkipSNI {
		return fmt.Errorf("SNI для sniffer %d, больше %d нельзя", len(list), MaxSkipSNI)
	}
	seen := make(map[string]bool, len(list))
	for _, s := range list {
		if !sniRe.MatchString(s) {
			return fmt.Errorf("SNI %q — не имя хоста", s)
		}
		if seen[s] {
			return fmt.Errorf("SNI %q указан дважды", s)
		}
		seen[s] = true
	}
	return nil
}

// HopFingerprint — отпечаток выбора пула хопов для If-Match.
//
// По режиму и выбору, без Pool: состав пересчитывается при обновлении
// подписки, а выбор владельца при этом тот же — отбивать его открытую
// вкладку незачем.
func HopFingerprint(h HopConfig) string {
	var b strings.Builder
	b.WriteString(string(h.Mode) + "\n")
	for _, n := range h.Nodes {
		b.WriteString(n + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:8])
}
