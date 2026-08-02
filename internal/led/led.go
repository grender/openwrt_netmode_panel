// Package led управляет индикацией режима.
//
// Мигание делается штатным триггером ядра `timer` с delay_on/delay_off,
// а не горутиной (SPEC §8). Горутина мигала бы только пока жив процесс:
// демон упал — светодиод замер в случайной фазе и врёт. Триггер ядра
// переживает и падение демона, и его перезапуск.
//
// Разведка (docs/recon/raw/30-leds-and-phy.txt) подтвердила: `timer`
// доступен, оба светодиода сейчас ничьи, а секций `config led` в
// /etc/config/system нет вовсе — драться не с кем.
//
// Политика: индикация — best-effort. Ошибка записи логируется и глотается,
// но НИКОГДА не валит операцию (ADR-0013). Погасший светодиод — неудобство;
// сорванное переключение режима из-за него — потеря связи.
package led

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"netmoded/internal/safe"
)

// DefaultRoot — каталог светодиодов в sysfs.
const DefaultRoot = "/sys/class/leds"

// Имена светодиодов на GL-MT3600BE (raw/30-leds-and-phy.txt).
const (
	Blue  = "blue:status"
	White = "white:status"
)

// Период мигания при применении режима и при ошибке.
const (
	applyOnMS  = 300
	applyOffMS = 300
	errorOnMS  = 100
	errorOffMS = 100
	// errorHold — сколько держится индикация ошибки (SPEC §8).
	errorHold = 3 * time.Second
)

// State — что показывают светодиоды.
type State string

const (
	Off           State = "off"            // оба погашены
	Nikki         State = "nikki"          // синий ровно
	B4            State = "b4"             // белый ровно
	ApplyingNikki State = "applying_nikki" // синий мигает
	ApplyingB4    State = "applying_b4"    // белый мигает
	ApplyingOff   State = "applying_off"   // синий мигает: цель — погасить
)

// StateForMode возвращает индикацию для установившегося режима.
func StateForMode(mode string) State {
	switch mode {
	case "nikki":
		return Nikki
	case "b4":
		return B4
	default:
		return Off
	}
}

// ApplyingForMode возвращает индикацию на время переключения.
func ApplyingForMode(mode string) State {
	switch mode {
	case "nikki":
		return ApplyingNikki
	case "b4":
		return ApplyingB4
	default:
		return ApplyingOff
	}
}

// Controller пишет в sysfs.
type Controller struct {
	root string
	logf func(format string, args ...any)

	mu sync.Mutex
	// revert — одноразовый таймер возврата после индикации ошибки.
	// У него есть владелец и Stop под тем же мьютексом, что его взвёл.
	revert *time.Timer
	// degraded — запись перестала совпадать с прочитанным; дальше не
	// трогаем светодиоды и не засоряем лог.
	degraded bool
}

// New возвращает контроллер. Пустой root — путь по умолчанию.
func New(root string, logf func(string, ...any)) *Controller {
	if root == "" {
		root = DefaultRoot
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Controller{root: root, logf: logf}
}

// Set выставляет индикацию.
//
// Ошибка возвращается для журнала, но вызывающий обязан её проглотить.
// Логирование и признак деградации — забота контроллера: Set зовётся при
// КАЖДОМ переключении режима, и если бы вызывающий писал в журнал сам, на
// железе без нужных светодиодов лог заполнился бы одинаковыми строками.
func (c *Controller) Set(s State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelRevertLocked()
	return c.applyNotedLocked(s, "установка индикации")
}

// Flash показывает ошибку три секунды, затем возвращает индикацию `then`.
//
// Возврат делается одноразовым таймером, а не горутиной с циклом:
// мигание всё это время ведёт ядро, нам остаётся только вернуть состояние.
func (c *Controller) Flash(then State) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cancelRevertLocked()
	if err := c.blinkBothLocked(errorOnMS, errorOffMS); err != nil {
		c.noteLocked("индикация ошибки: %v", err)
	}

	// Коллбэк исполняется в отдельной горутине, порождённой рантаймом
	// таймера, через три секунды после Flash: паника там рванула бы «на
	// ровном месте», когда владелец давно отпустил кнопку, и унесла бы с
	// собой весь демон — HTTP API, расписание и управление режимом.
	//
	// Замок берётся ВНУТРИ safe.Do намеренно. Так его снимает defer
	// внутреннего кадра, то есть раскрутка проходит через Unlock ДО того,
	// как перехватчик получит управление и начнёт писать стек через logf.
	// Обратный порядок (замок снаружи) оставил бы мьютекс удержанным на всё
	// время логирования, а если бы logf когда-нибудь заглянул в сам
	// контроллер — например, за Degraded для строки статуса, — то навсегда:
	// индикация замерла бы молча, и следующий Set повис бы вместе с
	// переключением режима.
	//
	// Ошибка не возвращается, потому что возвращать её некому: причину со
	// стеком safe.Do уже положил в журнал, а индикация — best-effort
	// (ADR-0013).
	c.revert = time.AfterFunc(errorHold, func() {
		_ = safe.Do(c.logf, "отложенный возврат индикации", func() error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.revert = nil
			return c.applyNotedLocked(then, "возврат индикации")
		})
	})
}

// Close останавливает отложенный возврат.
//
// Без него таймер пережил бы остановку демона и попытался писать в sysfs
// после закрытия — тихая утечка, которую заметишь только по логу.
func (c *Controller) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelRevertLocked()
}

func (c *Controller) cancelRevertLocked() {
	if c.revert != nil {
		c.revert.Stop()
		c.revert = nil
	}
}

// ErrUnknownState — состояние, которого нет в applyLocked.
//
// Отделено от ошибок записи намеренно: это баг вызывающего кода, а не
// отсутствие железа. Уйти из-за него в деградацию значило бы навсегда
// замолчать про настоящие проблемы sysfs — noteLocked идемпотентен и
// второй раз уже ничего не скажет.
var ErrUnknownState = errors.New("led: неизвестное состояние")

// applyNotedLocked применяет состояние и, если оно не далось из-за железа,
// один раз сообщает об этом и уходит в деградацию.
func (c *Controller) applyNotedLocked(s State, what string) error {
	err := c.applyLocked(s)
	if err != nil && !errors.Is(err, ErrUnknownState) {
		c.noteLocked("%s %s: %v", what, s, err)
	}
	return err
}

func (c *Controller) applyLocked(s State) error {
	switch s {
	case Nikki:
		return c.both(c.solid(Blue), c.dark(White))
	case B4:
		return c.both(c.dark(Blue), c.solid(White))
	case Off:
		return c.both(c.dark(Blue), c.dark(White))
	case ApplyingNikki:
		return c.both(c.blink(Blue, applyOnMS, applyOffMS), c.dark(White))
	case ApplyingB4:
		return c.both(c.dark(Blue), c.blink(White, applyOnMS, applyOffMS))
	case ApplyingOff:
		return c.both(c.blink(Blue, applyOnMS, applyOffMS), c.dark(White))
	default:
		return fmt.Errorf("%w %q", ErrUnknownState, s)
	}
}

func (c *Controller) blinkBothLocked(on, off int) error {
	return c.both(c.blink(Blue, on, off), c.blink(White, on, off))
}

// both возвращает первую ошибку, но выполняет обе операции: погасить один
// светодиод и бросить второй в прежнем состоянии — хуже, чем попытаться оба.
func (c *Controller) both(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// solid — ровное свечение: триггер снимается, яркость на максимум.
func (c *Controller) solid(name string) error {
	if err := c.write(name, "trigger", "none"); err != nil {
		return err
	}
	max := c.maxBrightness(name)
	return c.write(name, "brightness", strconv.Itoa(max))
}

func (c *Controller) dark(name string) error {
	if err := c.write(name, "trigger", "none"); err != nil {
		return err
	}
	return c.write(name, "brightness", "0")
}

// blink — мигание штатным триггером ядра.
//
// Порядок важен: delay_on и delay_off появляются в sysfs только ПОСЛЕ
// установки триггера timer. Записать их раньше — получить ENOENT.
func (c *Controller) blink(name string, on, off int) error {
	if err := c.write(name, "trigger", "timer"); err != nil {
		return err
	}
	if err := c.write(name, "delay_on", strconv.Itoa(on)); err != nil {
		return err
	}
	return c.write(name, "delay_off", strconv.Itoa(off))
}

// maxBrightness читает предел яркости; при неудаче — 1.
//
// Единица безопасна: для большинства светодиодов это и есть максимум,
// а «слишком тускло» лучше, чем «не горит».
func (c *Controller) maxBrightness(name string) int {
	b, err := os.ReadFile(filepath.Join(c.root, name, "max_brightness"))
	if err != nil {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func (c *Controller) write(name, attr, value string) error {
	if c.degraded {
		return nil
	}
	path := filepath.Join(c.root, name, attr)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		return fmt.Errorf("запись %s: %w", path, err)
	}
	return nil
}

// noteLocked логирует один раз и переводит контроллер в деградацию.
//
// Иначе неудачная запись повторялась бы при каждом переключении и
// засоряла журнал одинаковыми строками, среди которых потеряется
// что-то важное.
func (c *Controller) noteLocked(format string, args ...any) {
	if c.degraded {
		return
	}
	c.degraded = true
	c.logf("led: индикация отключена — "+format, args...)
}

// Degraded сообщает, отказался ли контроллер от работы со светодиодами.
// Попадает в /api/status, чтобы «почему не горит» не приходилось гадать.
func (c *Controller) Degraded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.degraded
}
