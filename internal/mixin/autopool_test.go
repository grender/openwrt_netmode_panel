package mixin

import (
	"errors"
	"strings"
	"testing"
)

// Имена узлов подписки — с эмодзи, пробелами, скобками и дефисами. Взяты
// с живого роутера: ровно на них проверяется экранирование и в регулярку,
// и в YAML.
const (
	nodeRu   = "🇷🇺💳Россия"
	nodeBS1  = "🇨🇭⚪Швейцария (БС-1)☁️"
	nodeDE   = "🇩🇪⚡Германия"
	nodeSep  = "⬇️ Обходы белых списков ⬇️"
	nodeQuot = "узел с ' апострофом"
)

func autoBodyOf(t *testing.T, c Config) string {
	t.Helper()
	out := string(Render(c))
	i := strings.Index(out, "proxy-providers:")
	if i < 0 {
		return ""
	}
	return out[i:]
}

// Умолчание — «все, кроме отмеченных» с пустым списком. Это сегодняшнее
// поведение слово в слово, и файл от него обязан не отличаться ни байтом:
// иначе обновление демона переписало бы mixin.yaml у всех, кто авто-пул
// не трогал, и сломало бы отпечатки в открытых вкладках.
func TestAutoDefaultChangesNothing(t *testing.T) {
	base := Config{Policy: PolicyDirect, Download: DownloadDirect,
		Sets: []Set{{Name: "youtube", Action: ActionTunnel}}}
	withAuto := base
	withAuto.Auto = AutoConfig{Mode: AutoDeny}

	if got, want := string(Render(withAuto)), string(Render(base)); got != want {
		t.Errorf("умолчание авто-пула изменило файл:\n%s\nожидалось:\n%s", got, want)
	}
	if strings.Contains(string(Render(withAuto)), autoStateField) {
		t.Error("в состоянии появилось поле авто-пула, хотя выбор умолчательный")
	}
}

// «Все, кроме отмеченных» — это exclude-filter, «только отмеченные» — filter.
// Разные ключи mihomo, и перепутать их значит получить ровно обратный пул.
func TestAutoModeChoosesFilterKey(t *testing.T) {
	for _, tt := range []struct {
		mode AutoMode
		key  string
	}{
		{AutoDeny, "exclude-filter"},
		{AutoAllow, "filter"},
		{AutoProvider, "filter"},
	} {
		body := autoBodyOf(t, Config{Policy: PolicyDirect, Download: DownloadDirect,
			Auto: AutoConfig{Mode: tt.mode, Nodes: []string{nodeDE}}})
		if !strings.Contains(body, "    "+tt.key+": ") {
			t.Errorf("режим %q: в теле нет %q:\n%s", tt.mode, tt.key, body)
		}
		if !strings.HasPrefix(body, "proxy-providers:\n  "+AutoProviderName+":\n") {
			t.Errorf("режим %q: не та рубрика:\n%s", tt.mode, body)
		}
	}
}

// Имена уезжают в регулярку RE2. В них есть скобки и дефисы — без
// QuoteMeta «(БС-1)» стала бы группой захвата, и фильтр поймал бы не тот
// узел. Якоря обязательны: без них «Россия» совпала бы и с «Россия-2».
func TestAutoNamesAreQuotedAndAnchored(t *testing.T) {
	body := autoBodyOf(t, Config{Policy: PolicyDirect, Download: DownloadDirect,
		Auto: AutoConfig{Mode: AutoDeny, Nodes: []string{nodeRu, nodeBS1}}})

	if strings.Contains(body, "(БС-1)") {
		t.Errorf("скобки имени не экранированы — это группа захвата:\n%s", body)
	}
	if !strings.Contains(body, `\(БС-1\)`) {
		t.Errorf("ожидалось экранирование скобок:\n%s", body)
	}
	if !strings.Contains(body, "'^(?:") || !strings.Contains(body, ")$'") {
		t.Errorf("регулярка без якорей:\n%s", body)
	}
}

// Апостроф в имени закрыл бы YAML-строку. Одинарные кавычки тела —
// дом-стиль этого файла (так же пишутся правила), и удваивание апострофа
// в них единственный верный способ.
func TestAutoNameWithQuoteSurvivesYAML(t *testing.T) {
	body := autoBodyOf(t, Config{Policy: PolicyDirect, Download: DownloadDirect,
		Auto: AutoConfig{Mode: AutoAllow, Nodes: []string{nodeQuot}}})
	if !strings.Contains(body, "''") {
		t.Errorf("апостроф не удвоен, строка YAML оборвётся:\n%s", body)
	}
	line := ""
	for _, ln := range strings.Split(body, "\n") {
		if strings.Contains(ln, "filter: ") {
			line = ln
		}
	}
	if n := strings.Count(line, "'"); n%2 != 0 {
		t.Errorf("нечётное число кавычек в строке — YAML сломан: %q", line)
	}
}

// Состояние — единственный источник тела, значит Parse(Render(c)) обязан
// вернуть ровно c. Имена с пробелами, «;» и не-ASCII проверяют, что
// percent-encoding списка не рвётся.
func TestAutoRoundTrip(t *testing.T) {
	for _, want := range []AutoConfig{
		{Mode: AutoDeny, Nodes: []string{nodeRu, nodeBS1}},
		{Mode: AutoAllow, Nodes: []string{nodeDE, nodeSep}},
		{Mode: AutoProvider, Nodes: []string{nodeDE}},
	} {
		c := Config{Policy: PolicyDirect, Download: DownloadDirect,
			Sets: []Set{{Name: "youtube", Action: ActionTunnel}}, Auto: want}

		got, foreign, err := Parse(Render(c))
		if err != nil || foreign {
			t.Fatalf("%+v: Parse → foreign=%v err=%v", want, foreign, err)
		}
		if got.Auto.Mode != want.Mode {
			t.Errorf("%+v: режим вернулся %q", want, got.Auto.Mode)
		}
		if strings.Join(got.Auto.Nodes, "|") != strings.Join(want.Nodes, "|") {
			t.Errorf("%+v: узлы вернулись %q", want, got.Auto.Nodes)
		}
	}
}

// Пустой режим и AutoDeny с пустым списком — одно состояние: пул не
// настроен. Разбор их НЕ канонизирует, чтобы Parse(Render(c)) совпадало с
// c байт в байт и структура в структуру; выбор слова для провода делает
// граница API, а не этот пакет.
func TestAutoEmptyModeEqualsDeny(t *testing.T) {
	for _, a := range []AutoConfig{{}, {Mode: AutoDeny}} {
		if !a.IsDefault() {
			t.Errorf("%+v не считается умолчанием", a)
		}
		c := Config{Policy: PolicyDirect, Download: DownloadDirect, Auto: a}
		back, _, err := Parse(Render(c))
		if err != nil {
			t.Fatalf("%+v: Parse: %v", a, err)
		}
		if !back.Auto.IsDefault() {
			t.Errorf("%+v: после круга пул перестал быть умолчательным: %+v", a, back.Auto)
		}
	}
}

// Авто-пул живёт в том же файле, что и наборы, и политика «правила из
// профиля» его отменять не должна: это разные решения. Ранний выход
// Render на PolicyProfile обязан выпускать тело авто-пула наружу.
func TestAutoSurvivesProfilePolicy(t *testing.T) {
	c := Config{Policy: PolicyProfile, Download: DownloadDirect,
		Auto: AutoConfig{Mode: AutoAllow, Nodes: []string{nodeDE}}}

	out := string(Render(c))
	if !strings.Contains(out, "proxy-providers:") {
		t.Errorf("при policy=profile авто-пул пропал из файла:\n%s", out)
	}
	if strings.Contains(out, "rule-providers:") || strings.Contains(out, "nikki-rules:") {
		t.Errorf("при policy=profile просочились наборы:\n%s", out)
	}
	got, _, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Policy != PolicyProfile || got.Auto.Mode != AutoAllow || len(got.Auto.Nodes) != 1 {
		t.Errorf("вернулось %+v", got)
	}
}

// Тело порождается из состояния и обратно не разбирается — сверяется
// только число строк. Строка фильтра, дописанная руками, при следующей
// записи исчезла бы молча.
func TestAutoBodyCountMismatchIsCorrupt(t *testing.T) {
	c := Config{Policy: PolicyDirect, Download: DownloadDirect,
		Auto: AutoConfig{Mode: AutoDeny, Nodes: []string{nodeRu}}}
	out := string(Render(c))

	// Убираем строку фильтра, оставив состояние на месте.
	var kept []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "    exclude-filter: ") {
			continue
		}
		kept = append(kept, ln)
	}
	if _, _, err := Parse([]byte(strings.Join(kept, "\n"))); !errors.Is(err, ErrCorrupt) {
		t.Errorf("тело без строки фильтра прошло как целое: %v", err)
	}
}

// Пул из нуля узлов — это «AUTO не из чего собрать». empty-fallback в
// профиле спасёт движок, но записывать заведомую бессмыслицу нельзя:
// владелец увидит пустую группу и не поймёт, чем он её опустошил.
func TestAutoEmptyPoolRefused(t *testing.T) {
	for _, m := range []AutoMode{AutoAllow, AutoProvider} {
		if err := ValidateAuto(AutoConfig{Mode: m}); err == nil {
			t.Errorf("режим %q с нулём узлов принят", m)
		}
	}
	// А «все, кроме отмеченных» с пустым списком — это весь список, и
	// это законное умолчание.
	if err := ValidateAuto(AutoConfig{Mode: AutoDeny}); err != nil {
		t.Errorf("умолчание отвергнуто: %v", err)
	}
}

func TestAutoValidateRejectsJunk(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    AutoConfig
	}{
		{"неизвестный режим", AutoConfig{Mode: "как-нибудь", Nodes: []string{nodeDE}}},
		{"пустое имя", AutoConfig{Mode: AutoAllow, Nodes: []string{""}}},
		{"дубликат", AutoConfig{Mode: AutoAllow, Nodes: []string{nodeDE, nodeDE}}},
		{"перевод строки", AutoConfig{Mode: AutoAllow, Nodes: []string{"узел\nещё"}}},
	} {
		if err := ValidateAuto(tt.c); err == nil {
			t.Errorf("%s: принято", tt.name)
		}
	}
}

// Отпечатки половин файла независимы: правка авто-пула не обязана
// отбивать чужой PUT наборов, и наоборот. Иначе две вкладки владельца
// мешали бы друг другу на ровном месте.
func TestAutoFingerprintIsIndependent(t *testing.T) {
	base := Config{Policy: PolicyDirect, Download: DownloadDirect,
		Sets: []Set{{Name: "youtube", Action: ActionTunnel}}}
	other := base
	other.Auto = AutoConfig{Mode: AutoAllow, Nodes: []string{nodeDE}}

	if Fingerprint(base) != Fingerprint(other) {
		t.Error("правка авто-пула сдвинула отпечаток наборов")
	}
	if AutoFingerprint(base.Auto) == AutoFingerprint(other.Auto) {
		t.Error("разный авто-пул дал одинаковый отпечаток")
	}
}

// Откат демона на версию без авто-пула. Файл С настроенным пулом старый
// разбор обязан отвергнуть громко: поле auto= ему незнакомо, а по
// контракту незнакомое поле состояния — это ErrCorrupt, а не «пропустим».
// Иначе старый демон переписал бы файл, молча потеряв пул. Файл БЕЗ пула
// при этом читается как раньше — поля просто нет.
func TestAutoFieldAbsentWhenDefault(t *testing.T) {
	base := Config{Policy: PolicyDirect, Download: DownloadDirect}
	if strings.Contains(string(Render(base)), autoStateField) {
		t.Error("поле авто-пула пишется при умолчании — старый демон сломается на ровном месте")
	}
	withAuto := base
	withAuto.Auto = AutoConfig{Mode: AutoAllow, Nodes: []string{nodeDE}}
	if !strings.Contains(string(Render(withAuto)), autoStateField) {
		t.Error("поле авто-пула не пишется — старый демон потеряет пул молча")
	}
}
