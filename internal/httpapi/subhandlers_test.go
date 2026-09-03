package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"netmoded/internal/sched"
)

// testSubURL — правдоподобный адрес подписки. Значение секретное только на
// роутере; здесь важно лишь то, что оно непусто.
const testSubURL = "https://example.invalid/sub/0123456789abcdef"

// subServer — сервер с ЗАДАННЫМ адресом подписки.
//
// Пересобирается настоящим NewServer, а не правкой s.cfg на готовом сервере:
// признак configured проставляется в конструкторе, и присваивание постфактум
// проверяло бы тест сам себя, а не проводку от конфигурации до ответа.
func subServer(t *testing.T, url string) *Server {
	t.Helper()
	base, f := newServer(t)
	cfg := base.cfg
	cfg.SubscriptionURL = url

	s, err := NewServer(cfg, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.SetB4Client(newFakeB4Client())
	s.SetNikkiClient(newFakeNikkiClient())
	// Как и в newServer: боевое обновление ходит в сеть и пишет в каталоги,
	// которых на машине разработчика нет.
	s.sched = sched.New(fakeSubUpdater{nodes: 42}, s.logs, 0, s.logf)
	return s
}

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
	// Адрес задан: без него обновление отбивается 409 и джоба не заводит.
	s := subServer(t, testSubURL)

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
	// Адрес задан: проверяется отказ ИЗ-ЗА ЗАНЯТОСТИ, и пустой адрес отбил бы
	// запрос раньше, оставив тест зелёным по чужой причине.
	s := subServer(t, testSubURL)

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

// Пустой адрес подписки — 409 и НИ ОДНОГО следа: ни джоба, ни строки в
// журнале обновлений.
//
// Отбить это внутри джоба было бы дешевле кодом, но дороже владельцу:
// каждое нажатие на свежей установке писало бы в журнал строку «fail», и
// нормальное состояние «ещё не настроено» копилось бы там историей неудач.
// Журнал, полный ожидаемых провалов, перестают читать.
func TestSubscriptionUpdateWithoutURLIs409(t *testing.T) {
	s, _ := newServer(t) // адрес не задан — как на свежей установке

	rec := post(t, s, "/api/subscription/update", `{}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "subscription_not_configured" {
		t.Errorf("код ошибки %q", got)
	}
	if j := s.jobs.Current(); j != nil {
		t.Errorf("заведён джоб %+v: отказ обязан случиться ДО запуска", j)
	}
	lines, err := s.logs.Tail(10)
	if err != nil {
		t.Fatalf("журнал: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("в журнал обновлений записано %d строк: %+v", len(lines), lines)
	}
}

// Секрет не уходит наружу ни отказом, ни статусом: наружу едет признак
// «задан», а не значение (ADR-0012).
func TestSubscriptionRefusalNeverLeaksURL(t *testing.T) {
	s := subServer(t, testSubURL)
	// Занимаем менеджер, чтобы получить отказ job_busy при заданном адресе.
	block := make(chan struct{})
	if _, err := s.jobs.Start("mode", "b4", "занято", 8, func(ctx context.Context) error {
		<-block
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := post(t, s, "/api/subscription/update", `{}`, "")
	if contains(rec.Body.Bytes(), testSubURL) {
		t.Errorf("адрес подписки утёк в тело отказа: %s", rec.Body.String())
	}
	st := do(t, s, "GET", "/api/status", true)
	if contains(st.Body.Bytes(), testSubURL) {
		t.Errorf("адрес подписки утёк в /api/status: %s", st.Body.String())
	}
	close(block)
	s.jobs.Wait(2 * time.Second)
}

// configured отвечает на вопрос, на который остальные три поля ответить не
// могут: у незаданного адреса и у заданного, но ни разу не скачанного,
// last_update одинаково null, status одинаково "never", nodes одинаково 0.
func TestStatusSubscriptionConfiguredFlag(t *testing.T) {
	read := func(t *testing.T, s *Server) struct {
		Configured bool    `json:"configured"`
		Status     string  `json:"status"`
		LastUpdate *string `json:"last_update"`
		Nodes      int     `json:"nodes"`
	} {
		t.Helper()
		var got struct {
			Subscription struct {
				Configured bool    `json:"configured"`
				Status     string  `json:"status"`
				LastUpdate *string `json:"last_update"`
				Nodes      int     `json:"nodes"`
			} `json:"subscription"`
		}
		rec := do(t, s, "GET", "/api/status", true)
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("разбор: %v", err)
		}
		return got.Subscription
	}

	t.Run("адрес не задан", func(t *testing.T) {
		s, _ := newServer(t)
		got := read(t, s)
		if got.Configured {
			t.Errorf("configured=true при пустом subscription_url: %+v", got)
		}
	})

	t.Run("адрес задан, обновлений ещё не было", func(t *testing.T) {
		s := subServer(t, testSubURL)
		got := read(t, s)
		// Ровно тот случай, ради которого поле заведено: всё остальное
		// выглядит так же, как у ненастроенной подписки.
		if !got.Configured {
			t.Errorf("configured=false при заданном subscription_url: %+v", got)
		}
		if got.Status != "never" || got.LastUpdate != nil || got.Nodes != 0 {
			t.Errorf("остальные поля обязаны быть неотличимы от ненастроенной: %+v", got)
		}
	})

	t.Run("адрес задан, обновление прошло", func(t *testing.T) {
		s := subServer(t, testSubURL)
		if _, err := s.sched.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		// Кэш статуса живёт 500 мс — двигаем часы, а не спим.
		base := time.Unix(1700000000, 0)
		cur := base
		s.status.now = func() time.Time { return cur }
		_ = read(t, s)
		cur = base.Add(StatusCacheTTL + time.Millisecond)

		got := read(t, s)
		if !got.Configured || got.Status != "ok" || got.Nodes != 42 {
			t.Errorf("после обновления: %+v", got)
		}
	})
}
