package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netmoded/internal/exits"
	"netmoded/internal/happ"
	"netmoded/internal/job"
	"netmoded/internal/mixin"
	"netmoded/internal/nikki"
)

// Экспорт ноды в форме node-export.yml репо выходных нод. Значения
// выдуманы; по ним же проверяется, что секреты наружу не уходят.
const (
	exportUUIDResi = "11111111-2222-4333-8444-555555555555"
	exportUUIDDC   = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	exportPBK      = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	exportSID      = "0123abcd"
)

func exportJSON(name, sni string) string {
	return fmt.Sprintf(`{"schema":1,"name":%q,"country":"kz","address":%q,
	  "vless":{"port":443,"flow":"xtls-rprx-vision","sni":%q,"fingerprint":"chrome",
	  "public_key":%q,"short_id":%q,"users":{"resi":%q,"dc":%q}}}`,
		name, name+".example.net", sni, exportPBK, exportSID, exportUUIDResi, exportUUIDDC)
}

func seedExport(t *testing.T, s *Server, name, sni string) {
	t.Helper()
	if err := os.MkdirAll(s.cfg.ExitsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.ExitsDir, name+".json"), []byte(exportJSON(name, sni)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getHops(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/nikki/hops", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET код %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	return got, rec.Body.String()
}

func putHops(t *testing.T, s *Server, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/nikki/hops", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func exitsProvider(t *testing.T, s *Server) []string {
	t.Helper()
	b, err := os.ReadFile(s.cfg.ExitsProviderPath)
	if err != nil {
		t.Fatalf("exits.yaml: %v", err)
	}
	var f struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("exits.yaml не JSON: %v", err)
	}
	var names []string
	for _, p := range f.Proxies {
		names = append(names, p["name"].(string))
	}
	return names
}

func assertNoSecrets(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{exportUUIDResi, exportUUIDDC, exportPBK, exportSID} {
		if strings.Contains(body, s) {
			t.Errorf("в ответе секрет экспорта %q", s)
		}
	}
}

func TestHopsGetFresh(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())

	got, body := getHops(t, s)
	if got["mode"] != "off" {
		t.Errorf("режим %v, ожидался off — цепочек по умолчанию нет", got["mode"])
	}
	if a := strList(t, got["available"]); len(a) != 3 {
		t.Errorf("узлов для выбора %v", a)
	}
	if p := strList(t, got["pool"]); len(p) != 0 {
		t.Errorf("пул %v, ожидалось пусто", p)
	}
	if got["fingerprint"] == "" || got["max_pool"] != float64(mixin.MaxHopPool) {
		t.Errorf("fingerprint=%v max_pool=%v", got["fingerprint"], got["max_pool"])
	}
	if nodes, _ := got["exits"].([]any); len(nodes) != 0 {
		t.Errorf("своих нод %v, ожидалось пусто", nodes)
	}
	assertNoSecrets(t, body)
}

// Свои ноды видны панели — имя, страна, адрес, SNI, выходы, — но ни одного
// секрета: UUID, ключ и shortId живут только в файлах 0600.
func TestHopsGetListsExitsWithoutSecrets(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")

	got, body := getHops(t, s)
	nodes, _ := got["exits"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("своих нод %v", got["exits"])
	}
	n := nodes[0].(map[string]any)
	if n["name"] != "kz-nqz-exit-01" || n["sni"] != "krisha.kz" || n["country"] != "kz" {
		t.Errorf("нода %v", n)
	}
	if e := strList(t, n["exits"]); strings.Join(e, ",") != "dc,resi" {
		t.Errorf("выходы %v", e)
	}
	assertNoSecrets(t, body)
}

// engineHasHopGroups — движок после склейки: группы-однохопники есть.
// Без этого джоб честно падает на сверке «группы доехали», и тест не
// дошёл бы до своего предмета.
func engineHasHopGroups(t *testing.T, s *Server, hops ...string) {
	t.Helper()
	f := nikkiFake(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	members := []string{}
	for _, h := range hops {
		n := exits.HopGroupName(h)
		f.all[n] = nikki.Proxy{Name: n, Type: "Selector", Alive: true, Members: []string{h}, Now: h, Selectable: true}
		members = append(members, n)
	}
	// Повторитель ноды из seedExport — его тоже пишет миксин.
	fg := exits.FollowGroupName("kz-nqz-exit-01")
	f.all[fg] = nikki.Proxy{Name: fg, Type: "Selector", Alive: true, Members: append(members, "REJECT"), Now: "REJECT", Selectable: true}
}

// После перезапуска демон сверяет и повторители: нет — «⇢ авто» отказывала
// бы на каждом соединении, и это провал джоба.
func TestHopsPutFailsWhenEngineLacksFollower(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	engineHasHopGroups(t, s, "🇭🇳⚡Отель")
	f := nikkiFake(t, s)
	f.mu.Lock()
	delete(f.all, exits.FollowGroupName("kz-nqz-exit-01"))
	f.mu.Unlock()
	rec := putHops(t, s, `{"mode":"allow","nodes":["🇭🇳⚡Отель"]}`, mixin.HopFingerprint(mixin.HopConfig{}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Failed || !strings.Contains(msg, "kz-nqz-exit-01") {
		t.Fatalf("джоб %s: %s — ждали провал с именем ноды", state, msg)
	}
}

// GET показывает, через какой хоп сейчас идёт resi каждой ноды.
func TestHopsGetShowsFollow(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	writeMixin(t, s, mixin.Render(mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops:        mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇭🇳⚡Отель"}, Pool: []string{"🇭🇳⚡Отель"}},
		FollowNodes: []string{"kz-nqz-exit-01"}}))
	engineHasHopGroups(t, s, "🇭🇳⚡Отель")
	f := nikkiFake(t, s)
	f.mu.Lock()
	g := f.all[exits.FollowGroupName("kz-nqz-exit-01")]
	g.Now = exits.HopGroupName("🇭🇳⚡Отель")
	f.all[exits.FollowGroupName("kz-nqz-exit-01")] = g
	f.mu.Unlock()
	got, _ := getHops(t, s)
	fw, _ := got["follow"].(map[string]any)
	if fw["kz-nqz-exit-01"] != "🇭🇳⚡Отель" {
		t.Fatalf("follow = %v", got["follow"])
	}
}

func TestHopsPutNeedsIfMatch(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	rec := putHops(t, s, `{"mode":"allow","nodes":["🇭🇳⚡Отель"]}`, "")
	if rec.Code != http.StatusConflict || errCodeOf(t, rec) != "stale_hops" {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
}

// Главный путь: два хопа — в миксине группы и SNI своих нод, в exits.yaml
// прямые узлы и цепочки; наборы целы; движок перезапущен.
func TestHopsPutAllowWritesMixinAndProvider(t *testing.T) {
	s, f := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	before := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Sets: []mixin.Set{{Name: "openai", Action: mixin.ActionExit, Group: "EXIT-RESI"}}}
	writeMixin(t, s, mixin.Render(before))
	engineHasHopGroups(t, s, "🇦🇬💳Альфа", "🇭🇳⚡Отель")

	rec := putHops(t, s, `{"mode":"allow","nodes":["🇦🇬💳Альфа","🇭🇳⚡Отель"]}`, mixin.HopFingerprint(before.Hops))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	raw, _ := mixinBytes(t, s)
	got, _, err := mixin.Parse(raw)
	if err != nil {
		t.Fatalf("разбор: %v\n%s", err, raw)
	}
	// Пул — в порядке подписки, а не присланного списка: от него зависит
	// порядок цепочек, и он не должен зависеть от того, как кликали.
	if p := strings.Join(got.Hops.Pool, "|"); p != "🇭🇳⚡Отель|🇦🇬💳Альфа" {
		t.Errorf("пул %q", p)
	}
	if strings.Join(got.SkipSNI, ",") != "krisha.kz" {
		t.Errorf("SkipSNI %v", got.SkipSNI)
	}
	if len(got.Sets) != 1 {
		t.Errorf("наборы потерялись: %+v", got.Sets)
	}
	// Повторитель: у resi одна цепочка «⇢ авто», хоп ей выбирает демон.
	if strings.Join(got.FollowNodes, ",") != "kz-nqz-exit-01" {
		t.Errorf("FollowNodes %v", got.FollowNodes)
	}
	names := exitsProvider(t, s)
	want := []string{"kz-nqz-exit-01-dc", "kz-nqz-exit-01-dc" + exits.ChainSep + "🇭🇳⚡Отель",
		"kz-nqz-exit-01-dc" + exits.ChainSep + "🇦🇬💳Альфа", "kz-nqz-exit-01-resi",
		"kz-nqz-exit-01-resi" + exits.ChainSep + exits.AutoHop}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("exits.yaml:\n%s", strings.Join(names, "\n"))
	}
	if len(f.CallsContaining("apply-mode nikki")) != 1 {
		t.Errorf("движок не перезапущен: %v", f.Calls)
	}
}

func TestHopsPutProviderTakesBalancer(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	engineHasHopGroups(t, s, "🇭🇳⚡Отель", "🇨🇻⚡Чарли 2")
	rec := putHops(t, s, `{"mode":"provider"}`, mixin.HopFingerprint(mixin.HopConfig{}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	raw, _ := mixinBytes(t, s)
	got, _, _ := mixin.Parse(raw)
	if p := strings.Join(got.Hops.Pool, "|"); p != "🇭🇳⚡Отель|🇨🇻⚡Чарли 2" {
		t.Errorf("пул %q, ждали состав балансировщика", p)
	}
}

func TestHopsPutRejects(t *testing.T) {
	many := []happ.Entry{}
	for i := 0; i < mixin.MaxHopPool+2; i++ {
		many = append(many, happ.Entry{Name: fmt.Sprintf("узел-%02d", i), Kind: happ.KindNode, Type: "vless"})
	}
	for name, tc := range map[string]struct {
		entries []happ.Entry
		body    string
		code    string
	}{
		"неизвестный режим":  {threeNodes(), `{"mode":"всё"}`, "bad_request"},
		"неизвестный узел":   {threeNodes(), `{"mode":"allow","nodes":["нет такого"]}`, "unknown_node"},
		"пул больше потолка": {many, `{"mode":"deny","nodes":[]}`, "bad_hops"},
		"пустой пул":         {threeNodes(), `{"mode":"allow","nodes":[]}`, "bad_hops"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newServer(t)
			seedManifest(t, s, tc.entries)
			rec := putHops(t, s, tc.body, mixin.HopFingerprint(mixin.HopConfig{}))
			if rec.Code != http.StatusBadRequest || errCodeOf(t, rec) != tc.code {
				t.Fatalf("код %d (%s), ждали 400 %s", rec.Code, rec.Body.String(), tc.code)
			}
			if _, err := os.Stat(s.cfg.ExitsProviderPath); err == nil {
				t.Error("отказ, но exits.yaml записан")
			}
		})
	}
}

// Выключение: секция уходит из миксина, цепочки — из exits.yaml, прямые
// узлы остаются.
func TestHopsPutOffClears(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	cur := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops:    mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇭🇳⚡Отель"}, Pool: []string{"🇭🇳⚡Отель"}},
		SkipSNI: []string{"krisha.kz"}}
	writeMixin(t, s, mixin.Render(cur))
	engineHasHopGroups(t, s)

	rec := putHops(t, s, `{"mode":"off"}`, mixin.HopFingerprint(cur.Hops))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	raw, _ := mixinBytes(t, s)
	got, _, _ := mixin.Parse(raw)
	if !got.Hops.IsDefault() {
		t.Errorf("пул остался: %+v", got.Hops)
	}
	if strings.Join(got.SkipSNI, ",") != "krisha.kz" {
		t.Errorf("SNI своих нод нужны и без цепочек (прямой вход из LAN): %v", got.SkipSNI)
	}
	if names := exitsProvider(t, s); strings.Join(names, ",") != "kz-nqz-exit-01-dc,kz-nqz-exit-01-resi,kz-nqz-exit-01-resi ⇢ авто" {
		t.Errorf("exits.yaml: %v", names)
	}
}

// Битый экспорт — отказ до записи: иначе нода молча выпала бы из
// exits.yaml вместе со своим выходом.
func TestHopsBrokenExportRefusesAndShows(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	if err := os.MkdirAll(s.cfg.ExitsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.ExitsDir, "kz-nqz-exit-01.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := getHops(t, s)
	if e, _ := got["exits_error"].(string); !strings.Contains(e, "kz-nqz-exit-01.json") {
		t.Errorf("exits_error = %v", got["exits_error"])
	}
	rec := putHops(t, s, `{"mode":"allow","nodes":["🇭🇳⚡Отель"]}`, mixin.HopFingerprint(mixin.HopConfig{}))
	if rec.Code != http.StatusConflict || errCodeOf(t, rec) != "exits_broken" {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
}

// Перечитка нод после exits-push.sh: новый SNI — запись миксина и
// перезапуск; тот же набор SNI — только файл провайдера и его перечитка.
func TestExitsReload(t *testing.T) {
	s, f := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	engineHasHopGroups(t, s)

	rec := do(t, s, http.MethodPost, "/api/exits/reload", true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	raw, _ := mixinBytes(t, s)
	got, _, _ := mixin.Parse(raw)
	if strings.Join(got.SkipSNI, ",") != "krisha.kz" {
		t.Errorf("SkipSNI %v", got.SkipSNI)
	}
	if names := exitsProvider(t, s); len(names) != 3 {
		t.Errorf("exits.yaml: %v", names)
	}
	if strings.Join(got.FollowNodes, ",") != "kz-nqz-exit-01" {
		t.Errorf("перечитка не записала повторитель: %v", got.FollowNodes)
	}
	restarts := len(f.CallsContaining("apply-mode nikki"))
	if restarts != 1 {
		t.Errorf("перезапусков %d, ждали 1 (SNI изменились)", restarts)
	}

	// Второй раз то же самое — без перезапуска, с перечиткой провайдера.
	rec = do(t, s, http.MethodPost, "/api/exits/reload", true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	if n := len(f.CallsContaining("apply-mode nikki")); n != restarts {
		t.Errorf("лишний перезапуск движка: %d", n)
	}
	if fn, ok := s.nikki.(*fakeNikkiClient); ok {
		fn.mu.Lock()
		reloaded := strings.Join(fn.reloaded, ",")
		fn.mu.Unlock()
		if !strings.Contains(reloaded, exitsProviderName) {
			t.Errorf("провайдер exits не перечитан: %q", reloaded)
		}
	}
}

// Режим «все, кроме отмеченных» следует за подпиской: новый узел — новый
// хоп и новые цепочки.
func TestHopsResyncAfterSubscriptionUpdate(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	cur := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops: mixin.HopConfig{Mode: mixin.AutoDeny, Nodes: []string{"🇦🇬💳Альфа"},
			Pool: []string{"🇭🇳⚡Отель", "🇨🇻⚡Чарли 2"}},
		SkipSNI: []string{"krisha.kz"}}
	writeMixin(t, s, mixin.Render(cur))

	seedManifest(t, s, append(threeNodes(), happ.Entry{Name: "🇩🇪⚡Германия", Kind: happ.KindNode, Type: "vless"}))
	s.resyncHops(context.Background())

	raw, _ := mixinBytes(t, s)
	got, _, _ := mixin.Parse(raw)
	if p := strings.Join(got.Hops.Pool, "|"); p != "🇭🇳⚡Отель|🇨🇻⚡Чарли 2|🇩🇪⚡Германия" {
		t.Errorf("пул после обновления подписки %q", p)
	}
	if names := exitsProvider(t, s); !strings.Contains(strings.Join(names, "|"), "🇩🇪⚡Германия") {
		t.Errorf("новый хоп не попал в цепочки: %v", names)
	}
}

// Пул хопов и SNI — секции ТОГО ЖЕ файла, что наборы. «Применить» на
// вкладке наборов обязано их перенести: иначе цепочки в exits.yaml
// остались бы ссылаться на исчезнувшие группы, а прямой вход из LAN снова
// ломался бы sniffer'ом — и связи с нажатой кнопкой никто бы не увидел.
func TestRulesetsApplyKeepsHopsAndSNI(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	before := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Sets:        []mixin.Set{{Name: "youtube", Action: mixin.ActionTunnel}},
		Hops:        mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇭🇳⚡Отель"}, Pool: []string{"🇭🇳⚡Отель"}},
		SkipSNI:     []string{"krisha.kz"},
		FollowNodes: []string{"kz-nqz-exit-01"}}
	writeMixin(t, s, mixin.Render(before))
	nikkiFake(t, s).ruleProviders = loadedProviders(mixin.Set{Name: "openai", Action: mixin.ActionTunnel})

	rec := putRulesets(t, s, `{"policy":"direct","sets":[{"name":"openai","action":"tunnel"}]}`, mixin.Fingerprint(before))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	raw, _ := mixinBytes(t, s)
	got, _, err := mixin.Parse(raw)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if strings.Join(got.Hops.Pool, "|") != "🇭🇳⚡Отель" || strings.Join(got.SkipSNI, ",") != "krisha.kz" ||
		strings.Join(got.FollowNodes, ",") != "kz-nqz-exit-01" {
		t.Errorf("после применения наборов: пул %v, SNI %v", got.Hops.Pool, got.SkipSNI)
	}
	if len(got.Sets) != 1 || got.Sets[0].Name != "openai" {
		t.Errorf("наборы не применились: %+v", got.Sets)
	}
}

// «Правила из профиля» при заданном пуле хопов не выключают склейку:
// группы-однохопники едут только в миксине.
func TestHopsKeepMixinFlagOnWithProfilePolicy(t *testing.T) {
	s, f := newServer(t)
	seedManifest(t, s, threeNodes())
	f.UCIValues["nikki.mixin."+mixinFlagOpt] = "0"
	engineHasHopGroups(t, s, "🇭🇳⚡Отель")
	rec := putHops(t, s, `{"mode":"allow","nodes":["🇭🇳⚡Отель"]}`, mixin.HopFingerprint(mixin.HopConfig{}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	if callIndex(f.Calls, "set nikki.mixin."+mixinFlagOpt+"=1") < 0 {
		t.Errorf("склейка не включена при заданном пуле хопов — группы до движка не доедут: %v", f.Calls)
	}
}

// Защита от гонок видит и третью секцию: правка пула хопов между чтением
// и записью наборов — повод отказать, а не затереть.
func TestMixinStateCoversHops(t *testing.T) {
	a := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect}
	b := a
	b.Hops = mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"x"}, Pool: []string{"x"}}
	c := a
	c.SkipSNI = []string{"krisha.kz"}
	d := a
	d.FollowNodes = []string{"kz-nqz-exit-01"}
	if mixinState(a, nil) == mixinState(b, nil) || mixinState(a, nil) == mixinState(c, nil) || mixinState(a, nil) == mixinState(d, nil) {
		t.Fatal("mixinState не видит пул хопов или SNI")
	}
}

// Движок перезапустился, а групп-однохопников у него нет (склейка не
// доехала): каждая цепочка отказывает на каждом соединении. Это провал
// джоба, а не «готово».
func TestHopsPutFailsWhenEngineLacksGroups(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	rec := putHops(t, s, `{"mode":"allow","nodes":["🇭🇳⚡Отель"]}`, mixin.HopFingerprint(mixin.HopConfig{}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	state, msg := jobOutcome(t, s)
	if state != job.Failed || !strings.Contains(msg, "🇭🇳⚡Отель") {
		t.Fatalf("джоб %s: %s — ждали провал с именем хопа", state, msg)
	}
}
