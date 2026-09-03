package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestLogsEndpointEmptyIsArrayNotNull(t *testing.T) {
	// Панель перебирает список; null заставил бы её отдельно это проверять.
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/logs", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.Bytes(), `"lines": []`) {
		t.Errorf("пустой журнал отдан не пустым массивом: %s", rec.Body.String())
	}
}

func TestLogsRejectsBadN(t *testing.T) {
	s, _ := newServer(t)
	for _, v := range []string{"0", "-5", "много"} {
		if rec := do(t, s, "GET", "/api/logs?n="+v, true); rec.Code != http.StatusBadRequest {
			t.Errorf("n=%q → %d, ожидался 400", v, rec.Code)
		}
	}
}

func TestSubscriptionUpdateStartsJob(t *testing.T) {
	s, _ := newServer(t)

	rec := post(t, s, "/api/subscription/update", `{}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Job struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Arg   string `json:"arg"`
			State string `json:"state"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Job.ID == "" || got.Job.Kind != "subscription" {
		t.Errorf("джоб: %+v", got.Job)
	}
	// Уточнять в обновлении подписки нечего: панель подписывает такой джоб
	// по одному kind.
	if got.Job.Arg != "" {
		t.Errorf("arg=%q, ожидалась пустая строка", got.Job.Arg)
	}
	s.jobs.Wait(2 * time.Second)
}

// Второй джоб отбивается 409-м, а не встаёт в очередь (SPEC §6):
// пользователь нажал кнопку сейчас, и начать операцию через минуту —
// не то, о чём он просил.
func TestSecondJobIs409(t *testing.T) {
	s, _ := newServer(t)

	// Занимаем менеджер долгой операцией.
	block := make(chan struct{})
	if _, err := s.jobs.Start("mode", "b4", "занято", 8, func(ctx context.Context) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	}); err != nil {
		t.Fatalf("первый джоб: %v", err)
	}

	rec := post(t, s, "/api/subscription/update", `{}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "job_busy" {
		t.Errorf("код ошибки %q", got)
	}

	close(block)
	s.jobs.Wait(2 * time.Second)
}

func TestStatusCarriesSubscriptionAndJob(t *testing.T) {
	s, _ := newServer(t)

	// Часы подменяются ДО первого запроса: иначе метка кэша окажется по
	// настоящему времени, а сравнение — по поддельному, и кэш будет
	// считаться вечно свежим.
	base := time.Unix(1700000000, 0)
	cur := base
	s.status.now = func() time.Time { return cur }

	// До первого обновления — «никогда».
	rec := do(t, s, "GET", "/api/status", true)
	var before struct {
		Subscription struct {
			Status     string  `json:"status"`
			LastUpdate *string `json:"last_update"`
		} `json:"subscription"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &before)
	if before.Subscription.Status != "never" || before.Subscription.LastUpdate != nil {
		t.Errorf("до обновления: %+v", before.Subscription)
	}

	if _, err := s.sched.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Результат появится не мгновенно: статус кэшируется на 500 мс.
	// Это нормально и заложено в SPEC §7 — поэтому двигаем часы, а не спим.
	cur = base.Add(StatusCacheTTL + time.Millisecond)

	rec = do(t, s, "GET", "/api/status", true)
	var after struct {
		Subscription struct {
			Status     string  `json:"status"`
			Nodes      int     `json:"nodes"`
			LastUpdate *string `json:"last_update"`
		} `json:"subscription"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Subscription.Status != "ok" || after.Subscription.Nodes != 42 {
		t.Errorf("после обновления: %+v", after.Subscription)
	}
	if after.Subscription.LastUpdate == nil {
		t.Error("время обновления не проставлено")
	}
}

// Прогресс операции обязан быть виден сразу, а не раз в 500 мс: застывший
// прогресс выглядит как зависшая операция.
func TestJobVisibleEvenWhileStatusCached(t *testing.T) {
	s, _ := newServer(t)
	base := time.Unix(1700000000, 0)
	s.status.now = func() time.Time { return base }

	_ = do(t, s, "GET", "/api/status", true) // заполнили кэш: джоба нет

	block := make(chan struct{})
	if _, err := s.jobs.Start("mode", "b4", "Переключение", 8, func(ctx context.Context) error {
		<-block
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Время не двигаем — кэш заведомо свежий.
	rec := do(t, s, "GET", "/api/status", true)
	var got struct {
		Job *struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
		} `json:"job"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Job == nil {
		t.Fatal("джоб не виден из кэшированного статуса — прогресс замрёт")
	}
	if got.Job.Kind != "mode" || got.Job.State != "running" {
		t.Errorf("джоб: %+v", got.Job)
	}

	close(block)
	s.jobs.Wait(2 * time.Second)
}
