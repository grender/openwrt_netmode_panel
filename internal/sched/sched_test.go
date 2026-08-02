package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"netmoded/internal/logs"
)

type fakeUpdater struct {
	mu     sync.Mutex
	out    []byte
	err    error
	calls  int
	block  chan struct{}
	onCall func()
}

func (f *fakeUpdater) UpdateSubscription(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	blk, onCall := f.block, f.onCall
	out, err := f.out, f.err
	f.mu.Unlock()

	if onCall != nil {
		onCall()
	}
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, err
}

func (f *fakeUpdater) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newSched(t *testing.T, up *fakeUpdater) (*Scheduler, *logs.Log) {
	t.Helper()
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	return New(up, l, time.Hour, nil), l
}

// recorder собирает то, что демон написал бы в свой лог.
type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// brokenLogPath возвращает путь, по которому запись журнала гарантированно
// провалится: каталог создать нельзя, потому что на его месте файл.
func brokenLogPath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	return filepath.Join(blocker, "updates.log")
}

func TestSuccessfulUpdateIsLogged(t *testing.T) {
	up := &fakeUpdater{out: []byte("parsed 42 nodes, provider updated\n")}
	s, l := newSched(t, up)

	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 42 {
		t.Errorf("узлов %d, ожидалось 42", n)
	}

	last, ok, _ := l.Last()
	if !ok {
		t.Fatal("запись в журнал не сделана")
	}
	if last.Status != logs.StatusOK || last.Nodes != 42 || last.Err != "" {
		t.Errorf("запись: %+v", last)
	}
}

// Предохранитель happ2clash: при нуле разобранных узлов старый файл
// провайдера НЕ перезаписывается (SPEC §2). Для нас это неудача, а не
// успех с нулём — иначе панель показала бы «обновлено, 0 узлов», хотя
// список остался прежним.
func TestEmptyResultIsFailureNotSuccess(t *testing.T) {
	up := &fakeUpdater{out: []byte("parsed 0 nodes, keeping previous file\n")}
	s, l := newSched(t, up)

	n, err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("пустой результат выдан за успех")
	}
	if n != 0 {
		t.Errorf("узлов %d, ожидалось 0", n)
	}

	last, ok, _ := l.Last()
	if !ok {
		t.Fatal("запись не сделана")
	}
	if last.Status != logs.StatusFail {
		t.Errorf("статус %q, ожидался fail", last.Status)
	}
	if last.Err == "" {
		t.Error("причина не записана — по журналу будет непонятно, что случилось")
	}
}

func TestConverterErrorIsLogged(t *testing.T) {
	up := &fakeUpdater{err: errors.New("happ2clash: подписка недоступна")}
	s, l := newSched(t, up)

	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("ошибка конвертера проглочена")
	}

	last, _, _ := l.Last()
	if last.Status != logs.StatusFail {
		t.Errorf("статус %q", last.Status)
	}
	if last.Err == "" {
		t.Error("текст ошибки не сохранён")
	}
}

// Пересечение реально: расписание сработало ровно тогда, когда владелец
// нажал «обновить сейчас».
func TestConcurrentRunIsRefused(t *testing.T) {
	up := &fakeUpdater{out: []byte("41 nodes"), block: make(chan struct{})}
	s, _ := newSched(t, up)

	started := make(chan struct{})
	up.onCall = func() { close(started) }

	go func() { _, _ = s.RunOnce(context.Background()) }()
	<-started

	if _, err := s.RunOnce(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("второй запуск: %v, ожидалась ErrAlreadyRunning", err)
	}

	close(up.block)
	// Дожидаемся завершения первого.
	for i := 0; i < 200 && s.Running(); i++ {
		time.Sleep(2 * time.Millisecond)
	}
	if up.count() != 1 {
		t.Errorf("конвертер вызван %d раз, ожидался один", up.count())
	}
}

// Демон перезапускается при каждом обновлении прошивки и при отладке.
// Обновлять подписку на каждый перезапуск значило бы дёргать провайдера
// почём зря.
func TestNoUpdateOnStart(t *testing.T) {
	up := &fakeUpdater{out: []byte("41 nodes")}
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	s := New(up, l, 50*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	time.Sleep(20 * time.Millisecond) // меньше интервала
	if got := up.count(); got != 0 {
		t.Errorf("на старте выполнено %d обновлений, ожидалось 0", got)
	}

	time.Sleep(70 * time.Millisecond) // интервал прошёл
	if got := up.count(); got == 0 {
		t.Error("по расписанию обновление не выполнилось")
	}
	cancel()
}

func TestStopEndsLoop(t *testing.T) {
	up := &fakeUpdater{out: []byte("41 nodes")}
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	s := New(up, l, 20*time.Millisecond, nil)

	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()

	s.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run не завершился после Stop")
	}
	// Повторный Stop не должен паниковать.
	s.Stop()
}

func TestParseNodeCount(t *testing.T) {
	tests := []struct {
		out  string
		want int
	}{
		{"parsed 42 nodes", 42},
		{"Разобрано 14 узлов", 14},
		{"wrote 7 proxies to sub.yaml", 7},
		{"0 nodes parsed", 0},
		{"", 0},
		// Не нашли числа — ноль, и это будет трактовано как неудача.
		// Лучше лишний раз сказать «не получилось», чем показать успех,
		// которого не было.
		{"готово", 0},
		{"всё хорошо, файл записан", 0},
	}
	for _, tt := range tests {
		if got := ParseNodeCount([]byte(tt.out)); got != tt.want {
			t.Errorf("ParseNodeCount(%q) = %d, ожидалось %d", tt.out, got, tt.want)
		}
	}
}

func TestLastRunRecorded(t *testing.T) {
	up := &fakeUpdater{out: []byte("41 nodes")}
	s, _ := newSched(t, up)

	if !s.LastRun().IsZero() {
		t.Error("до первого запуска LastRun должен быть нулевым")
	}
	_, _ = s.RunOnce(context.Background())
	if s.LastRun().IsZero() {
		t.Error("LastRun не проставлен")
	}
}

// Контракт logs.Append: сбой записи журнала не отменяет сделанного.
// Подписка уже обновилась, и вернуть ошибку значило бы покрасить успешное
// обновление в «неудачу». Ошибка обязана попасть в лог демона, а не пропасть.
func TestLogWriteFailureIsLoggedNotReturned(t *testing.T) {
	rec := &recorder{}
	up := &fakeUpdater{out: []byte("parsed 42 nodes, provider updated\n")}
	s := New(up, logs.New(brokenLogPath(t)), time.Hour, rec.logf)

	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Errorf("подписка обновилась, но RunOnce вернул ошибку: %v", err)
	}
	if n != 42 {
		t.Errorf("узлов %d, ожидалось 42", n)
	}
	if rec.count() == 0 {
		t.Error("ошибка записи журнала потеряна: наружу не отдана и в лог не попала")
	}
}

// На пути неудачи поведение не меняется: ошибка конвертера возвращается
// независимо от того, записался журнал или нет.
func TestLogWriteFailureKeepsConverterError(t *testing.T) {
	rec := &recorder{}
	up := &fakeUpdater{err: errors.New("happ2clash: подписка недоступна")}
	s := New(up, logs.New(brokenLogPath(t)), time.Hour, rec.logf)

	err := func() error { _, e := s.RunOnce(context.Background()); return e }()
	if err == nil || err.Error() != "happ2clash: подписка недоступна" {
		t.Errorf("ошибка конвертера подменена или проглочена: %v", err)
	}
	if rec.count() == 0 {
		t.Error("сбой журнала не залогирован")
	}
}

// Пустой logf не должен ронять демона: New обязан подставить заглушку.
func TestNilLogfIsSafe(t *testing.T) {
	up := &fakeUpdater{out: []byte("parsed 42 nodes")}
	s := New(up, logs.New(brokenLogPath(t)), time.Hour, nil)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Errorf("RunOnce: %v", err)
	}
}

// Демон перезапускается часто (sysupgrade, watchdog, отладка). Тикер «с
// нуля» откладывал бы автообновление месяцами, поэтому первый интервал
// отсчитывается от последней записи журнала.
func TestFirstDelayFromLog(t *testing.T) {
	const interval = 12 * time.Hour
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		last time.Time
		want time.Duration
	}{
		{"просрочено на час — догоняем", now.Add(-13 * time.Hour), catchUpDelay},
		{"интервал ровно истёк — догоняем", now.Add(-interval), catchUpDelay},
		{"обновлялись час назад — ждём остаток", now.Add(-time.Hour), 11 * time.Hour},
		{"обновились только что — ждём весь интервал", now, interval},
		// Часов у роутера нет, до NTP они уходят в прошлое: запись
		// оказывается «из будущего». Ждать разницу значило бы отложить
		// обновление на годы.
		{"запись из будущего — обычный интервал", now.Add(48 * time.Hour), interval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
			if err := l.Append(logs.Entry{TS: tt.last, Nodes: 41, Status: logs.StatusOK}); err != nil {
				t.Fatalf("подготовка журнала: %v", err)
			}

			s := New(&fakeUpdater{}, l, interval, nil)
			s.now = func() time.Time { return now }

			if got := s.firstDelay(); got != tt.want {
				t.Errorf("задержка %v, ожидалась %v", got, tt.want)
			}
		})
	}
}

// Свежая установка не должна дёргать провайдера до того, как владелец
// что-либо настроил: пустой и нечитаемый журнал — это полный интервал,
// а не «догнать немедленно».
func TestFirstDelayWithoutUsableLog(t *testing.T) {
	const interval = 12 * time.Hour

	t.Run("журнала нет", func(t *testing.T) {
		l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
		if got := New(&fakeUpdater{}, l, interval, nil).firstDelay(); got != interval {
			t.Errorf("задержка %v, ожидался полный интервал %v", got, interval)
		}
	})

	t.Run("журнал нечитаем", func(t *testing.T) {
		// На месте файла журнала — каталог: чтение гарантированно упадёт.
		dir := t.TempDir()
		l := logs.New(dir)
		if _, _, err := l.Last(); err == nil {
			t.Skip("на этой системе чтение каталога не даёт ошибки")
		}
		if got := New(&fakeUpdater{}, l, interval, nil).firstDelay(); got != interval {
			t.Errorf("задержка %v, ожидался полный интервал %v", got, interval)
		}
	})
}

// Stop обязан прерывать И таймер первого срабатывания, и последующий тикер.
func TestStopDuringFirstWait(t *testing.T) {
	up := &fakeUpdater{out: []byte("41 nodes")}
	// Журнал пуст → Run уходит ждать целый час.
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	s := New(up, l, time.Hour, nil)

	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()

	s.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run не завершился после Stop во время ожидания первого таймера")
	}
	if got := up.count(); got != 0 {
		t.Errorf("выполнено %d обновлений, ожидалось 0", got)
	}
}

// То же для отмены контекста: ожидание первого таймера не должно держать
// демона при завершении.
func TestContextCancelDuringFirstWait(t *testing.T) {
	up := &fakeUpdater{out: []byte("41 nodes")}
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	s := New(up, l, time.Hour, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}
}

func TestIntervalFallsBackToDefault(t *testing.T) {
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	for _, bad := range []time.Duration{0, -time.Hour} {
		if got := New(&fakeUpdater{}, l, bad, nil).interval; got != DefaultInterval {
			t.Errorf("интервал %v → %v, ожидался %v", bad, got, DefaultInterval)
		}
	}
}
