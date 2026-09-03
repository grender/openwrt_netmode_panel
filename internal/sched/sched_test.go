package sched

import (
	"context"
	"errors"
	"fmt"
	"netmoded/internal/subs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"netmoded/internal/happ"
	"netmoded/internal/logs"
)

type fakeUpdater struct {
	mu     sync.Mutex
	sum    happ.Summary
	err    error
	calls  int
	block  chan struct{}
	onCall func()
}

func (f *fakeUpdater) Update(ctx context.Context) (happ.Summary, error) {
	f.mu.Lock()
	f.calls++
	blk, onCall := f.block, f.onCall
	sum, err := f.sum, f.err
	f.mu.Unlock()

	if onCall != nil {
		onCall()
	}
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return happ.Summary{}, ctx.Err()
		}
	}
	return sum, err
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

// has — есть ли в журнале строка с подстрокой.
func (r *recorder) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// waitFor крутится до выполнения условия или до истечения срока.
// Нужен, потому что расписание работает в своей горутине.
func waitFor(cond func() bool, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 42}}
	s, l := newSched(t, up)

	sum, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Nodes != 42 {
		t.Errorf("узлов %d, ожидалось 42", sum.Nodes)
	}

	last, ok, _ := l.Last()
	if !ok {
		t.Fatal("запись в журнал не сделана")
	}
	if last.Status != logs.StatusOK || last.Nodes != 42 || last.Err != "" {
		t.Errorf("запись: %+v", last)
	}
}

// Вторая линия предохранителя нуля узлов. Первичный стоит в subs.Update,
// который владеет файлами; здесь ловится обновление, СООБЩИВШЕЕ ОБ УСПЕХЕ
// с пустым списком, — иначе панель показала бы «обновлено, 0 узлов», хотя
// список остался прежним. Дублирование намеренное (ADR-0031).
func TestEmptyResultIsFailureNotSuccess(t *testing.T) {
	up := &fakeUpdater{sum: happ.Summary{Nodes: 0}}
	s, l := newSched(t, up)

	sum, err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("пустой результат выдан за успех")
	}
	if sum.Nodes != 0 {
		t.Errorf("узлов %d, ожидалось 0", sum.Nodes)
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
	up := &fakeUpdater{err: errors.New("subs: подписка не скачалась: i/o timeout")}
	s, l := newSched(t, up)

	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("ошибка обновления проглочена")
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}, block: make(chan struct{})}
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}
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

func TestLastRunRecorded(t *testing.T) {
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 42}}
	s := New(up, logs.New(brokenLogPath(t)), time.Hour, rec.logf)

	sum, err := s.RunOnce(context.Background())
	if err != nil {
		t.Errorf("подписка обновилась, но RunOnce вернул ошибку: %v", err)
	}
	if sum.Nodes != 42 {
		t.Errorf("узлов %d, ожидалось 42", sum.Nodes)
	}
	if rec.count() == 0 {
		t.Error("ошибка записи журнала потеряна: наружу не отдана и в лог не попала")
	}
}

// На пути неудачи поведение не меняется: ошибка конвертера возвращается
// независимо от того, записался журнал или нет.
func TestLogWriteFailureKeepsConverterError(t *testing.T) {
	rec := &recorder{}
	up := &fakeUpdater{err: errors.New("subs: подписка не скачалась: i/o timeout")}
	s := New(up, logs.New(brokenLogPath(t)), time.Hour, rec.logf)

	err := func() error { _, e := s.RunOnce(context.Background()); return e }()
	if err == nil || err.Error() != "subs: подписка не скачалась: i/o timeout" {
		t.Errorf("ошибка обновления подменена или проглочена: %v", err)
	}
	if rec.count() == 0 {
		t.Error("сбой журнала не залогирован")
	}
}

// Пустой logf не должен ронять демона: New обязан подставить заглушку.
func TestNilLogfIsSafe(t *testing.T) {
	up := &fakeUpdater{sum: happ.Summary{Nodes: 42}}
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}
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
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}
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

// Паника внутри одного срабатывания не должна уносить ни процесс, ни
// расписание.
//
// Цикл живёт месяцами: без перехвата первая же паника убила бы демона
// через полсуток после старта, без всякой связи с действиями владельца.
// Выход из цикла после перехвата тише, но не лучше: расписание молча
// перестало бы существовать, и это заметили бы неделями позже — по
// протухшей подписке.
func TestPanicInTickDoesNotEndSchedule(t *testing.T) {
	rec := &recorder{}
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}
	var fired atomic.Bool
	// Паникуем ровно на первом вызове: второй обязан состояться, иначе
	// тест не отличает «цикл выжил» от «цикл тихо кончился».
	up.onCall = func() {
		if !fired.Swap(true) {
			panic("подложенный сбой конвертера")
		}
	}

	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	s := New(up, l, 20*time.Millisecond, rec.logf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	defer s.Stop()

	if !waitFor(func() bool { return up.count() >= 2 }, 3*time.Second) {
		t.Fatalf("после паники расписание сделало %d вызовов, ожидалось не меньше двух", up.count())
	}
	if !rec.has("паника") {
		t.Error("паника не попала в журнал демона: чинить будет нечего")
	}
	// Стек нужен целиком и в той же записи: без него известно только,
	// что где-то рвануло.
	if !rec.has("sched.go") {
		t.Error("в журнале нет стека — по такой записи не найти место паники")
	}
}

// Паника при расчёте первой задержки приходится на самый старт: без
// перехвата procd поднимал бы демона по кругу, и владелец остался бы вообще
// без API. Не посчиталось — берём обычный интервал.
func TestPanicInFirstDelayFallsBackToInterval(t *testing.T) {
	rec := &recorder{}
	up := &fakeUpdater{sum: happ.Summary{Nodes: 41}}

	// Журнал говорит «обновлялись час назад»: без паники firstDelay вернул
	// бы catchUpDelay, то есть минуту, и обновления в этом тесте не было бы
	// вовсе. Значит, быстрый вызов конвертера доказывает именно откат на
	// интервал, а не случайное совпадение.
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	if err := l.Append(logs.Entry{TS: time.Now().Add(-time.Hour), Nodes: 41, Status: logs.StatusOK}); err != nil {
		t.Fatalf("подготовка журнала: %v", err)
	}

	s := New(up, l, 20*time.Millisecond, rec.logf)
	var fired atomic.Bool
	s.now = func() time.Time {
		if !fired.Swap(true) {
			panic("часы не отдали время")
		}
		return time.Now()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	defer s.Stop()

	if !waitFor(func() bool { return up.count() >= 1 }, 3*time.Second) {
		t.Fatal("после паники в расчёте задержки расписание не запустилось")
	}
	if !rec.has("паника") {
		t.Error("паника не попала в журнал демона")
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

// TestNotConfiguredIsNotJournaled — контракт subs.ErrNotConfigured: это
// состояние, а не поломка, и в журнал обновлений оно не пишется. Иначе
// свежая установка копила бы fail-строки при каждом вызове.
func TestNotConfiguredIsNotJournaled(t *testing.T) {
	up := &fakeUpdater{err: subs.ErrNotConfigured}
	s, l := newSched(t, up)

	_, err := s.RunOnce(context.Background())
	if !errors.Is(err, subs.ErrNotConfigured) {
		t.Fatalf("ошибка должна дойти до вызывающего как есть: %v", err)
	}
	if _, ok, lerr := l.Last(); lerr != nil || ok {
		t.Fatalf("журнал не должен получить запись: ok=%v err=%v", ok, lerr)
	}
}

// TestUnknownKeysAreLogged — единственный след того, что провайдер сменил
// формат xhttp, — строка в журнале демона.
func TestUnknownKeysAreLogged(t *testing.T) {
	rec := &recorder{}
	up := &fakeUpdater{sum: happ.Summary{Nodes: 3, UnknownKeys: []string{"xPaddingBytesV2"}}}
	l := logs.New(filepath.Join(t.TempDir(), "updates.log"))
	s := New(up, l, time.Hour, rec.logf)

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.has("xPaddingBytesV2") {
		t.Fatal("неизвестный ключ не попал в журнал демона")
	}
}
