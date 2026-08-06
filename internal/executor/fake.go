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

	// ErrorsBySuffix — внедрение ошибки по ХВОСТУ описания вызова.
	//
	// Нужно там, где адрес записи заранее неизвестен: имя новой секции
	// генерируется случайным (ADR-0005), и точный ключ в тесте не написать.
	// Сломать при этом надо КОНКРЕТНЫЙ шаг — «запись encryption», — а не
	// первую попавшуюся команду: отказ на первом же шаге не оставляет
	// черновика и потому ничего не доказывает про его отмену.
	ErrorsBySuffix map[string]error

	// ApplyExitCodes — код возврата netmode-apply по режиму ("nikki",
	// "b4", "off"). Отсутствие ключа или 0 — успех.
	//
	// Внедряется именно КОД, а не готовая ошибка: демон различает исходы
	// переключения по коду возврата скрипта, и тест, подсовывающий свою
	// errors.New, проверял бы не тот путь, по которому пойдёт живая
	// система. Код проходит через ту же таблицу applySentinels, что и на
	// роутере, — второй копии соответствия «код → исход» нет.
	ApplyExitCodes map[string]int

	// UpstreamExitCode — код возврата netmode-wifi. 0 (значение по
	// умолчанию) — «применение выполнено».
	//
	// Одно число, а не карта по радио: станционное радио на роутере одно, и
	// ключ по нему заставил бы каждый тест повторять выведенное имя — то
	// есть хардкодить ровно то, что ADR-0019 велит выводить.
	//
	// Код, а не готовая ошибка, — по той же причине, что и у
	// ApplyExitCodes: он проходит через ту же таблицу upstreamSentinels,
	// что и на роутере.
	//
	// ОТРИЦАТЕЛЬНОЕ значение означает «процесс снят сигналом»: именно -1
	// отдаёт ExitCode() у настоящего *exec.ExitError, когда кода возврата не
	// было вовсе (дедлайн, OOM-killer, kill по ssh). Отдельного поля-флага
	// для этого нет намеренно — иначе разбор «кода нет» жил бы у фейка своей
	// жизнью, а он обязан идти той же воронкой upstreamErrorForCode.
	UpstreamExitCode int

	// Panics — вызовы, на которых фейк ПАНИКУЕТ (ключ как в Calls/reads,
	// значение — текст паники).
	//
	// Нужен ровно для одного класса проверок: что паника в теле джоба доедет
	// до владельца причиной, а не только состоянием failed. Внедрить её
	// иначе нельзя — паниковать умеет только код, а весь код между демоном и
	// системой проходит через этот интерфейс.
	//
	// Паника бросается ПОСЛЕ снятия замка (см. fail/record): брось её под
	// замком, и следующий вызов фейка встал бы навсегда на mu.Lock, а тест
	// умер бы по общему таймауту, ничего не объяснив.
	Panics map[string]string

	// Calls — журнал изменяющих вызовов в порядке поступления.
	Calls []string

	// reads — журнал читающих вызовов. Отдельно от Calls намеренно:
	// иначе проверить «что именно демон изменил» станет нельзя, журнал
	// утонет в чтениях статуса.
	reads []string

	// Три поля ниже нужны ровно для UCIRevert. Без них revert был бы
	// записью в журнал и ничем больше: следующее `uci show` возвращало бы
	// отменённую правку, и тест «отказ в середине не оставил черновика»
	// проходил бы, не проверяя ничего.

	// stagedOps — НАШИ операции по пакетам в порядке поступления. Отдельно
	// от Staged потому, что revert обязан снять только своё: в Staged может
	// лежать чужой черновик, подставленный тестом напрямую.
	stagedOps map[string][]string
	// base — снимок `uci show pkg` до первой нашей правки. Revert
	// восстанавливает состояние, перепроигрывая оставшиеся операции поверх
	// него: адресно вычесть одну правку из текста `uci show` нельзя, а
	// пересборка с нуля повторяет то, что делает настоящий uci.
	base map[string][]byte
	// foreign — чужой стейджинг, каким он был до первой нашей правки.
	foreign map[string]string

	// fixtureQueue — ответы, меняющиеся от вызова к вызову (QueueFixture).
	fixtureQueue map[string][][]byte
}

// NewFake возвращает пустой фейк.
func NewFake() *Fake {
	return &Fake{
		Fixtures:       map[string][]byte{},
		UCIValues:      map[string]string{},
		Staged:         map[string]string{},
		Errors:         map[string]error{},
		ErrorsBySuffix: map[string]error{},
		ApplyExitCodes: map[string]int{},
		Panics:         map[string]string{},
		stagedOps:      map[string][]string{},
		base:           map[string][]byte{},
		foreign:        map[string]string{},
		fixtureQueue:   map[string][][]byte{},
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

	// Паника внедряется сразу после журналирования: вызов уже виден тесту, а
	// всё, что ниже, до него не доходит — ровно так же на роутере не доходит
	// до конца операция, рухнувшая на середине.
	f.panicLocked(call)

	if err := f.injectedLocked(call); err != nil {
		return err
	}

	// Коммит очищает стейджинг; всё прочее — правка, видимая в uci show
	// сразу, как и у настоящего uci.
	if pkg, ok := strings.CutPrefix(call, "commit "); ok {
		delete(f.Staged, pkg)
		delete(f.stagedOps, pkg)
		delete(f.base, pkg)
		delete(f.foreign, pkg)
		return nil
	}
	if addr, ok := strings.CutPrefix(call, "revert "); ok {
		f.revertSection(addr)
		return nil
	}
	if pkg := pkgOf(call); pkg != "" {
		f.snapshot(pkg)
		f.applyStaged(pkg, call)
		f.stagedOps[pkg] = append(f.stagedOps[pkg], call)
		f.Staged[pkg] += call + "\n"
	}
	return nil
}

// snapshot запоминает состояние пакета до ПЕРВОЙ нашей правки.
func (f *Fake) snapshot(pkg string) {
	if _, ok := f.base[pkg]; ok {
		return
	}
	f.base[pkg] = append([]byte(nil), f.Fixtures["uci show "+pkg]...)
	f.foreign[pkg] = f.Staged[pkg]
}

// revertSection отменяет наши правки ОДНОЙ секции, оставляя остальные.
//
// Повторяет семантику `uci revert pkg.section`: чужой черновик и наши
// правки других секций остаются на месте. Именно это свойство проверяет
// граница ADR-0028 — снос по пакету выглядел бы в тестах так же, пока
// однажды не унёс бы чужую работу.
func (f *Fake) revertSection(addr string) {
	pkg, section, ok := strings.Cut(addr, ".")
	if !ok || section == "" {
		return
	}
	if _, tracked := f.base[pkg]; !tracked {
		// Своих правок не было — отменять нечего. Чужое не трогаем.
		return
	}

	kept := make([]string, 0, len(f.stagedOps[pkg]))
	for _, op := range f.stagedOps[pkg] {
		if opSection(op) != section {
			kept = append(kept, op)
		}
	}
	f.stagedOps[pkg] = kept

	f.Fixtures["uci show "+pkg] = append([]byte(nil), f.base[pkg]...)
	f.Staged[pkg] = f.foreign[pkg]
	for _, op := range kept {
		f.applyStaged(pkg, op)
		f.Staged[pkg] += op + "\n"
	}
	if f.Staged[pkg] == "" {
		delete(f.Staged, pkg)
	}
}

// opSection выхватывает имя секции из описания операции.
//
// Формы: "add-named pkg.name=type", "set pkg.sec.opt=val",
// "delete pkg.sec[.opt]". Имя секции — второй компонент адреса.
func opSection(call string) string {
	_, addr, ok := strings.Cut(call, " ")
	if !ok {
		return ""
	}
	_, rest, ok := strings.Cut(addr, ".")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, ".="); i >= 0 {
		return rest[:i]
	}
	return rest
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
	f.panicLocked(call)
	return f.injectedLocked(call)
}

// panicLocked паникует, если вызов внесён в Panics.
//
// Замок при этом удерживается, и это безопасно ровно потому, что оба
// вызывающих (record и fail) снимают его через defer, а defer отрабатывает и
// на раскрутке паники. Сними кто-нибудь из них замок вручную в конце функции
// — и первая же внедрённая паника оставила бы фейк заблокированным навсегда,
// а тест умер бы по общему таймауту, ничего не объяснив.
func (f *Fake) panicLocked(call string) {
	if msg, ok := f.Panics[call]; ok {
		panic("фейк: " + msg + " (вызов " + call + ")")
	}
}

// injectedLocked ищет внедрённую ошибку сначала по точному описанию вызова,
// потом по хвосту. Точное совпадение главнее: иначе широкий хвост, забытый в
// соседнем подтесте, молча перекрыл бы адресный ключ.
func (f *Fake) injectedLocked(call string) error {
	if err := f.Errors[call]; err != nil {
		return err
	}
	for suffix, err := range f.ErrorsBySuffix {
		if strings.HasSuffix(call, suffix) {
			return err
		}
	}
	return nil
}

// Reads возвращает журнал читающих вызовов. Нужен, чтобы доказать, что
// кэш статуса действительно избавляет от запусков процессов, а не просто
// возвращает то же значение.
func (f *Fake) Reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

// QueueFixture задаёт ПОСЛЕДОВАТЕЛЬНОСТЬ ответов на один и тот же вызов:
// первый элемент уходит первому вызову, последний — всем остальным.
//
// Нужен там, где смысл проверки — в РАЗНИЦЕ между двумя чтениями одного
// источника. Пример, ради которого он и заведён: «прежний ssid» снимается
// до записи, «нынешний» — в ожидании после применения, и вердикт
// stayed_on_previous отличается от other_ssid ровно тем, совпали они или
// нет. С одним неподвижным ответом обе ветки неразличимы, и тест на них
// проверял бы только то, что код не паникует.
//
// Последний элемент липнет намеренно: опрос идёт до истечения окна, число
// итераций зависит от планировщика, и очередь, кончающаяся пустотой,
// сделала бы исход зависимым от того, сколько раз успели спросить.
func (f *Fake) QueueFixture(key string, bodies ...[]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fixtureQueue[key] = bodies
}

func (f *Fake) fixture(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.fixtureQueue[key]; len(q) > 0 {
		if len(q) > 1 {
			f.fixtureQueue[key] = q[1:]
		}
		return q[0]
	}
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
	// Та же функция, что и в реальной реализации: фейк, принимающий
	// disabled=true, учил бы тесты писать значение, которое uci прочитает
	// как ВКЛЮЧЕНО (ADR-0004).
	if err := forbidDisabledValue(pkg, option, value); err != nil {
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
	// Запрет ADR-0026 держится той же функцией, что и в реальной реализации.
	// Фейк, разрешающий запрещённое, учит тесты неправде: зелёный тест
	// доказывал бы путь, которого на роутере не существует.
	if err := forbidDeleteDisabled(pkg, option); err != nil {
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

func (f *Fake) UCIRevert(ctx context.Context, pkg, section string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Имя секции проверяется той же функцией, что и в реальной реализации:
	// пустое имя обязано отвергаться и здесь, иначе тест доказывал бы
	// границу ADR-0028 на фейке, который её не держит.
	for kind, s := range map[string]string{"пакет": pkg, "секция": section} {
		if err := validateName(kind, s); err != nil {
			return err
		}
	}
	return f.record("revert " + pkg + "." + section)
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

func (f *Fake) ApplyUpstream(ctx context.Context, t UpstreamTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Та же проверка, что и в реальной реализации: фейк, принимающий имя,
	// которое роутер отвергнет кодом 1, врал бы ровно там, где тест ищет
	// правду.
	if err := validateRadioName(t.Radio); err != nil {
		return err
	}
	// Вызов журналируется до отказа: на роутере скрипт тоже запускается, и
	// тест обязан видеть попытку, а не только её результат.
	if err := f.record("apply-upstream " + t.Radio); err != nil {
		return err
	}
	f.mu.Lock()
	code := f.UpstreamExitCode
	f.mu.Unlock()
	if code == 0 {
		return nil
	}
	// Текст повторяет форму того, что соберёт реальная реализация: имя
	// скрипта, аргумент, код возврата.
	cause := fmt.Errorf("netmode-wifi %s: exit status %d", t.Radio, code)
	if code < 0 {
		// Отрицательного КОДА у настоящего процесса не бывает: -1 отдаёт
		// ExitCode() ровно тогда, когда процесс сняли сигналом и кода не
		// было вовсе. Текст обязан выглядеть так же, иначе тест доказывал бы
		// разбор строки «exit status -1», которой на роутере не напечатает
		// никто.
		cause = fmt.Errorf("netmode-wifi %s: signal: killed", t.Radio)
	}
	return upstreamErrorForCode(code, cause)
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
