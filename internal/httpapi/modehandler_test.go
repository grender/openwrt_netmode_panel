package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netmoded/internal/executor"
	"netmoded/internal/led"
)

// serverWithLED поднимает сервер с поддельным sysfs, чтобы проверять
// индикацию по файлам, а не по вызовам.
func serverWithLED(t *testing.T) (*Server, *executor.Fake, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{led.Blue, led.White} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []struct{ n, v string }{
			{"trigger", "none"}, {"brightness", "0"},
			{"max_brightness", "255"}, {"delay_on", "0"}, {"delay_off", "0"},
		} {
			if err := os.WriteFile(filepath.Join(dir, f.n), []byte(f.v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.UCIValues["netmode.main.mode"] = "off"
	f.UCIValues["system.@system[0].hostname"] = "grenderRouter"

	s, err := NewServer(Config{
		Listen:  "192.168.9.1",
		Port:    8088,
		Token:   testToken,
		LogPath: filepath.Join(t.TempDir(), "updates.log"),
		LEDRoot: root,
	}, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.SetB4Client(newFakeB4Client())
	s.SetNikkiClient(newFakeNikkiClient())
	t.Cleanup(s.Close)
	return s, f, root
}

func ledAttr(t *testing.T, root, name, attr string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name, attr))
	if err != nil {
		t.Fatalf("%s/%s: %v", name, attr, err)
	}
	return strings.TrimSpace(string(b))
}

// Порядок обязателен: сначала намерение в UCI, потом скрипт. Иначе после
// успешного применения и упавшей записи система работала бы не в том
// режиме, который записан, и после перезагрузки молча вернулась бы назад.
func TestModeWritesIntentBeforeApplying(t *testing.T) {
	s, f, _ := serverWithLED(t)

	rec := post(t, s, "/api/mode", `{"mode":"nikki"}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if !s.jobs.Wait(3 * time.Second) {
		t.Fatal("джоб не завершился")
	}

	var iSet, iCommit, iApply = -1, -1, -1
	for i, c := range f.Calls {
		switch {
		case c == "set netmode.main.mode=nikki":
			iSet = i
		case c == "commit netmode":
			iCommit = i
		case c == "apply-mode nikki":
			iApply = i
		}
	}
	if iSet < 0 || iCommit < 0 || iApply < 0 {
		t.Fatalf("не все шаги выполнены: %v", f.Calls)
	}
	if !(iSet < iCommit && iCommit < iApply) {
		t.Errorf("порядок нарушен: set=%d commit=%d apply=%d\n%v", iSet, iCommit, iApply, f.Calls)
	}
}

// Панель подписывает операцию сама, по kind и arg: label с демона русский и
// живёт ради syslog. Без arg английский интерфейс либо показал бы русскую
// строку, либо потерял бы, на какой режим идёт переключение.
func TestModeJobCarriesMachineReadableArg(t *testing.T) {
	for _, mode := range []string{"nikki", "b4", "off"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := serverWithLED(t)

			rec := post(t, s, "/api/mode", `{"mode":"`+mode+`"}`, "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Job struct {
					Kind  string `json:"kind"`
					Arg   string `json:"arg"`
					Label string `json:"label"`
				} `json:"job"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("разбор ответа: %v", err)
			}
			if got.Job.Kind != "mode" {
				t.Errorf("kind=%q, ожидалось mode", got.Job.Kind)
			}
			if got.Job.Arg != mode {
				t.Errorf("arg=%q, ожидалось %q", got.Job.Arg, mode)
			}
			if got.Job.Label == "" {
				t.Error("label пуст: его читают в syslog и в диагностике по ssh")
			}
			s.jobs.Wait(3 * time.Second)
		})
	}
}

// Индикация показывает ЦЕЛЬ во время переключения и ФАКТ после.
func TestModeLEDShowsTargetThenResult(t *testing.T) {
	s, _, root := serverWithLED(t)

	if rec := post(t, s, "/api/mode", `{"mode":"b4"}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("код %d", rec.Code)
	}
	s.jobs.Wait(3 * time.Second)

	// Режим b4 — белый ровно, синий погашен (SPEC §8).
	if got := ledAttr(t, root, led.White, "brightness"); got != "255" {
		t.Errorf("белый brightness=%q, ожидался 255", got)
	}
	if got := ledAttr(t, root, led.White, "trigger"); got != "none" {
		t.Errorf("после завершения белый должен гореть ровно, trigger=%q", got)
	}
	if got := ledAttr(t, root, led.Blue, "brightness"); got != "0" {
		t.Errorf("синий brightness=%q, ожидался 0", got)
	}
}

// Сбой netmode-apply: джоб падает, но демон продолжает работать, а
// светодиод мигает ошибкой и возвращается к ЗАПИСАННОМУ режиму —
// панель и индикация должны говорить одно.
func TestModeApplyFailureIsReported(t *testing.T) {
	s, f, root := serverWithLED(t)
	f.Errors["apply-mode nikki"] = errors.New("nikki не запустился")

	if rec := post(t, s, "/api/mode", `{"mode":"nikki"}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("код %d", rec.Code)
	}
	s.jobs.Wait(3 * time.Second)

	cur := s.jobs.Current()
	if cur == nil || cur.State != "failed" {
		t.Fatalf("джоб: %+v", cur)
	}
	if cur.Error == nil || !strings.Contains(*cur.Error, "не запустился") {
		t.Errorf("причина не сохранена: %v", cur.Error)
	}
	// Намерение всё равно записано: повтор возможен, и netmode-apply
	// при следующей загрузке доведёт дело сам.
	joined := strings.Join(f.Calls, "\n")
	if !strings.Contains(joined, "set netmode.main.mode=nikki") {
		t.Error("намерение не записано — повторить будет нечего")
	}
	// Оба мигают быстро — индикация ошибки.
	if got := ledAttr(t, root, led.Blue, "delay_on"); got != "100" {
		t.Errorf("синий delay_on=%q, ожидалось 100", got)
	}
}

// Индикация — best-effort: её отказ НИКОГДА не валит переключение.
func TestModeSucceedsWhenLEDIsBroken(t *testing.T) {
	s, f, _ := serverWithLED(t)
	// Подсовываем несуществующий каталог.
	s.led = led.New(filepath.Join(t.TempDir(), "нет-такого"), func(string, ...any) {})

	if rec := post(t, s, "/api/mode", `{"mode":"b4"}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("код %d", rec.Code)
	}
	s.jobs.Wait(3 * time.Second)

	cur := s.jobs.Current()
	if cur == nil || cur.State != "done" {
		t.Fatalf("сломанная индикация уронила переключение: %+v", cur)
	}
	if !strings.Contains(strings.Join(f.Calls, "\n"), "apply-mode b4") {
		t.Error("режим не применён")
	}
}

func TestModeRejectsUnknown(t *testing.T) {
	s, f, _ := serverWithLED(t)
	for _, body := range []string{`{"mode":"мусор"}`, `{"mode":""}`, `{}`, `{`} {
		rec := post(t, s, "/api/mode", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("тело %q → %d, ожидался 400", body, rec.Code)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("при отказе записей быть не должно: %v", f.Calls)
	}
}

// Второй джоб отбивается 409-м (SPEC §6).
func TestModeSecondRequestIs409(t *testing.T) {
	s, _, _ := serverWithLED(t)

	block := make(chan struct{})
	if _, err := s.jobs.Start("mode", "b4", "занято", 8, func(ctx context.Context) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/api/mode", `{"mode":"b4"}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409", rec.Code)
	}
	if got := errCode(t, rec); got != "job_busy" {
		t.Errorf("код ошибки %q", got)
	}
	close(block)
	s.jobs.Wait(2 * time.Second)
}

func TestModeAppearsInStatus(t *testing.T) {
	s, f, _ := serverWithLED(t)
	f.UCIValues["netmode.main.mode"] = "off"

	if rec := post(t, s, "/api/mode", `{"mode":"nikki"}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("код %d", rec.Code)
	}

	// Джоб виден в статусе сразу, не дожидаясь истечения кэша.
	rec := do(t, s, "GET", "/api/status", true)
	var got struct {
		Job *struct {
			Kind  string `json:"kind"`
			Label string `json:"label"`
		} `json:"job"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Job == nil || got.Job.Kind != "mode" {
		t.Errorf("джоб в статусе: %+v", got.Job)
	}
	s.jobs.Wait(3 * time.Second)
}
