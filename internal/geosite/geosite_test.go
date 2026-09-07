package geosite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureETag — ETag, которым стенд помечает корневое дерево.
//
// Значение ровно в той форме, в какой его отдаёт GitHub (слабый валидатор
// W/"…"): клиент обязан возвращать строку как есть, а не разбирать её.
const fixtureETag = `W/"c0ffee1234567890"`

// stand — GitHub понарошку: четыре фикстуры дерева и счётчик запросов.
//
// Счётчик здесь не украшение, а главный измерительный прибор пакета. Весь
// смысл кэша — не ходить в GitHub: лимит 60 запросов в час на IP роутера
// тратится по четыре штуки за холодную загрузку. Проверить «не сходил» можно
// только счётчиком; проверка «данные те же» прошла бы и на клиенте, который
// перекачивает каталог при каждом обращении панели.
type stand struct {
	srv *httptest.Server

	mu       sync.Mutex
	hits     int               // все запросы, включая поддеревья
	condHits int               // запросы корня с If-None-Match
	lastHdr  http.Header       // заголовки последнего запроса
	trees    map[string][]byte // путь → тело фикстуры с подставленным адресом стенда

	// status ≠ 0 — отвечать этим кодом на всё. Так изображается лежащий
	// GitHub, не трогая фикстуры.
	status int
	// gate ≠ nil — держать ответ на корень до закрытия канала. Нужен, чтобы
	// доказать «старый список отдаётся СРАЗУ»: пока обновление заперто,
	// вернуться Get может только из кэша.
	gate chan struct{}
	// arrived ≠ nil — в него уходит сигнал о каждом пришедшем запросе.
	// Нужен там, где тест обязан дождаться, пока клиент ДОЙДЁТ до сети:
	// иначе ждать пришлось бы сном «на глазок».
	arrived chan struct{}
	// truncPath — на этом дереве отвечать с "truncated": true.
	truncPath string
	// oversize — отвечать телом больше maxBody.
	oversize bool
}

// rootPath — путь корневого дерева ветки meta.
//
// Дублируется в тесте намеренно: если кто-то поменяет путь в коде, тест
// обязан покраснеть, а не молча последовать за правкой.
const standRootPath = "/repos/MetaCubeX/meta-rules-dat/git/trees/meta"

func newStand(t *testing.T) *stand {
	t.Helper()
	s := &stand{}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.Start()
	t.Cleanup(s.srv.Close)

	// Фикстуры читаются здесь, а не в обработчике: обработчик работает в
	// чужой горутине, где t.Fatalf запрещён, а ошибку чтения файла надо
	// показать тесту, а не превратить в загадочный 500.
	s.trees = map[string][]byte{
		standRootPath:    s.load(t, "tree-root.json"),
		"/trees/geo":     s.load(t, "tree-geo.json"),
		"/trees/geosite": s.load(t, "tree-geosite.json"),
		"/trees/geoip":   s.load(t, "tree-geoip.json"),
	}
	return s
}

// load читает фикстуру и подставляет в неё адрес стенда.
//
// GitHub кладёт в каждую запись дерева ПОЛНЫЙ адрес поддерева, и клиент
// обязан ходить по нему, а не собирать путь сам. Значит фикстура не может
// быть статичной: адрес httptest известен только в момент запуска. Отсюда
// {{BASE}} — плейсхолдер, а не выдуманный хост.
func (s *stand) load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("фикстура %s: %v", name, err)
	}
	return bytes.ReplaceAll(b, []byte("{{BASE}}"), []byte(s.srv.URL))
}

func (s *stand) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits++
	s.lastHdr = r.Header.Clone()
	cond := r.Header.Get("If-None-Match")
	if r.URL.Path == standRootPath && cond != "" {
		s.condHits++
	}
	status, gate, trunc, oversize := s.status, s.gate, s.truncPath, s.oversize
	body := s.trees[r.URL.Path]
	arrived := s.arrived
	s.mu.Unlock()

	// Сигнал о приходе — ДО шлюза: тест ждёт именно того мгновения, когда
	// клиент уже в сети, но ответа ещё не получил.
	if arrived != nil {
		select {
		case arrived <- struct{}{}:
		default:
		}
	}

	if gate != nil && r.URL.Path == standRootPath {
		<-gate
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if oversize {
		// Тело заведомо больше потолка: клиент обязан отказаться, а не
		// разбирать обрезанное.
		chunk := bytes.Repeat([]byte("A"), 64<<10)
		for written := 0; written <= maxBody; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		return
	}

	// 304 отдаётся ТОЛЬКО при совпавшем If-None-Match. Это страж мутации:
	// уберите заголовок из клиента — сюда придёт полное дерево, счётчик
	// запросов вырастет на четыре вместо одного, и тест покраснеет.
	if r.URL.Path == standRootPath && cond == fixtureETag {
		w.Header().Set("ETag", fixtureETag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if body == nil {
		http.NotFound(w, r)
		return
	}
	if trunc == r.URL.Path {
		body = bytes.Replace(body, []byte(`"truncated": false`), []byte(`"truncated": true`), 1)
	}
	w.Header().Set("ETag", fixtureETag)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *stand) counts() (hits, cond int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.condHits
}

func (s *stand) set(fn func(*stand)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// journal — журнал демона, собранный в памяти.
type journal struct {
	mu    sync.Mutex
	lines []string
}

func (j *journal) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, fmt.Sprintf(format, args...))
}

func (j *journal) text() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.Join(j.lines, "\n")
}

// newClient — клиент, направленный на стенд.
//
// refreshed — хук ожидания фона: без него тест сравнивал бы состояние с
// горутиной наперегонки и лечился бы сном «на глазок». Сон в миллисекундах
// тут допустим только там, где он ждёт истечения TTL (нижняя граница), но
// не там, где он ждёт чужую работу.
func newClient(t *testing.T, s *stand, ttl time.Duration, j *journal) (*Client, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 8)
	c := &Client{
		BaseURL: s.srv.URL,
		HTTP:    s.srv.Client(),
		TTL:     ttl,
		Logf:    j.logf,
	}
	c.refreshed = func() {
		select {
		case done <- struct{}{}:
		default:
		}
	}
	return c, done
}

// waitArrived ждёт, пока запрос ДОЙДЁТ до стенда.
//
// Отдельно от waitRefresh: тот ждёт конца работы, а этот — её начала. Оба
// нужны затем, чтобы в тестах не было сна «на глазок»: он делает проверку
// зелёной или красной по нагрузке машины, а не по поведению кода.
func waitArrived(t *testing.T, arrived <-chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("клиент не сходил в GitHub за 5 с")
	}
}

func waitRefresh(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("фоновое обновление каталога не завершилось за 5 с")
	}
}

// TestGetLoadsCatalogFromTrees — холодная загрузка: четыре запроса, имена
// только из *.mrs, признак ip — пересечение с geoip.
func TestGetLoadsCatalogFromTrees(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)

	cat, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := []string{"category-ai-!cn", "netflix@ads", "telegram", "youtube"}
	if strings.Join(cat.Names, ",") != strings.Join(want, ",") {
		t.Errorf("имена %v, ожидались %v (отсортированы, только *.mrs)", cat.Names, want)
	}
	// Соседи по имени (.yaml, .list) и подкаталог не имена: за каждым из
	// них нет файла правил, который mihomo сможет скачать.
	for _, bad := range []string{"telegram.mrs", "telegram.yaml", "classical", "youtube.list"} {
		if cat.Has(bad) {
			t.Errorf("в каталоге оказалось %q", bad)
		}
	}
	if !cat.Has("category-ai-!cn") {
		t.Error("имя с '!' и '-' не найдено — разбор портит имена")
	}
	if !cat.Has("netflix@ads") {
		t.Error("имя с '@' не найдено")
	}
	if !cat.HasIP("telegram") {
		t.Error("telegram обязан быть с geoip: он есть в обоих деревьях")
	}
	if cat.HasIP("youtube") {
		t.Error("youtube помечен как geoip, хотя его нет в дереве geoip")
	}
	// cn и private лежат только в geoip: без домена в geosite предлагать
	// их нечего — ip лишь ДОПОЛНЯЕТ набор доменов.
	if got := cat.IPNames(); strings.Join(got, ",") != "telegram" {
		t.Errorf("IPNames %v, ожидался только telegram", got)
	}
	if cat.Commit != "9b1ce4a0d2f74c1e8ab35f60c7d9e2a4b8f01c37" {
		t.Errorf("коммит %q — не sha корневого дерева", cat.Commit)
	}
	if cat.ETag != fixtureETag {
		t.Errorf("ETag %q, ожидался %q", cat.ETag, fixtureETag)
	}
	if cat.FetchedAt.IsZero() {
		t.Error("FetchedAt не проставлен — Stale() станет вечно истинным")
	}

	hits, cond := s.counts()
	if hits != 4 {
		t.Errorf("запросов %d, ожидалось 4 (корень, geo, geosite, geoip)", hits)
	}
	if cond != 0 {
		t.Errorf("холодная загрузка ушла с If-None-Match %d раз — ETag ещё неоткуда взять", cond)
	}
	if !strings.Contains(j.text(), "каталог geosite: 4 имён, 1 с geoip, коммит 9b1ce4a0d2f7") {
		t.Errorf("журнал: %q", j.text())
	}
}

// TestGetInsideTTLDoesNotTouchGitHub — свежий кэш стоит ноль запросов.
//
// Ради этого кэш и заведён: без него открытие вкладки панели стоит четырёх
// запросов из шестидесяти в час.
func TestGetInsideTTLDoesNotTouchGitHub(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)

	first, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	before, _ := s.counts()

	for i := 0; i < 5; i++ {
		again, err := c.Get(context.Background())
		if err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
		if again != first {
			t.Fatalf("Get #%d вернул другой каталог — кэш подменяется без нужды", i)
		}
	}
	if after, _ := s.counts(); after != before {
		t.Errorf("внутри TTL ушло %d лишних запросов", after-before)
	}
}

// TestGetAfterTTLReturnsOldAndRevalidates — после TTL старый список отдаётся
// СРАЗУ, а обновление идёт в фоне и стоит одного запроса с If-None-Match.
//
// 304 не тратит лимит GitHub — на этом и держится вся экономия: продление
// свежести каталога бесплатно, пока ветка meta не двинулась.
func TestGetAfterTTLReturnsOldAndRevalidates(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, done := newClient(t, s, time.Millisecond, j)

	first, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // ждём только истечения TTL

	gate := make(chan struct{})
	s.set(func(s *stand) { s.gate = gate })

	stale, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get после TTL: %v", err)
	}
	// Обновление заперто на стенде, а Get уже вернулся: значит вернулся он
	// из кэша, а не дождавшись сети.
	if stale != first {
		t.Fatal("после TTL Get не отдал старый каталог сразу")
	}
	if !c.Stale(stale) {
		t.Error("Stale() врёт: каталог старше TTL")
	}
	close(gate)
	waitRefresh(t, done)

	fresh := c.Current()
	if fresh == first {
		t.Fatal("фон не заменил каталог — FetchedAt не продлён")
	}
	if !fresh.FetchedAt.After(first.FetchedAt) {
		t.Errorf("FetchedAt не продлён: было %v, стало %v", first.FetchedAt, fresh.FetchedAt)
	}
	if fresh.Commit != first.Commit || strings.Join(fresh.Names, ",") != strings.Join(first.Names, ",") {
		t.Error("304 обязан сохранить имена и коммит — перекачивать было нечего")
	}
	if c.Stale(fresh) {
		t.Error("после обновления каталог обязан быть свежим")
	}
	hits, cond := s.counts()
	if hits != 5 || cond != 1 {
		t.Errorf("запросов %d (условных %d), ожидалось 5 и 1: ревалидация — ОДИН запрос корня", hits, cond)
	}
	if !strings.Contains(j.text(), "без изменений (304)") {
		t.Errorf("журнал не отличает 304 от перезагрузки: %q", j.text())
	}
}

// TestGetStartsOneRefreshForManyCallers — сколько бы вкладок ни открыли,
// в GitHub уходит одно обновление.
//
// Без замка панель на трёх устройствах превращает один просроченный кэш в
// три холодных загрузки, то есть в двенадцать запросов из шестидесяти.
func TestGetStartsOneRefreshForManyCallers(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, done := newClient(t, s, time.Millisecond, j)

	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	gate := make(chan struct{})
	s.set(func(s *stand) { s.gate = gate })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background()); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	close(gate)
	waitRefresh(t, done)

	if hits, cond := s.counts(); hits != 5 || cond != 1 {
		t.Errorf("запросов %d (условных %d), ожидалось 5 и 1: восемь вызовов — одно обновление", hits, cond)
	}
}

// TestRefreshRefusesTruncatedTree — обрезанное дерево не каталог.
//
// GitHub обрезает ответ молча, полем truncated. Принять его значит показать
// владельцу неполный список и отбить его собственное имя как несуществующее.
func TestRefreshRefusesTruncatedTree(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)
	s.set(func(s *stand) { s.truncPath = "/trees/geosite" })

	_, err := c.Get(context.Background())
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("ошибка %v, ожидалась ErrTruncated", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("ошибка %v не опознаётся как ErrUnavailable — панели нечем ответить 503", err)
	}
	if c.Current() != nil {
		t.Error("кэш заполнен неполным каталогом")
	}
}

// TestRefreshRefusesOversizedBody — потолок тела: отказ, а не усечение
// (ADR-0022). Обрезанный JSON дал бы загадочную ошибку парсера, и владелец
// пошёл бы искать поломку у нас.
func TestRefreshRefusesOversizedBody(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)
	s.set(func(s *stand) { s.oversize = true })

	_, err := c.Get(context.Background())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ошибка %v, ожидалась ErrTooLarge", err)
	}
	if c.Current() != nil {
		t.Error("кэш заполнен после отказа по размеру")
	}
}

// TestGetWithoutCacheReportsUnavailable — отказ GitHub при пустом кэше
// обязан опознаваться одним сентинелом: панель отвечает 503
// catalog_unavailable, а не «500 внутренняя ошибка».
func TestGetWithoutCacheReportsUnavailable(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)
	s.set(func(s *stand) { s.status = http.StatusInternalServerError })

	_, err := c.Get(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ошибка %v, ожидалась ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("в ошибке %v нет кода ответа — владельцу не с чем идти разбираться", err)
	}
}

// TestGetKeepsOldCatalogWhenRefreshFails — упавшее обновление не отнимает
// у владельца рабочий каталог.
//
// Ночью с плохим аплинком GitHub недоступен, а имена проверять надо: старый
// список остаётся, отказ уходит одной строкой в журнал, ошибки наружу нет.
func TestGetKeepsOldCatalogWhenRefreshFails(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, done := newClient(t, s, time.Millisecond, j)

	first, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	s.set(func(s *stand) { s.status = http.StatusInternalServerError })

	again, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get при упавшем обновлении вернул ошибку: %v", err)
	}
	if again != first {
		t.Fatal("Get не отдал старый каталог")
	}
	waitRefresh(t, done)

	if c.Current() != first {
		t.Error("неудачное обновление затёрло кэш")
	}
	if !c.Stale(first) {
		t.Error("каталог обязан числиться просроченным: обновиться не удалось")
	}
	if !strings.Contains(j.text(), "каталог geosite: не обновлён") {
		t.Errorf("отказ фона не попал в журнал: %q", j.text())
	}
}

// TestFetchRefusesRedirect — за перенаправлением не идём.
//
// Причина та же, что у подписки (internal/happ/fetch.go): проверка схемы
// смотрит только на исходный адрес, а http.Client по умолчанию сходил бы за
// «302 Location: http://…» и перепроверять схему не стал бы. Каталог — не
// секрет, но подменённый список имён — это чужие правила маршрутизации в
// конфиге владельца.
func TestFetchRefusesRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"x","tree":[],"truncated":false}`))
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	// HTTP: nil — берётся боевой клиент пакета: именно его настройку и надо
	// проверять, а не копию рядом.
	c := &Client{BaseURL: srv.URL}
	_, err := c.Get(context.Background())
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("ошибка %v, ожидалась ErrRedirect", err)
	}
}

// TestFetchSendsGitHubHeaders — Accept и User-Agent часть контракта: без
// Accept GitHub вправе отдать другую версию представления, без User-Agent
// api.github.com отвечает 403.
func TestFetchSendsGitHubHeaders(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)

	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	s.mu.Lock()
	hdr := s.lastHdr
	s.mu.Unlock()
	if got := hdr.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept %q", got)
	}
	if got := hdr.Get("User-Agent"); got != userAgent {
		t.Errorf("User-Agent %q, ожидался %q", got, userAgent)
	}
}

// TestHTTPSRequiredOnlyForDefaultBase — правило схемы.
//
// Боевой адрес зашит в пакет и всегда https; проверка стоит на адресах,
// которые GitHub присылает В ТЕЛЕ ответа (url поддеревьев) — по ним клиент
// ходит, и подменённый url увёл бы обход на открытый провод. Явно заданный
// BaseURL — стенд разработчика и httptest, там http разрешён: иначе ни один
// тест этого файла не мог бы существовать.
func TestHTTPSRequiredOnlyForDefaultBase(t *testing.T) {
	if err := (&Client{}).checkURL("http://api.github.com/repos/x/y/git/trees/meta"); err == nil {
		t.Error("http принят при пустом BaseURL")
	}
	if err := (&Client{}).checkURL("https://api.github.com/repos/x/y/git/trees/meta"); err != nil {
		t.Errorf("https отвергнут: %v", err)
	}
	if err := (&Client{BaseURL: "http://127.0.0.1:8080"}).checkURL("http://127.0.0.1:8080/trees/geo"); err != nil {
		t.Errorf("явный BaseURL обязан разрешать http: %v", err)
	}
}

func TestFileURL(t *testing.T) {
	cases := []struct {
		kind Kind
		name string
		want string
	}{
		{KindSite, "youtube", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/youtube.mrs"},
		{KindIP, "telegram", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/telegram.mrs"},
		{KindSite, "category-ai-!cn", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/category-ai-!cn.mrs"},
	}
	for _, c := range cases {
		if got := FileURL(c.kind, c.name); got != c.want {
			t.Errorf("FileURL(%s, %s) = %q, ожидался %q", c.kind, c.name, got, c.want)
		}
	}
}

// TestPacks — паки это обещание панели, а не украшение: их имена уезжают в
// конфиг владельца как есть.
func TestPacks(t *testing.T) {
	want := map[string][]string{
		"social":     {"facebook", "instagram", "twitter", "tiktok", "reddit"},
		"messengers": {"telegram", "whatsapp", "discord"},
		"ai":         {"openai", "anthropic", "category-ai-!cn"},
		"video":      {"youtube", "netflix", "spotify", "twitch"},
		"work":       {"github", "linkedin"},
	}
	if len(Packs) != len(want) {
		t.Fatalf("паков %d, ожидалось %d", len(Packs), len(want))
	}
	for _, p := range Packs {
		w, ok := want[p.ID]
		if !ok {
			t.Errorf("неизвестный пак %q", p.ID)
			continue
		}
		if strings.Join(p.Sets, ",") != strings.Join(w, ",") {
			t.Errorf("пак %s: %v, ожидался %v", p.ID, p.Sets, w)
		}
	}
}

// TestCatalogLookups — Has по отсортированному списку (двоичный поиск) и
// HasIP по карте.
func TestCatalogLookups(t *testing.T) {
	cat := newCatalog("sha", "etag", time.Now(),
		[]string{"category-ai-!cn", "netflix@ads", "telegram", "youtube"},
		map[string]bool{"telegram": true, "youtube": true},
	)
	for _, n := range []string{"category-ai-!cn", "netflix@ads", "telegram", "youtube"} {
		if !cat.Has(n) {
			t.Errorf("Has(%q) = false", n)
		}
	}
	for _, n := range []string{"", "telegra", "telegramm", "zzz", "aaa"} {
		if cat.Has(n) {
			t.Errorf("Has(%q) = true", n)
		}
	}
	if !cat.HasIP("youtube") || cat.HasIP("netflix@ads") {
		t.Error("HasIP считает не то")
	}
	if got := strings.Join(cat.IPNames(), ","); got != "telegram,youtube" {
		t.Errorf("IPNames = %q, ожидался отсортированный список", got)
	}
	// nil-каталог отвечает «нет», а не паникует: обработчик PUT спрашивает
	// раньше, чем каталог загрузился.
	var empty *Catalog
	if empty.Has("telegram") || empty.HasIP("telegram") || empty.IPNames() != nil {
		t.Error("nil-каталог обязан отвечать «нет» без паники")
	}
}

func TestStaleWithoutCatalog(t *testing.T) {
	c := &Client{}
	if !c.Stale(nil) {
		t.Error("отсутствующий каталог обязан числиться просроченным")
	}
	if c.Current() != nil {
		t.Error("Current() на пустом клиенте обязан быть nil")
	}
}

// TestFailedRefreshHoldsOffNextAttempt — упавшее обновление выдерживает
// паузу, а не долбится в GitHub на каждое открытие панели.
//
// Без паузы просроченный кэш при лежащем GitHub означает по запросу на
// КАЖДЫЙ Get: панель опрашивает вкладку, каждый опрос стоит запроса, и
// шестьдесят запросов в час выбираются за минуту — после чего GitHub
// отвечает 403 уже всем, включая честную загрузку, когда сеть вернётся.
// То есть отсутствие паузы превращает временный отказ в наведённый на себя
// самого бан.
func TestFailedRefreshHoldsOffNextAttempt(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, done := newClient(t, s, time.Millisecond, j)
	// Полсекунды, а не сотня миллисекунд: пять Get и ожидание фона под
	// -race на загруженной машине легко переползают через сотню, и тест
	// краснел бы от нагрузки, а не от поведения.
	c.retryHold = 500 * time.Millisecond

	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // ждём только истечения TTL
	s.set(func(s *stand) { s.status = http.StatusInternalServerError })

	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("Get при лежащем GitHub: %v", err)
	}
	waitRefresh(t, done)
	afterFail, _ := s.counts()
	if afterFail != 5 {
		t.Fatalf("запросов %d, ожидалось 5 (холодная загрузка плюс один неудачный корень)", afterFail)
	}

	// Внутри паузы обновление не начинается вовсе — ни одного запроса,
	// сколько бы раз панель ни спросила.
	for i := 0; i < 5; i++ {
		if _, err := c.Get(context.Background()); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	// Ждём хук, а не считаем запросы сразу: обновление уходит в фон, и
	// счётчик через миллисекунду после Get одинаков и у клиента с паузой,
	// и у клиента без неё — такая проверка была бы зелёной всегда. Ожидание
	// заодно перекрывает саму паузу, поэтому спать перед последней
	// попыткой уже не нужно.
	select {
	case <-done:
		t.Fatal("внутри паузы началось обновление — пауза не держит")
	case <-time.After(700 * time.Millisecond):
	}
	if hits, _ := s.counts(); hits != afterFail {
		t.Errorf("внутри паузы ушло %d лишних запросов", hits-afterFail)
	}

	// А после паузы — ровно одна новая попытка.
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("Get после паузы: %v", err)
	}
	waitRefresh(t, done)
	if hits, _ := s.counts(); hits != afterFail+1 {
		t.Errorf("после паузы запросов %d, ожидалось %d: попытка обязана возобновиться ровно одна",
			hits, afterFail+1)
	}
}

// TestCatalogNamesAreSortedOnBuild — сортировка в newCatalog не украшение.
//
// Has ищет двоичным поиском, а тот на несортированном списке не жалуется —
// он просто не находит имя. Сломай сортировку, и половина имён каталога
// станет «несуществующей»: панель отобьёт при сохранении набор, который
// сама же показала в списке. Остальные тесты этого не увидят, потому что
// фикстуры деревьев отсортированы сами.
func TestCatalogNamesAreSortedOnBuild(t *testing.T) {
	cat := NewForTest([]string{"youtube", "telegram", "anthropic"}, nil)

	if strings.Join(cat.Names, ",") != "anthropic,telegram,youtube" {
		t.Fatalf("имена %v не отсортированы", cat.Names)
	}
	// anthropic лежит в конце несортированного списка, и двоичный поиск
	// прошёл бы мимо него — на нём и держится проверка.
	if !cat.Has("anthropic") {
		t.Error("Has не нашёл имя, поданное вне порядка: список не отсортирован")
	}
}

// TestOddNamesFromRepoAreDropped — имя с пробелом или апострофом в каталог
// не попадает.
//
// Имена приходят из ЧУЖОГО репозитория, а уезжают в две конструкции, где
// посторонний знак ломает не наш разбор, а работу движка: строку правила
// mixin.yaml в одинарных кавычках и машинное состояние sets=… через запятую.
// Имя вида «it's» дало бы битый YAML при следующем старте nikki, имя с
// запятой — состояние, которое Parse разбирает как два набора. Отбрасывать
// такое надо там, где оно появляется, а не там, где оно взрывается.
func TestOddNamesFromRepoAreDropped(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)

	cat, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	for _, bad := range []string{"bad name", "it's"} {
		if cat.Has(bad) {
			t.Errorf("имя %q попало в каталог — оно уедет в mixin.yaml как есть", bad)
		}
	}
	if len(cat.Names) != 4 {
		t.Errorf("имён %d (%v), ожидалось 4: посторонние отброшены, остальные на месте", len(cat.Names), cat.Names)
	}
	// Молчаливая пропажа имени необъяснима: список короче, чем в
	// репозитории, и почему — не видно нигде.
	if !strings.Contains(j.text(), "пропущено") {
		t.Errorf("журнал молчит о пропущенных именах: %q", j.text())
	}
}

// TestColdGetFinishesRefreshInBackground — оборванный холодный обход
// доводится фоном.
//
// Панель ждёт побочный список восемь секунд, а холодный обход — четыре
// последовательных запроса в GitHub. На медленном аплинке каждая попытка
// умирает вместе с запросом панели, кэш остаётся пустым, и «Повторить»
// начинает с нуля СНОВА И СНОВА — каталога владелец не получает никогда.
// Поэтому неудача синхронной попытки запускает один фоновый обход со своим
// сроком: следующее нажатие обязано найти готовый каталог.
func TestColdGetFinishesRefreshInBackground(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, done := newClient(t, s, time.Hour, j)

	arrived := make(chan struct{}, 8)
	gate := make(chan struct{})
	s.set(func(s *stand) { s.arrived, s.gate = arrived, gate })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx)
		errCh <- err
	}()

	// Контекст рвётся не по часам, а по факту: клиент уже в сети, запрос
	// упёрся в шлюз. Сон «на глазок» сделал бы тест зелёным или красным по
	// нагрузке машины, а не по поведению кода.
	waitArrived(t, arrived)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Get отдал каталог, хотя обход оборвался")
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("ошибка %v не опознаётся как ErrUnavailable — панели нечем ответить 503", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get не вернулся после отмены контекста — он привязан не к тому сроку")
	}

	// Шлюз открыт: фоновый обход, начатый вместо оборванного, обязан
	// дойти до конца сам.
	close(gate)
	waitRefresh(t, done)

	cat := c.Current()
	if cat == nil {
		t.Fatal("фон не довёз каталог: «Повторить» начнёт всё с нуля")
	}

	before, _ := s.counts()
	again, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get после фона: %v", err)
	}
	if again != cat {
		t.Error("Get отдал не тот снимок, что принёс фон")
	}
	if after, _ := s.counts(); after != before {
		t.Errorf("Get после фона сходил в GitHub ещё %d раз — кэша он не нашёл", after-before)
	}
}

// TestConcurrentColdGetsWalkGitHubOnce — холодных вызовов много, обход один.
//
// То же правило «одно обновление за раз», что и у просроченного кэша, но на
// холодном пути: панель, открытая на трёх устройствах, иначе превратила бы
// пустой кэш в три обхода — двенадцать запросов из шестидесяти в час.
func TestConcurrentColdGetsWalkGitHubOnce(t *testing.T) {
	s := newStand(t)
	j := &journal{}
	c, _ := newClient(t, s, time.Hour, j)

	arrived := make(chan struct{}, 8)
	gate := make(chan struct{})
	s.set(func(s *stand) { s.arrived, s.gate = arrived, gate })

	start := make(chan struct{})
	got := make([]*Catalog, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = c.Get(context.Background())
		}(i)
	}
	close(start)

	// Пока первый заперт шлюзом, остальные обязаны ждать ЕГО, а не идти
	// в GitHub своей дорогой.
	waitArrived(t, arrived)
	close(gate)
	wg.Wait()

	for i := range got {
		if errs[i] != nil {
			t.Fatalf("Get #%d: %v", i, errs[i])
		}
		if got[i] == nil || got[i] != got[0] {
			t.Errorf("Get #%d отдал другой снимок: обходов было больше одного", i)
		}
	}
	if hits, _ := s.counts(); hits != 4 {
		t.Errorf("запросов %d, ожидалось 4: три холодных вызова — один обход", hits)
	}
}
