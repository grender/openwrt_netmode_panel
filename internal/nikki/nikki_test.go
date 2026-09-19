package nikki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeClash отвечает формой, снятой с живого роутера.
type fakeClash struct {
	body         string
	status       int
	putStatus    int
	deleteStatus int
	wantSecret   string
	lastPUT      string
	lastDELETE   string
	lastBody     string
	// lastPUTRaw — путь ДО раскодирования процентов. r.URL.Path сервер
	// раскодирует, поэтому по нему «имя экранировали» и «имя вставили как
	// есть» неотличимы — а разница между ними в том, уводит ли слэш внутри
	// имени запрос на посторонний маршрут.
	lastPUTRaw string
	// putMessage — тело {"message": …} при putStatus ≥ 400: живой mihomo
	// кладёт туда причину отказа, и клиент обязан её донести.
	putMessage string
	// providerBody — ответ GET /providers/proxies/{name}.
	providerBody string
	// providersBody — ответ GET /providers/proxies (без имени). Пустой
	// означает движок без провайдеров: {"providers":{}}. Отдельное поле,
	// потому что Proxies ходит туда ВСЕГДА — узлы провайдеров в /proxies
	// не попадают, и без этого ответа список узлов пуст.
	providersBody string
	// rulesBody — ответ GET /providers/rules.
	rulesBody string
}

func (f *fakeClash) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Секрет обязан приходить заголовком: ?secret= на живом роутере
	// отвечает Unauthorized (docs/recon/nikki.md).
	if f.wantSecret != "" {
		if r.Header.Get("Authorization") != "Bearer "+f.wantSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
			return
		}
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if r.Method == http.MethodPut {
		f.lastPUT = r.URL.Path
		f.lastPUTRaw = r.URL.EscapedPath()
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		f.lastBody = string(b)
		if f.putStatus != 0 {
			w.WriteHeader(f.putStatus)
			if f.putMessage != "" {
				_, _ = w.Write([]byte(`{"message":` + strconv.Quote(f.putMessage) + `}`))
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/providers/proxies/") {
		if f.providerBody == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Resource not found"}`))
			return
		}
		_, _ = w.Write([]byte(f.providerBody))
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/providers/rules" {
		if f.rulesBody == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Resource not found"}`))
			return
		}
		_, _ = w.Write([]byte(f.rulesBody))
		return
	}
	if r.Method == http.MethodDelete {
		f.lastDELETE = r.URL.Path
		if f.deleteStatus != 0 {
			w.WriteHeader(f.deleteStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.URL.Path {
	case "/version":
		_, _ = w.Write([]byte(`{"meta":true,"version":"v1.19.27"}`))
	case "/proxies":
		_, _ = w.Write([]byte(f.body))
	case "/providers/proxies":
		if f.providersBody == "" {
			_, _ = w.Write([]byte(`{"providers":{}}`))
			return
		}
		_, _ = w.Write([]byte(f.providersBody))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// realProxiesBody собирает ответ /proxies из снятых фикстур: узлы из
// raw/60 плюс группы из raw/61 — так тесты видят настоящую раскладку,
// а не придуманную.
func realProxiesBody(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "docs", "recon", "raw")

	nodes, err := os.ReadFile(filepath.Join(dir, "60-clash-proxies.json"))
	if err != nil {
		t.Fatalf("фикстура узлов: %v", err)
	}
	var nodeDoc struct {
		Proxies map[string]json.RawMessage `json:"proxies"`
	}
	if err := json.Unmarshal(nodes, &nodeDoc); err != nil {
		t.Fatalf("разбор узлов: %v", err)
	}

	groups, err := os.ReadFile(filepath.Join(dir, "61-clash-groups.json"))
	if err != nil {
		t.Fatalf("фикстура групп: %v", err)
	}
	var gs []struct {
		Name string   `json:"name"`
		Type string   `json:"type"`
		Now  string   `json:"now"`
		All  []string `json:"all"`
	}
	if err := json.Unmarshal(groups, &gs); err != nil {
		t.Fatalf("разбор групп: %v", err)
	}

	all := map[string]any{}
	for k, v := range nodeDoc.Proxies {
		if strings.HasPrefix(k, "_") {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(v, &m)
		all[k] = m
	}
	for _, g := range gs {
		all[g.Name] = map[string]any{
			"name": g.Name, "type": g.Type, "now": g.Now,
			"all": g.All, "alive": true, "history": []any{},
		}
	}
	b, _ := json.Marshal(map[string]any{"proxies": all})
	return string(b)
}

func newClient(t *testing.T, secret string) (*fakeClash, *HTTP, func()) {
	t.Helper()
	f := &fakeClash{body: realProxiesBody(t), wantSecret: secret}
	srv := httptest.NewServer(f)
	return f, New(srv.URL, secret), srv.Close
}

// ─────────── аутентификация ───────────

func TestSecretGoesInBearerHeader(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("Version с верным секретом: %v", err)
	}

	// Неверный секрет — та же недоступность: подбирать его мы не станем.
	bad := New(c.BaseURL, "неверный")
	if _, err := bad.Version(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("неверный секрет → %v, ожидалась ErrUnavailable", err)
	}
}

// ─────────── чтение ───────────

func TestProxiesFromRealFixture(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	all, err := c.Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}

	proxy, ok := all["PROXY"]
	if !ok {
		t.Fatal("группа PROXY не найдена")
	}
	// Главный факт разведки: PROXY — URLTest, а не Selector.
	if proxy.Type != "URLTest" {
		t.Errorf("тип PROXY = %q, на этом роутере URLTest", proxy.Type)
	}
	if !proxy.IsGroup() || len(proxy.Members) != 29 {
		t.Errorf("участников %d, ожидалось 29", len(proxy.Members))
	}
	if proxy.Now != "🇨🇭⚡Швейцария 2" {
		t.Errorf("now = %q", proxy.Now)
	}

	bypass := all["BYPASS"]
	if bypass.Type != "Fallback" || len(bypass.Members) != 2 {
		t.Errorf("BYPASS = %+v", bypass)
	}
	global := all["GLOBAL"]
	if global.Type != "Selector" {
		t.Errorf("GLOBAL = %+v", global)
	}
}

// Ручной выбор принимает ЛЮБАЯ группа: mihomo приводит цель к интерфейсу
// SelectAble, а не к конкретному типу, и Selector, URLTest и Fallback его
// реализуют (hub/route/proxies.go).
func TestEveryGroupIsSelectable(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	all, _ := c.Proxies(context.Background())
	for _, name := range []string{"PROXY", "BYPASS", "GLOBAL"} {
		if !all[name].Selectable {
			t.Errorf("%s (тип %s): selectable=false", name, all[name].Type)
		}
	}
	// А обычный узел — не группа и выбора не принимает.
	if all["203.0.113.10:443"].Selectable {
		t.Error("узел помечен как принимающий выбор")
	}
}

// Самая опасная деталь ответа mihomo: delay:0 в истории означает
// НЕСОСТОЯВШУЮСЯ пробу, а не нулевую задержку. Показать такой узел
// как самый быстрый — прямо неверно.
func TestZeroDelayMeansNoProbeNotZeroMilliseconds(t *testing.T) {
	f := &fakeClash{body: `{"proxies":{
		"мёртвый":{"name":"мёртвый","type":"Vless","alive":true,
			"history":[{"time":"t1","delay":40},{"time":"t2","delay":0}]},
		"живой":{"name":"живой","type":"Vless","alive":true,
			"history":[{"time":"t1","delay":0},{"time":"t2","delay":38}]},
		"нетистории":{"name":"нетистории","type":"Vless","alive":false,"history":[]}
	}}`}
	srv := httptest.NewServer(f)
	defer srv.Close()

	all, err := New(srv.URL, "").Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}

	if d := all["мёртвый"].DelayMS; d != nil {
		t.Errorf("последняя проба не прошла, а задержка = %d; ожидался nil", *d)
	}
	if d := all["живой"].DelayMS; d == nil || *d != 38 {
		t.Errorf("задержка живого = %v, ожидалось 38", d)
	}
	if d := all["нетистории"].DelayMS; d != nil {
		t.Errorf("без истории задержка = %v, ожидался nil", d)
	}
}

func TestMembersSkipsSeparators(t *testing.T) {
	// «⬇️ Обходы белых списков ⬇️» — разделитель провайдера: он есть
	// в списке участников, но узлом не является.
	_, c, done := newClient(t, "118296")
	defer done()

	all, _ := c.Proxies(context.Background())
	members := Members(all, "PROXY")

	// В фикстуре узлов всего два, остальные имена — только в списке
	// участников группы, поэтому разворачиваются лишь существующие.
	for _, m := range members {
		if strings.Contains(m.Name, "⬇️") {
			t.Errorf("разделитель попал в узлы: %q", m.Name)
		}
	}
	if len(members) == 0 {
		t.Error("ни один участник не развернулся в узел")
	}
}

func TestGroupsSorted(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	all, _ := c.Proxies(context.Background())
	gs := Groups(all)
	if len(gs) != 3 {
		t.Fatalf("групп %d, ожидалось 3", len(gs))
	}
	for i := 1; i < len(gs); i++ {
		if gs[i-1].Name > gs[i].Name {
			t.Errorf("порядок групп нестабилен: %v", []string{gs[i-1].Name, gs[i].Name})
		}
	}
}

// ─────────── выбор ───────────

// Закрепление работает и у URLTest — это и был мой неверный вывод,
// исправленный по исходникам: mihomo хранит выбор в поле selected и
// обходит автоподбор, пока оно непусто.
func TestSelectWorksOnURLTest(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()

	if err := c.Select(context.Background(), "PROXY", "🇵🇱⚡Польша"); err != nil {
		t.Fatalf("Select у URLTest: %v", err)
	}
	if f.lastPUT != "/proxies/PROXY" {
		t.Errorf("PUT ушёл на %q", f.lastPUT)
	}
	if !strings.Contains(f.lastBody, "Польша") {
		t.Errorf("тело PUT = %q", f.lastBody)
	}
}

// Возврат к автовыбору — DELETE. Это и есть пункт AUTO из SPEC §7,
// встроенный в движок: правка профиля не нужна.
func TestUnfixReturnsToAuto(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()

	if err := c.Unfix(context.Background(), "PROXY"); err != nil {
		t.Fatalf("Unfix: %v", err)
	}
	if f.lastDELETE != "/proxies/PROXY" {
		t.Errorf("DELETE ушёл на %q", f.lastDELETE)
	}
}

// ─────────── перезагрузка провайдера ───────────

// Обновление подписки не заканчивается записью файла: список узлов живёт
// в памяти движка, и без этого PUT панель показывает вчерашние узлы.
func TestReloadProviderPutsOnProviderPath(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()

	if err := c.ReloadProvider(context.Background(), "subscription"); err != nil {
		t.Fatalf("ReloadProvider: %v", err)
	}
	if f.lastPUT != "/providers/proxies/subscription" {
		t.Errorf("PUT ушёл на %q", f.lastPUT)
	}
	if f.lastBody != "" {
		t.Errorf("тело у перезагрузки лишнее: %q", f.lastBody)
	}
}

// Слэш внутри имени провайдера обязан уехать как %2F. Иначе запрос
// молча попадёт на другой маршрут — и вместо отказа мы получим успех
// от чего-то постороннего.
func TestReloadProviderEscapesName(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()

	_ = c.ReloadProvider(context.Background(), "мой/провайдер")

	if strings.Contains(strings.TrimPrefix(f.lastPUTRaw, "/providers/proxies/"), "/") {
		t.Errorf("имя ушло неэкранированным: %q", f.lastPUTRaw)
	}
}

// Провайдера с таким именем движок не знает: имя разошлось с профилем.
// Это ErrNotFound, а не «недоступен» — движок ответил, и внятно.
func TestReloadProviderNotFound(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.putStatus = http.StatusNotFound

	if err := c.ReloadProvider(context.Background(), "нет-такого"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 на PUT → %v, ожидалась ErrNotFound", err)
	}
}

// 503 — худшее из состояний: файл на диске новый, список в движке старый.
// Отдельная ошибка и отдельный текст, потому что ErrUnavailable отправил бы
// владельца перезапускать nikki, а чинить надо содержимое файла.
func TestReloadProviderStaleOn503(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.putStatus = http.StatusServiceUnavailable

	err := c.ReloadProvider(context.Background(), "subscription")
	if !errors.Is(err, ErrProviderStale) {
		t.Fatalf("503 → %v, ожидалась ErrProviderStale", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("503 от живого движка выдан за недоступность Clash API")
	}
	// Текст обязан назвать расхождение: без него владелец пойдёт искать
	// причину в сети, а не в файле, который мы только что записали.
	for _, want := range []string{"файл", "новый", "стар"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("в тексте %q нет %q", err.Error(), want)
		}
	}
}

// Прочие пятисотки остаются недоступностью. Регрессия на то, что код
// ответа теперь едет внутри ошибки: разбирать его по числу можно только
// там, где число что-то значит, и 500 к ErrProviderStale отношения не имеет.
func TestReloadProviderOther5xxStaysUnavailable(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.putStatus = http.StatusInternalServerError

	err := c.ReloadProvider(context.Background(), "subscription")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("500 → %v, ожидалась ErrUnavailable", err)
	}
	if errors.Is(err, ErrProviderStale) {
		t.Errorf("500 принят за отказ перечитать провайдера: %v", err)
	}
}

// Поле fixed отличает «закреплено руками» от «движок так решил».
// По одному now это неразличимо.
func TestFixedDistinguishesPinnedFromAuto(t *testing.T) {
	f := &fakeClash{body: `{"proxies":{
		"АВТО":{"name":"АВТО","type":"URLTest","alive":true,"all":["a"],"now":"a","fixed":"","history":[]},
		"ЗАКРЕП":{"name":"ЗАКРЕП","type":"URLTest","alive":true,"all":["a"],"now":"a","fixed":"a","history":[]}
	}}`}
	srv := httptest.NewServer(f)
	defer srv.Close()

	all, err := New(srv.URL, "").Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	if all["АВТО"].Pinned || all["АВТО"].Fixed != "" {
		t.Error("группа без fixed не должна считаться закреплённой")
	}
	if !all["ЗАКРЕП"].Pinned || all["ЗАКРЕП"].Fixed != "a" {
		t.Errorf("закреплённая группа: %+v", all["ЗАКРЕП"])
	}
}

// Когда движок отвечает 400 «Must be a Selector», это ErrNotSelectable,
// а не безымянная ошибка.
func TestMustBeSelectorMapsToNotSelectable(t *testing.T) {
	f := &fakeClash{
		body:      `{"proxies":{"Г":{"name":"Г","type":"LoadBalance","alive":true,"all":["a"],"history":[]}}}`,
		putStatus: http.StatusBadRequest,
	}
	srv := httptest.NewServer(f)
	defer srv.Close()

	err := New(srv.URL, "").Select(context.Background(), "Г", "a")
	if !errors.Is(err, ErrNotSelectable) {
		t.Errorf("400 от движка → %v, ожидалась ErrNotSelectable", err)
	}
}

// Тот же 400, но у DELETE: у обычного Selector автовыбора нет, снимать нечего.
func TestUnfixOnSelectorIsNotSelectable(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.deleteStatus = http.StatusBadRequest

	if err := c.Unfix(context.Background(), "GLOBAL"); !errors.Is(err, ErrNotSelectable) {
		t.Errorf("400 на DELETE → %v, ожидалась ErrNotSelectable", err)
	}
}

// Регрессия: раньше ErrNotSelectable определялась поиском подстроки «код 400»
// в тексте ошибки, который собирается в do(). Теперь решение принимает тип,
// а не формулировка — и переписать текст StatusError можно, не сломав тихо
// HTTP-код панели.
//
// Проверяется на 400 с ДРУГОГО маршрута: текст ошибки тот же самый, но
// «Must be a Selector» здесь ни при чём, и подменять её нечем.
func TestStatusErrorCarriesCodeNotText(t *testing.T) {
	f := &fakeClash{status: http.StatusBadRequest}
	srv := httptest.NewServer(f)
	defer srv.Close()

	_, err := New(srv.URL, "").Version(context.Background())

	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("400 → %v, ожидалась *StatusError", err)
	}
	if se.Code != http.StatusBadRequest || se.Path != "/version" {
		t.Errorf("StatusError = %+v", se)
	}
	if errors.Is(err, ErrNotSelectable) {
		t.Error("400 с /version принят за «группа не допускает выбор»")
	}
}

// 4xx, который смысла для нас не имеет, остаётся собой: не ErrNotSelectable,
// не ErrUnavailable — иначе панель показала бы «движок недоступен» на ровном
// месте, а он ответил.
func TestOtherClientErrorStaysStatusError(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.putStatus = http.StatusConflict

	err := c.Select(context.Background(), "GLOBAL", "PROXY")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusConflict {
		t.Fatalf("409 → %v, ожидалась *StatusError с кодом 409", err)
	}
	if errors.Is(err, ErrNotSelectable) || errors.Is(err, ErrUnavailable) {
		t.Errorf("409 подменён другой ошибкой: %v", err)
	}
}

func TestSelectOnSelectorAlsoWorks(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()

	if err := c.Select(context.Background(), "GLOBAL", "PROXY"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if f.lastPUT != "/proxies/GLOBAL" {
		t.Errorf("PUT ушёл на %q", f.lastPUT)
	}
	if !strings.Contains(f.lastBody, "PROXY") {
		t.Errorf("тело PUT = %q", f.lastBody)
	}
}

func TestSelectUnknownGroupOrMember(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	if err := c.Select(context.Background(), "НЕТТАКОЙ", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("неизвестная группа → %v", err)
	}
	if err := c.Select(context.Background(), "GLOBAL", "нет-такого-узла"); !errors.Is(err, ErrNotFound) {
		t.Errorf("неизвестный участник → %v", err)
	}
	// Узел — не группа.
	if err := c.Select(context.Background(), "203.0.113.10:443", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("узел вместо группы → %v", err)
	}
}

// ─────────── недоступность ───────────

func TestUnavailable(t *testing.T) {
	c := New("http://127.0.0.1:1", "секрет")
	for _, tt := range []struct {
		name string
		call func() error
	}{
		{"version", func() error { _, err := c.Version(context.Background()); return err }},
		{"proxies", func() error { _, err := c.Proxies(context.Background()); return err }},
		{"select", func() error { return c.Select(context.Background(), "PROXY", "x") }},
		{"unfix", func() error { return c.Unfix(context.Background(), "PROXY") }},
		{"reload", func() error { return c.ReloadProvider(context.Background(), "subscription") }},
		{"ruleProviders", func() error { _, err := c.RuleProviders(context.Background()); return err }},
	} {
		if err := tt.call(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestVersionFromFixture(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "v1.19.27" {
		t.Errorf("версия %q", v)
	}
}

func TestContextCancellation(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Proxies(ctx); err == nil {
		t.Error("отменённый контекст не прервал вызов")
	}
}

var _ Client = (*HTTP)(nil)

// TestReloadProviderCarriesEngineMessage — причина отказа из тела 503 доходит
// до текста ошибки. Без неё «proxy 3 error: …» и «permission denied» для
// владельца неразличимы, и обе выглядят как «движок отверг».
func TestReloadProviderCarriesEngineMessage(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.putStatus = http.StatusServiceUnavailable
	f.putMessage = "proxy 3 error: invalid REALITY public key"

	err := c.ReloadProvider(context.Background(), "sub")
	if !errors.Is(err, ErrProviderStale) {
		t.Fatalf("503 → %v, ожидалась ErrProviderStale", err)
	}
	if !strings.Contains(err.Error(), "invalid REALITY public key") {
		t.Fatalf("текст движка потерян: %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Message != f.putMessage {
		t.Fatalf("StatusError.Message не заполнен: %+v", se)
	}
}

// TestStatusErrorSurvivesBrokenBody — кривое тело не отнимает код ответа.
func TestStatusErrorSurvivesBrokenBody(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.putStatus = http.StatusServiceUnavailable
	f.putMessage = "" // тела нет вовсе

	err := c.ReloadProvider(context.Background(), "sub")
	if !errors.Is(err, ErrProviderStale) {
		t.Fatalf("503 без тела → %v", err)
	}
	if strings.Contains(err.Error(), "код 503:") {
		t.Fatalf("пустое сообщение не должно давать висящее двоеточие: %v", err)
	}
}

// TestProviderProxiesParsesNames — имена из proxies[].name, в порядке движка.
func TestProviderProxiesParsesNames(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.providerBody = `{"name":"sub","type":"Proxy","vehicleType":"File",
	  "proxies":[{"name":"🇩🇪⚡Германия","type":"Vless"},{"name":"🇵🇱⚡Польша","type":"Vless"}],
	  "updatedAt":"2026-09-06T10:00:00Z"}`

	names, err := c.ProviderProxies(context.Background(), "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "🇩🇪⚡Германия" || names[1] != "🇵🇱⚡Польша" {
		t.Fatalf("имена: %v", names)
	}
}

// TestProviderProxiesNotFound — нет такого провайдера → ErrNotFound, как у PUT.
func TestProviderProxiesNotFound(t *testing.T) {
	_, c, done := newClient(t, "118296")
	defer done()
	if _, err := c.ProviderProxies(context.Background(), "нет-такого"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 → %v, ожидалась ErrNotFound", err)
	}
}

// ─────────── провайдеры правил ───────────

// TestRuleProvidersDistinguishesDownloadedFromNever — нулевой updatedAt у
// одного провайдера и ненулевой у другого: ровно это отличает «файл ни разу
// не скачан» от «скачан и лежит», и по одному только ruleCount>0 это не
// проверить — у нескачанного он тоже может не быть нулём в чужих сборках.
func TestRuleProvidersDistinguishesDownloadedFromNever(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.rulesBody = `{"providers":{
		"geosite-ru":{"name":"geosite-ru","type":"Rule","vehicleType":"HTTP",
			"behavior":"Domain","format":"MrsRule","ruleCount":15234,
			"updatedAt":"2026-09-06T10:00:00Z"},
		"geosite-ads":{"name":"geosite-ads","type":"Rule","vehicleType":"HTTP",
			"behavior":"Domain","format":"MrsRule","ruleCount":0,
			"updatedAt":"0001-01-01T00:00:00Z"}
	}}`

	providers, err := c.RuleProviders(context.Background())
	if err != nil {
		t.Fatalf("RuleProviders: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("провайдеров %d, ожидалось 2", len(providers))
	}

	ru, ok := providers["geosite-ru"]
	if !ok {
		t.Fatal("geosite-ru не найден")
	}
	if ru.RuleCount != 15234 || ru.Behavior != "Domain" || ru.Format != "MrsRule" || ru.VehicleType != "HTTP" {
		t.Errorf("geosite-ru = %+v", ru)
	}
	if ru.UpdatedAt.IsZero() {
		t.Error("скачанный провайдер получил нулевой UpdatedAt")
	}

	ads, ok := providers["geosite-ads"]
	if !ok {
		t.Fatal("geosite-ads не найден")
	}
	if !ads.UpdatedAt.IsZero() {
		t.Errorf("нескачанный провайдер получил ненулевой UpdatedAt: %v", ads.UpdatedAt)
	}
	if ads.Name != "geosite-ads" {
		t.Errorf("имя = %q", ads.Name)
	}
}

// ─────────── узлы провайдеров ───────────

// providerShapeBody — форма живого ответа /providers/proxies, снятая с
// роутера 19.09.2026 и урезанная до сути: провайдер-файл `sub` со своими
// узлами и «совместимые» провайдеры, которые mihomo заводит на каждую
// группу. Имена групп в них повторяются — и перезаписывать ими группы из
// /proxies нельзя: у провайдерской копии нет ни now, ни fixed.
const providerShapeBody = `{"providers":{
  "sub": {"name":"sub","vehicleType":"File","proxies":[
    {"name":"Польша","type":"Vless","alive":true,"history":[{"time":"2026-09-19T14:34:33Z","delay":41}]},
    {"name":"Швейцария","type":"Vless","alive":false,"history":[{"time":"2026-09-19T14:34:33Z","delay":0}]}
  ]},
  "BYPASS": {"name":"BYPASS","vehicleType":"Compatible","proxies":[
    {"name":"PROXY","type":"URLTest","alive":true,"all":["Польша"]},
    {"name":"REJECT","type":"Reject","alive":true}
  ]}
}}`

// Узлы провайдера в /proxies НЕ попадают: там лежат только группы и
// встроенные DIRECT/REJECT/GLOBAL. Замер с роутера 19.09.2026 —
// 9 ключей верхнего уровня против 38 имён в PROXY.all, пересечение
// ПУСТОЕ. Пока Proxies спрашивал только /proxies, каждый узел подписки
// доезжал до панели как «движок его не принял», и выбрать было нечего,
// хотя zashboard те же 38 показывал: он читает список участников группы,
// а не карту узлов.
func TestProxiesPicksUpProviderNodes(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.body = `{"proxies":{
	  "PROXY":{"name":"PROXY","type":"URLTest","alive":true,"now":"Польша","all":["Польша","Швейцария"]},
	  "DIRECT":{"name":"DIRECT","type":"Direct","alive":true}
	}}`
	f.providersBody = providerShapeBody

	all, err := c.Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}

	for _, name := range []string{"Польша", "Швейцария"} {
		p, ok := all[name]
		if !ok {
			t.Fatalf("узел провайдера %q не доехал: есть только %v", name, keysOf(all))
		}
		if p.IsGroup() {
			t.Errorf("узел %q приехал группой: %+v", name, p)
		}
	}
	// Живое состояние берётся у движка, а не выдумывается: у Польши проба
	// прошла, у Швейцарии в истории 0 — то есть пробы не было, и наружу
	// это обязано уходить как nil, а не как ноль миллисекунд.
	if pl := all["Польша"]; !pl.Alive || pl.DelayMS == nil || *pl.DelayMS != 41 {
		t.Errorf("Польша = %+v, ожидались alive и 41 мс", pl)
	}
	if sw := all["Швейцария"]; sw.Alive || sw.DelayMS != nil {
		t.Errorf("Швейцария = %+v, ожидались мёртвый узел и nil задержки", sw)
	}
	// Ради этого всё и делалось: участники группы теперь разворачиваются.
	if ms := Members(all, "PROXY"); len(ms) != 2 {
		t.Errorf("участников группы развернулось %d, ожидалось 2", len(ms))
	}
}

// «Совместимый» провайдер повторяет каждую группу, но без now и fixed.
// Затереть им группу из /proxies значило бы потерять и текущий узел, и
// признак ручного закрепления — то есть именно то, чем панель отличает
// «владелец выбрал» от «движок решил».
func TestProviderCopiesNeverOverwriteGroups(t *testing.T) {
	f, c, done := newClient(t, "118296")
	defer done()
	f.body = `{"proxies":{
	  "PROXY":{"name":"PROXY","type":"URLTest","alive":true,"now":"Польша","fixed":"Польша","all":["Польша"]},
	  "REJECT":{"name":"REJECT","type":"Reject","alive":true}
	}}`
	f.providersBody = providerShapeBody

	all, err := c.Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}

	g := all["PROXY"]
	if g.Now != "Польша" || g.Fixed != "Польша" || !g.Pinned {
		t.Errorf("группу затёрло провайдерской копией: %+v", g)
	}
	if len(g.Members) != 1 {
		t.Errorf("участники группы = %v, ожидался один", g.Members)
	}
}

func keysOf(m map[string]Proxy) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
