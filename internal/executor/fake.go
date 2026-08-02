package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Fake — исполнитель для тестов. Отвечает записанным выводом живого
// роутера и записывает всё, что у него просили изменить.
//
// Именно он делает выполнимой цель SPEC §10: вся логика режимов, джобов и
// планировщика гоняется на ноуте без роутера.
//
// Фикстуры, а не рукописные структуры: рукописный мок повторяет нашу же
// догадку о форме данных и проверяет её сам против себя. Записанный вывод
// ловит то, чего не придумаешь, — например, что три сети из четырнадцати
// приходят вообще без поля ssid (docs/recon/ubus.md).
type Fake struct {
	mu sync.Mutex

	// Fixtures — сырые ответы: ключи "uci show wireless",
	// "ubus network.wireless status" и т.п.
	Fixtures map[string][]byte

	// UCIValues — ответы UCIGet, ключ "pkg.section.option".
	UCIValues map[string]string

	// Staged — вывод `uci changes` по пакетам. Пусто = стейджинг чист.
	Staged map[string]string

	// Errors — внедрение ошибок по описанию вызова (как в Calls).
	// Без этого нельзя проверить деградацию, которой требует SPEC §10.
	Errors map[string]error

	// ApplyExitCodes — код возврата netmode-apply по режиму ("nikki",
	// "b4", "off"). Отсутствие ключа или 0 — успех.
	//
	// Внедряется именно КОД, а не готовая ошибка: демон различает исходы
	// переключения по коду возврата скрипта, и тест, подсовывающий свою
	// errors.New, проверял бы не тот путь, по которому пойдёт живая
	// система. Код проходит через ту же таблицу applySentinels, что и на
	// роутере, — второй копии соответствия «код → исход» нет.
	ApplyExitCodes map[string]int

	// Calls — журнал изменяющих вызовов в порядке поступления.
	Calls []string

	// reads — журнал читающих вызовов. Отдельно от Calls намеренно:
	// иначе проверить «что именно демон изменил» станет нельзя, журнал
	// утонет в чтениях статуса.
	reads []string
}

// NewFake возвращает пустой фейк.
func NewFake() *Fake {
	return &Fake{
		Fixtures:       map[string][]byte{},
		UCIValues:      map[string]string{},
		Staged:         map[string]string{},
		Errors:         map[string]error{},
		ApplyExitCodes: map[string]int{},
	}
}

// fixtureFiles — соответствие «ключ вызова → файл разведки».
// Список явный: молчаливое сопоставление по маске однажды подсунет
// не тот файл, и тест пройдёт не на тех данных.
var fixtureFiles = map[string]string{
	"uci show wireless":                  "10-uci-show-wireless.txt",
	"uci show network":                   "11-uci-show-network.txt",
	"ubus network.wireless status":       "21-ubus-network-wireless-status.json",
	"ubus iwinfo scan":                   "23-ubus-iwinfo-scan.json",
	"ubus iwinfo info":                   "24-ubus-iwinfo-info.json",
	"ubus network.interface.wwan status": "26-ubus-network-interface-wwan.json",
}

// LoadFixtures заполняет фейк выводом роутера из docs/recon/raw.
// Падает через t.Fatal: пустой фейк порождает тесты, которые ничего
// не проверяют, и молчать об этом нельзя.
func (f *Fake) LoadFixtures(t *testing.T, dir string) {
	t.Helper()
	if err := f.loadFixtures(dir); err != nil {
		t.Fatalf("фикстуры: %v", err)
	}
}

// LoadFixturesDir — вариант без testing.T: нужен инструменту разработки
// cmd/netmoded-dev, который поднимает НАСТОЯЩИЕ обработчики против
// записанного вывода роутера.
func (f *Fake) LoadFixturesDir(dir string) error {
	return f.loadFixtures(dir)
}

func (f *Fake) loadFixtures(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	loaded := 0
	for key, name := range fixtureFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		f.Fixtures[key] = b
		loaded++
	}
	if loaded == 0 {
		return fmt.Errorf("в %s не найдено ни одной фикстуры — тесты проверяли бы пустоту", dir)
	}
	return nil
}

// record журналирует изменяющий вызов, отражает его в читаемом состоянии
// и отдаёт внедрённую ошибку, если есть.
func (f *Fake) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, call)

	if err := f.Errors[call]; err != nil {
		return err
	}

	// Коммит очищает стейджинг; всё прочее — правка, видимая в uci show
	// сразу, как и у настоящего uci.
	if pkg, ok := strings.CutPrefix(call, "commit "); ok {
		delete(f.Staged, pkg)
		return nil
	}
	if pkg := pkgOf(call); pkg != "" {
		f.applyStaged(pkg, call)
		f.Staged[pkg] += call + "\n"
	}
	return nil
}

// pkgOf выхватывает имя пакета из описания вызова.
func pkgOf(call string) string {
	i := strings.IndexByte(call, ' ')
	if i < 0 {
		return ""
	}
	rest := call[i+1:]
	if j := strings.IndexByte(rest, '.'); j > 0 {
		return rest[:j]
	}
	return ""
}

// fail журналирует читающий вызов и отдаёт внедрённую ошибку, если есть.
// Чтения копятся отдельно от Calls: смешав их, нельзя было бы проверить,
// что именно демон изменил.
func (f *Fake) fail(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, call)
	return f.Errors[call]
}

// Reads возвращает журнал читающих вызовов. Нужен, чтобы доказать, что
// кэш статуса действительно избавляет от запусков процессов, а не просто
// возвращает то же значение.
func (f *Fake) Reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

func (f *Fake) fixture(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Fixtures[key]
}

func (f *Fake) UCIShow(ctx context.Context, pkg string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := "uci show " + pkg
	if err := f.fail(key); err != nil {
		return nil, err
	}
	// Отсутствие фикстуры = пакета нет. Свежая установка выглядит именно так.
	return f.fixture(key), nil
}

func (f *Fake) UCIGet(ctx context.Context, pkg, section, option string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := pkg + "." + section + "." + option
	if err := f.fail("uci get " + key); err != nil {
		return "", err
	}
	f.mu.Lock()
	v, ok := f.UCIValues[key]
	f.mu.Unlock()
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *Fake) UCIChanges(ctx context.Context, pkg string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.fail("uci changes " + pkg); err != nil {
		return nil, err
	}
	f.mu.Lock()
	s := f.Staged[pkg]
	f.mu.Unlock()
	if s == "" {
		return nil, nil
	}
	return []byte(s), nil
}

func (f *Fake) UCIAddNamed(ctx context.Context, pkg, name, sectionType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for kind, s := range map[string]string{"пакет": pkg, "секция": name, "тип": sectionType} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	return f.record(fmt.Sprintf("add-named %s.%s=%s", pkg, name, sectionType))
}

func (f *Fake) UCISet(ctx context.Context, pkg, section, option, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for kind, s := range map[string]string{"пакет": pkg, "секция": section, "опция": option} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	if err := validateValue(value); err != nil {
		return err
	}
	return f.record(fmt.Sprintf("set %s.%s.%s=%s", pkg, section, option, value))
}

func (f *Fake) UCIDelete(ctx context.Context, pkg, section, option string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := pkg + "." + section
	if option != "" {
		target += "." + option
	}
	return f.record("delete " + target)
}

func (f *Fake) UCICommit(ctx context.Context, pkg string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.record("commit " + pkg)
}

func (f *Fake) UbusCall(ctx context.Context, object, method string, args map[string]any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Фейк проверяет объект той же функцией, что и реальная реализация:
	// иначе тест пройдёт на вызове, который на роутере будет отвергнут.
	if err := validateUbusObject(object); err != nil {
		return nil, err
	}
	key := "ubus " + object + " " + method
	if err := f.fail(key); err != nil {
		return nil, err
	}
	b := f.fixture(key)
	if b == nil {
		return nil, fmt.Errorf("нет фикстуры для %q", key)
	}
	return b, nil
}

func (f *Fake) ApplyMode(ctx context.Context, mode string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validModes[mode] {
		return fmt.Errorf("неизвестный режим %q (допустимы nikki, b4, off)", mode)
	}
	// Вызов журналируется до отказа: на роутере скрипт тоже запускается,
	// и тест обязан видеть попытку, а не только её результат.
	if err := f.record("apply-mode " + mode); err != nil {
		return err
	}
	f.mu.Lock()
	code := f.ApplyExitCodes[mode]
	f.mu.Unlock()
	if code == 0 {
		return nil
	}
	// Текст повторяет форму того, что соберёт реальная реализация:
	// имя скрипта, аргумент, код возврата.
	cause := fmt.Errorf("netmode-apply %s: exit status %d", mode, code)
	return applyErrorForCode(code, cause)
}

func (f *Fake) UpdateSubscription(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.record("update-subscription"); err != nil {
		return nil, err
	}
	return f.fixture("subscription-output"), nil
}

// CallsContaining — помощник для тестов: вызовы, содержащие подстроку.
func (f *Fake) CallsContaining(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

// ─────────── отражение записей в чтениях ───────────
//
// Без этого фейк был бы односторонним: запись фиксируется в журнале, но
// следующее чтение возвращает исходную фикстуру, будто ничего не было.
// Тест «создали сеть — она появилась в списке» тогда невозможен, а
// инструмент разработки показывает неправду о собственном поведении.
//
// Семантика повторяет UCI: правки копятся в стейджинге и становятся
// видны в `uci show` сразу (uci так и делает), а `uci changes` очищается
// коммитом.

// applyStaged переписывает фикстуру `uci show pkg`, применяя одну операцию.
func (f *Fake) applyStaged(pkg, line string) {
	key := "uci show " + pkg
	cur := string(f.Fixtures[key])

	switch {
	case strings.HasPrefix(line, "add-named "):
		// add-named wireless.up_x=wifi-iface
		body := strings.TrimPrefix(line, "add-named ")
		cur = appendLine(cur, pkg+"."+strings.TrimPrefix(body, pkg+"."))

	case strings.HasPrefix(line, "set "):
		// set wireless.up_x.ssid=Имя
		body := strings.TrimPrefix(line, "set ")
		eq := strings.Index(body, "=")
		if eq < 0 {
			return
		}
		addr, val := body[:eq], body[eq+1:]
		cur = setOption(cur, addr, val)

	case strings.HasPrefix(line, "delete "):
		target := strings.TrimPrefix(line, "delete ")
		cur = deleteLines(cur, target)
	}

	f.Fixtures[key] = []byte(cur)
}

func appendLine(cur, line string) string {
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	return cur + line + "\n"
}

// setOption заменяет строку опции либо дописывает её.
func setOption(cur, addr, val string) string {
	quoted := addr + "='" + val + "'"
	lines := strings.Split(cur, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, addr+"=") {
			lines[i] = quoted
			return strings.Join(lines, "\n")
		}
	}
	return appendLine(cur, quoted)
}

// deleteLines убирает секцию целиком либо одну опцию.
func deleteLines(cur, target string) string {
	var out []string
	for _, l := range strings.Split(cur, "\n") {
		// Секция: убираем и её объявление, и все её опции.
		if l == target+"=" || strings.HasPrefix(l, target+"=") || strings.HasPrefix(l, target+".") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
