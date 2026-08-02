package b4

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
}

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
		_ = json.NewEncoder(w).Encode(batchResponse{Success: true, Updated: n})

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

func TestSelectOnlyDisablesOthers(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	workki := sets[0].ID // выключен; HomeSet включён

	if err := c.SelectOnly(context.Background(), workki); err != nil {
		t.Fatalf("SelectOnly: %v", err)
	}

	got := f.enabled()
	if len(got) != 1 || got[0] != "workki" {
		t.Errorf("включено %v, ожидался только workki", got)
	}
}

// Порядок вызовов важен: сначала гасим лишние, потом включаем нужный.
// Обратный порядок оставил бы промежуток с двумя включёнными сетами —
// а это уже другое поведение обхода.
func TestSelectOnlyDisablesBeforeEnabling(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	_ = c.SelectOnly(context.Background(), sets[0].ID)

	f.mu.Lock()
	calls := append([]string(nil), f.calls...)
	f.mu.Unlock()

	batches := 0
	for _, c := range calls {
		if c == "POST /api/sets/batch-set-enabled" {
			batches++
		}
	}
	if batches != 2 {
		t.Fatalf("вызовов batch-set-enabled %d, ожидалось 2 (погасить, включить): %v", batches, calls)
	}
}

func TestSelectOnlyAlreadySelectedIsCheap(t *testing.T) {
	// Сет уже единственный включённый — включать его повторно незачем.
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	homeSet := sets[1].ID // уже включён, других включённых нет

	if err := c.SelectOnly(context.Background(), homeSet); err != nil {
		t.Fatalf("SelectOnly: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call == "POST /api/sets/batch-set-enabled" {
			t.Errorf("лишний вызов при уже выбранном сете: %v", f.calls)
			break
		}
	}
}

func TestSelectOnlyUnknownID(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	err := c.SelectOnly(context.Background(), "нет-такого-uuid")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидалась ErrNotFound, получено %v", err)
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
		{"select", func() error { return c.SelectOnly(context.Background(), "x") }},
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

func TestContextCancellation(t *testing.T) {
	_, c, done := newFakeB4(t)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Sets(ctx); err == nil {
		t.Error("отменённый контекст не прервал вызов")
	}
}

// Регрессия на ADR-0007: PUT /api/sets/{id} — полная замена, а не слияние
// (src/http/handler/sets.go:351-368). Отправив туда {"enabled":true},
// мы стёрли бы всю стратегию сета.
func TestNeverUsesPutSets(t *testing.T) {
	f, c, done := newFakeB4(t)
	defer done()

	sets, _ := c.Sets(context.Background())
	_ = c.SelectOnly(context.Background(), sets[0].ID)

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if len(call) > 3 && call[:3] == "PUT" {
			t.Errorf("использован PUT — он стирает стратегию: %q", call)
		}
	}
}

var _ Client = (*HTTP)(nil)
