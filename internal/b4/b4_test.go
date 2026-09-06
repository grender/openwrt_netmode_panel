package b4

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeB4 повторяет поведение настоящего b4 в той части, что нас касается.
// Форма ответов взята из docs/recon/raw/51-b4-sets.json — снятого с живого
// роутера, а не придуманного.
type fakeB4 struct {
	mu      sync.Mutex
	sets    []Set
	calls   []string
	down    bool
	status  int
	garbage bool

	// batchStatus — код, которым отвечает batch-set-enabled начиная
	// с batchFailFrom-го вызова (нумерация с 1). Счётчик пережил ADR-0033,
	// хотя вызов теперь один: он же отличает «отказал сразу» от «отказал
	// после успешного», а это разные пути в do.
	batchStatus   int
	batchFailFrom int
	batchCalls    int
	// batchSuccess подменяет поле success в ответе. Пусто — поля нет вовсе:
	// сборка b4, которая его не шлёт, обязана считаться успешной.
	batchSuccess *bool
}

func boolPtr(b bool) *bool { return &b }

func newFakeB4(t *testing.T) (*fakeB4, *HTTP, func()) {
	t.Helper()
	f := &fakeB4{sets: fixtureSets(t)}
	srv := httptest.NewServer(f)
	return f, New(srv.URL), srv.Close
}

// fixtureSets читает настоящий ответ роутера: два сета, workki выключен,
// HomeSet включён.
func fixtureSets(t *testing.T) []Set {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "recon", "raw", "51-b4-sets.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	var out []Set
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("разбор фикстуры: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("в фикстуре %d сетов, ожидалось 2", len(out))
	}
	return out
}

func (f *fakeB4) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)

	if f.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if f.garbage {
		_, _ = w.Write([]byte("не json"))
		return
	}

	switch r.URL.Path {
	case "/api/version":
		_ = json.NewEncoder(w).Encode(Version{Version: "1.74.1", Commit: "99808ed"})

	case "/api/sets":
		_ = json.NewEncoder(w).Encode(f.sets)

	case "/api/sets/batch-set-enabled":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.batchCalls++
		if f.batchStatus != 0 && f.batchCalls >= f.batchFailFrom {
			w.WriteHeader(f.batchStatus)
			return
		}
		var req batchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := 0
		for _, id := range req.IDs {
			for i := range f.sets {
				if f.sets[i].ID == id && f.sets[i].Enabled != req.Enabled {
					f.sets[i].Enabled = req.Enabled
					n++
				}
			}
		}
		// Тело собирается картой, а не нашей структурой: так тест видит
		// ту форму, что описана в docs/recon/b4-api.md, и умеет поле
		// success не отправлять вовсе.
		out := map[string]any{"updated": n}
		if f.batchSuccess != nil {
			out["success"] = *f.batchSuccess
		}
		_ = json.NewEncoder(w).Encode(out)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeB4) enabled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sets {
		if s.Enabled {
			out = append(out, s.Name)
		}
	}
	return out
}

// ─────────── чтение ───────────

func TestSetsFromRealFixture(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	sets, err := c.Sets(context.Background())
	if err != nil {
		t.Fatalf("Sets: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("сетов %d", len(sets))
	}
	// Имена с настоящего роутера, а не general/discord из макета.
	if sets[0].Name != "workki" || sets[1].Name != "HomeSet" {
		t.Errorf("имена: %q, %q", sets[0].Name, sets[1].Name)
	}
	if sets[0].Enabled || !sets[1].Enabled {
		t.Errorf("включённость: %v, %v", sets[0].Enabled, sets[1].Enabled)
	}
	// id — uuid, сгенерированный сервером; имя ключом не является.
	if len(sets[0].ID) != 36 {
		t.Errorf("id не похож на uuid: %q", sets[0].ID)
	}
}

func TestVersionIsHealthProbe(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Version != "1.74.1" {
		t.Errorf("версия %q", v.Version)
	}
}

func TestSelectedAndCount(t *testing.T) {
	sets := []Set{{Name: "a"}, {Name: "b", Enabled: true}}
	if got := Selected(sets); got != "b" {
		t.Errorf("Selected = %q", got)
	}
	if got := EnabledCount(sets); got != 1 {
		t.Errorf("EnabledCount = %d", got)
	}

	// Ноль включённых — пусто, а не первый попавшийся.
	if got := Selected([]Set{{Name: "a"}}); got != "" {
		t.Errorf("при нуле включённых Selected = %q", got)
	}
	// Больше одного — тоже пусто: состояние выставлено мимо панели,
	// и выдавать первый за «текущий» значило бы врать.
	two := []Set{{Name: "a", Enabled: true}, {Name: "b", Enabled: true}}
	if got := Selected(two); got != "" {
		t.Errorf("при двух включённых Selected = %q, ожидалась пустота", got)
	}
	if got := EnabledCount(two); got != 2 {
		t.Errorf("EnabledCount = %d", got)
	}
}

// ─────────── переключение ───────────

// Главный инвариант ADR-0033, и он ровно обратен прежнему: включение сета
// НЕ трогает соседние. До этого решения тот же тест утверждал
// противоположное («включено только workki»), поэтому проверять надо не
// «целевой включился», а «HomeSet остался включённым» — иначе эксклюзивность
// могла бы вернуться через любую правку и тест бы это пропустил.
func TestSetEnabledLeavesOthersAlone(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	workki := sets[0].ID // выключен; HomeSet включён

	if err := c.SetEnabled(context.Background(), workki, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	got := f.enabled()
	if len(got) != 2 {
		t.Fatalf("включено %v, ожидались оба сета: включение одного погасило соседний", got)
	}
}

// Выключение — то, чего у панели не было вовсе: SelectOnly умел только
// включать, и состояние «ни одного сета» из панели было недостижимо, хотя
// контракт называет его валидным (enabled_count: 0).
func TestSetEnabledCanTurnOff(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	homeSet := sets[1].ID // включён

	if err := c.SetEnabled(context.Background(), homeSet, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if got := f.enabled(); len(got) != 0 {
		t.Errorf("включено %v, ожидалась пустота", got)
	}
}

// Один вызов, а не два. Это не про экономию: у двухшагового переключения
// была середина (прочие погашены, целевой не включён), ради которой
// существовала ErrPartial. Пока вызов один, такого состояния не существует.
func TestSetEnabledIsOneCall(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	_ = c.SetEnabled(context.Background(), sets[0].ID, true)

	f.mu.Lock()
	calls := append([]string(nil), f.calls...)
	f.mu.Unlock()

	batches := 0
	for _, call := range calls {
		if call == "POST /api/sets/batch-set-enabled" {
			batches++
		}
	}
	if batches != 1 {
		t.Fatalf("вызовов batch-set-enabled %d, ожидался 1: %v", batches, calls)
	}
}

func TestSetEnabledAlreadyAtValueIsCheap(t *testing.T) {
	// Значение уже стоит — ходить в сеть незачем.
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	homeSet := sets[1].ID // уже включён

	if err := c.SetEnabled(context.Background(), homeSet, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call == "POST /api/sets/batch-set-enabled" {
			t.Errorf("лишний вызов при уже стоящем значении: %v", f.calls)
			break
		}
	}
}

func TestSetEnabledUnknownID(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	err := c.SetEnabled(context.Background(), "нет-такого-uuid", true)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидалась ErrNotFound, получено %v", err)
	}
}

// Провал вызова оставляет состояние нетронутым, и называть его особым
// словом больше незачем: промежуточного состояния у одного вызова нет.
// Раньше здесь стояли две проверки — на провал первого шага и на провал
// второго, — и разница между ними и была ErrPartial.
func TestSetEnabledFailureLeavesStateUntouched(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	workki := sets[0].ID // выключен

	f.mu.Lock()
	f.batchStatus, f.batchFailFrom = http.StatusInternalServerError, 1
	f.mu.Unlock()

	err := c.SetEnabled(context.Background(), workki, true)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("500 от b4 → %v, ожидалась ErrUnavailable", err)
	}
	if got := f.enabled(); len(got) != 1 || got[0] != "HomeSet" {
		t.Errorf("состояние изменилось: %v", got)
	}
}

// 200 с телом {"success": false} — не успех. До этой проверки b4 мог
// отказаться выполнять запрос, а панель показывала бы выбор состоявшимся.
func TestExplicitSuccessFalseIsRejected(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())

	f.mu.Lock()
	f.batchSuccess = boolPtr(false)
	f.mu.Unlock()

	err := c.SetEnabled(context.Background(), sets[0].ID, true)
	if !errors.Is(err, ErrRejected) {
		t.Errorf("success:false → %v, ожидалась ErrRejected", err)
	}
}

// Обратная сторона той же проверки: тела с полем success в разведке нет —
// оно известно только по исходникам b4. Сборка, которая поля не шлёт, обязана
// считаться успешной, иначе наивная проверка сломает КАЖДОЕ переключение.
func TestMissingSuccessFieldIsSuccess(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	if err := c.SetEnabled(context.Background(), sets[0].ID, true); err != nil {
		t.Fatalf("ответ без поля success принят за отказ: %v", err)
	}
	// Оба: workki включили, HomeSet не трогали (ADR-0033).
	if got := f.enabled(); len(got) != 2 {
		t.Errorf("включено %v, ожидались оба", got)
	}
}

func TestExplicitSuccessTrueIsSuccess(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())

	f.mu.Lock()
	f.batchSuccess = boolPtr(true)
	f.mu.Unlock()

	if err := c.SetEnabled(context.Background(), sets[0].ID, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
}

// updated:0 — НЕ ошибка (sets.go:679-683, ADR-0008): значит запрошенное
// значение уже стояло. Сверять его с числом отправленных id нельзя.
func TestUpdatedZeroIsNotAnError(t *testing.T) {
	sets := fixtureSets(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sets" {
			_ = json.NewEncoder(w).Encode(sets)
			return
		}
		// b4 пропускает запись, если значение уже стоит, и отвечает успехом
		// с нулём обновлённых.
		_, _ = w.Write([]byte(`{"success":true,"updated":0}`))
	}))
	defer srv.Close()

	if err := New(srv.URL).SetEnabled(context.Background(), sets[0].ID, true); err != nil {
		t.Errorf("updated:0 принят за ошибку: %v", err)
	}
}

// ─────────── недоступность ───────────

// b4 перезапускается сам — в снимке разведки он как раз лежал
// (raw/25-netstat.txt: порта 7000 нет, три curl вернули пустоту).
// Это штатная ситуация, а не сбой демона.
func TestUnavailableIsDistinguishable(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()
	f.down = true

	for _, tt := range []struct {
		name string
		call func() error
	}{
		{"version", func() error { _, err := c.Version(context.Background()); return err }},
		{"sets", func() error { _, err := c.Sets(context.Background()); return err }},
		{"select", func() error { return c.SetEnabled(context.Background(), "x", true) }},
	} {
		if err := tt.call(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: ожидалась ErrUnavailable, получено %v", tt.name, err)
		}
	}
}

func TestConnectionRefusedIsUnavailable(t *testing.T) {
	// Порт, на котором никого нет.
	c := New("http://127.0.0.1:1")
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("отказ соединения → %v, ожидалась ErrUnavailable", err)
	}
}

func TestAuthEnabledLaterDegradesGracefully(t *testing.T) {
	// Если авторизацию когда-нибудь включат, вызовы начнут отдавать 401.
	// Клиент обязан честно сказать «недоступен», а не подбирать пароль.
	f, c, done := newFakeB4(t)
	defer done()
	f.status = http.StatusUnauthorized

	if _, err := c.Sets(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("401 → %v, ожидалась ErrUnavailable", err)
	}
}

func TestGarbageResponseIsError(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()
	f.garbage = true

	if _, err := c.Sets(context.Background()); err == nil {
		t.Error("мусор вместо JSON принят молча")
	}
}

// Отмена контекста — НАШ отказ, а не недоступность b4: панель ушла со
// страницы и закрыла запрос. Превратив это в ErrUnavailable, статус погасил
// бы чипы сетов на ровном месте.
func TestContextCancellation(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Sets(ctx)
	if err == nil {
		t.Fatal("отменённый контекст не прервал вызов")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("отмена выдана за недоступность b4: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("причина отмены потеряна: %v", err)
	}
}

// Истёкший дедлайн — наоборот, недоступность: b4 не успел ответить.
func TestDeadlineIsUnavailable(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.Sets(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("истёкший дедлайн → %v, ожидалась ErrUnavailable", err)
	}
}

// Регрессия на ADR-0007: PUT /api/sets/{id} — полная замена, а не слияние
// (src/http/handler/sets.go:351-368). Отправив туда {"enabled":true},
// мы стёрли бы всю стратегию сета.
func TestNeverUsesPutSets(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	_ = c.SetEnabled(context.Background(), sets[0].ID, true)

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if len(call) > 3 && call[:3] == "PUT" {
			t.Errorf("использован PUT — он стирает стратегию: %q", call)
		}
	}
}

var _ Client = (*HTTP)(nil)

// PanelURL берёт порт из подтверждённого DefaultBaseURL, а не из своего
// литерала: второй литерал в internal/httpapi гейт check-evidence не увидел бы
// вовсе (он этот пакет не сканирует), и адрес чужого сервиса появился бы в
// коде без ссылки на разведку.
func TestPanelURLTakesPortFromConfirmedBaseURL(t *testing.T) {
	u, err := url.Parse(DefaultBaseURL)
	if err != nil {
		t.Fatalf("DefaultBaseURL не разбирается: %v", err)
	}
	got, ok := PanelURL("192.168.9.1")
	if !ok {
		t.Fatal("PanelURL отказал на обычном хосте")
	}
	want := "http://192.168.9.1:" + u.Port() + "/"
	if got != want {
		t.Errorf("PanelURL = %q, ожидалось %q", got, want)
	}
	// 127.0.0.1 из базового адреса — для демона, а не для браузера владельца.
	if strings.Contains(got, u.Hostname()) {
		t.Errorf("в ссылке для браузера остался хост демона: %q", got)
	}
}

// IPv6 обязан приезжать в скобках. Ручная склейка host + ":" + port дала бы
// `http://fd00::1:7000/` — адрес, который браузер разберёт как другой хост
// без порта, то есть тихо неправильную ссылку вместо ошибки.
func TestPanelURLBracketsIPv6(t *testing.T) {
	got, ok := PanelURL("fd00::1")
	if !ok {
		t.Fatal("PanelURL отказал на IPv6")
	}
	if got != "http://[fd00::1]:7000/" {
		t.Errorf("PanelURL(fd00::1) = %q, ожидалось http://[fd00::1]:7000/", got)
	}
}

func TestPanelURLRefusesEmptyHost(t *testing.T) {
	if got, ok := PanelURL(""); ok {
		t.Errorf("пустой хост принят: %q", got)
	}
}
