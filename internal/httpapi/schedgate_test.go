package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Плановое обновление идёт джобом «subscription» — видно в статусе и не
// пересекается с операциями владельца.
func TestSchedGateRunsAsJob(t *testing.T) {
	s := subServer(t, testSubURL)
	ran := make(chan struct{})
	if err := s.schedGate(context.Background(), func(context.Context) error {
		close(ran)
		return nil
	}); err != nil {
		t.Fatalf("schedGate: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("обновление не запущено")
	}
	s.jobs.Wait(2 * time.Second)
	if j := s.jobs.Current(); j == nil || j.Kind != "subscription" {
		t.Errorf("джоб: %+v, ожидался subscription", j)
	}
}

// Без адреса джоб не заводится: он тут же провалился бы «адрес не задан», и
// раз в полсуток панель показывала бы провал того, чего не просили.
func TestSchedGateSkipsWithoutURL(t *testing.T) {
	s, _ := newServer(t)
	called := false
	if err := s.schedGate(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("schedGate: %v", err)
	}
	if called || s.jobs.Current() != nil {
		t.Error("без адреса подписки плановое обновление всё равно запущено")
	}
}

// Занятая панель откладывает тик, а не теряет его на двенадцать часов.
func TestSchedGateWaitsForBusyJob(t *testing.T) {
	defer func(d time.Duration, n int) { schedBusyRetry, schedBusyRetries = d, n }(schedBusyRetry, schedBusyRetries)
	schedBusyRetry, schedBusyRetries = 5*time.Millisecond, 200

	s := subServer(t, testSubURL)
	block := make(chan struct{})
	if _, err := s.jobs.Start("mode", "b4", "занято", 8, func(ctx context.Context) error {
		<-block
		return nil
	}); err != nil {
		t.Fatalf("первый джоб: %v", err)
	}
	time.AfterFunc(30*time.Millisecond, func() { close(block) })

	ran := make(chan struct{})
	if err := s.schedGate(context.Background(), func(context.Context) error {
		close(ran)
		return nil
	}); err != nil {
		t.Fatalf("schedGate: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("отложенное обновление так и не запущено")
	}
	s.jobs.Wait(2 * time.Second)
}

// Если панель занята всё окно повторов, тик пропускается с записью в лог,
// а не ждёт бесконечно.
func TestSchedGateGivesUp(t *testing.T) {
	defer func(d time.Duration, n int) { schedBusyRetry, schedBusyRetries = d, n }(schedBusyRetry, schedBusyRetries)
	schedBusyRetry, schedBusyRetries = time.Millisecond, 2

	s := subServer(t, testSubURL)
	block := make(chan struct{})
	defer close(block)
	if _, err := s.jobs.Start("mode", "b4", "занято", 8, func(ctx context.Context) error {
		<-block
		return nil
	}); err != nil {
		t.Fatalf("первый джоб: %v", err)
	}
	called := false
	err := s.schedGate(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if err != nil || called {
		t.Errorf("err=%v called=%v, ожидался тихий пропуск", err, called)
	}
}

// Остановка по отмене контекста — чистый выход, а не ошибка: раньше
// ListenAndServe возвращал http.ErrServerClosed, и каждый SIGTERM
// завершал демон кодом 1 посреди окна дочитывания.
func TestListenAndServeStopsCleanly(t *testing.T) {
	s, _ := newServer(t)
	s.cfg.Listen, s.cfg.Port = "127.0.0.1", 0

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.ListenAndServe(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("остановка вернула %v, ожидался чистый выход", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("сервер не остановился")
	}
}
