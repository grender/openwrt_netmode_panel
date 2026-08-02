// Package job — сериализация медленных операций.
//
// Медленные операции (смена режима, обновление подписки) идут по одной.
// Второй запрос **отбивается**, а не ставится в очередь (SPEC §6): очередь
// означала бы, что пользователь нажал кнопку, ушёл, а операция началась
// через минуту — когда обстановка уже другая.
//
// Быстрые операции (выбор узла Nikki, сета b4) сюда не попадают вовсе:
// провести их через джобы значило бы получить мигающий светодиод и
// секундную задержку там, где должен быть мгновенный отклик (SPEC §5).
package job

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBusy — уже идёт другая операция.
var ErrBusy = errors.New("job: уже идёт другая операция")

// State — состояние операции.
type State string

const (
	Running State = "running"
	Done    State = "done"
	Failed  State = "failed"
)

// Job — снимок операции для /api/status.
//
// Kind и Arg — машинное описание операции, по нему панель строит подпись на
// своём языке. Label — та же операция человеческими словами по-русски: он
// уходит в журнал и в диагностику по ssh, где русский уместен, и панелью не
// показывается. Словарь на роутере был бы копией web/i18n.js — вторым местом,
// где строки разъезжаются.
type Job struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Arg — уточнение вида операции: для kind="mode" это nikki|b4|off,
	// для kind="subscription" уточнять нечего и поле пустое.
	Arg        string  `json:"arg"`
	Label      string  `json:"label"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	ETASec     int     `json:"eta_sec"`
	State      State   `json:"state"`
	Error      *string `json:"error"`
}

// keepFinished — сколько показывать завершённую операцию.
//
// Панель опрашивает статус раз в секунду; без задержки результат
// быстрой операции мелькнул бы и исчез, не успев прочитаться.
const keepFinished = 5 * time.Second

// Manager хранит текущую операцию. Единственный на весь демон.
type Manager struct {
	mu      sync.Mutex
	current *Job
	// doneAt — когда операция завершилась; нужен, чтобы убрать её из
	// статуса не сразу.
	doneAt time.Time

	now    func() time.Time
	nextID func() string
}

func NewManager() *Manager {
	var n uint64
	return &Manager{
		now: time.Now,
		nextID: func() string {
			n++
			return "j-" + itoa36(n)
		},
	}
}

// Start запускает операцию в фоне.
//
// Возвращает снимок запущенной операции либо ErrBusy. Функция fn получает
// контекст, живущий дольше HTTP-запроса: клиент может уйти, а смена режима
// обязана довестись до конца — брошенная на середине, она оставила бы
// висячие цепочки в nftables.
func (m *Manager) Start(kind, arg, label string, etaSec int, fn func(context.Context) error) (Job, error) {
	m.mu.Lock()
	if m.current != nil && m.current.State == Running {
		m.mu.Unlock()
		return Job{}, ErrBusy
	}

	now := m.now()
	j := &Job{
		ID:        m.nextID(),
		Kind:      kind,
		Arg:       arg,
		Label:     label,
		StartedAt: now.UTC().Format(time.RFC3339),
		ETASec:    etaSec,
		State:     Running,
	}
	m.current = j
	// Снимок снимается ПОД замком: после старта горутина начнёт менять j,
	// и копирование снаружи оказалось бы гонкой.
	snapshot := *j
	m.mu.Unlock()

	go m.run(j, fn)
	return snapshot, nil
}

func (m *Manager) run(j *Job, fn func(context.Context) error) {
	// Своя отмена, не от запроса: операция доводится до конца, даже если
	// вкладку закрыли.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	err := fn(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	fin := m.now().UTC().Format(time.RFC3339)
	j.FinishedAt = &fin
	m.doneAt = m.now()
	if err != nil {
		j.State = Failed
		s := err.Error()
		j.Error = &s
		return
	}
	j.State = Done
}

// Current возвращает текущую или недавно завершённую операцию.
func (m *Manager) Current() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return nil
	}
	if m.current.State != Running && m.now().Sub(m.doneAt) > keepFinished {
		m.current = nil
		return nil
	}
	cp := *m.current
	return &cp
}

// Busy сообщает, идёт ли операция прямо сейчас.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current != nil && m.current.State == Running
}

// Wait ждёт завершения текущей операции. Только для тестов: в рантайме
// ждать нечего — панель узнаёт результат из опроса статуса.
func (m *Manager) Wait(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.Busy() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// itoa36 — короткий идентификатор без внешних зависимостей.
func itoa36(n uint64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var b [13]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%36]
		n /= 36
	}
	return string(b[i:])
}
