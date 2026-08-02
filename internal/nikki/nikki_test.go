package nikki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClash отвечает формой, снятой с живого роутера.
type fakeClash struct {
	body       string
	status     int
	putStatus  int
	wantSecret string
	lastPUT    string
	lastDELETE string
	lastBody   string
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
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		f.lastBody = string(b)
		if f.putStatus != 0 {
			w.WriteHeader(f.putStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodDelete {
		f.lastDELETE = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.URL.Path {
	case "/version":
		_, _ = w.Write([]byte(`{"meta":true,"version":"v1.19.27"}`))
	case "/proxies":
		_, _ = w.Write([]byte(f.body))
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
