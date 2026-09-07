package httpapi

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"netmoded/internal/executor"
	"netmoded/internal/geosite"
	"netmoded/internal/nikki"
	"netmoded/internal/rulesets"
)

// standCommit — sha корневого дерева, которым отвечает подставной GitHub.
// Значение приметное: оно уезжает в тело ответа и в ключ кэша.
const standCommit = "c0ffee00d2f74c1e8ab35f60c7d9e2a4b8f01c37"

// newCatalogStand — GitHub понарошку: четыре дерева ветки meta и ничего
// больше.
//
// Не копия фикстур internal/geosite/testdata: разбор деревьев проверен в
// своём пакете, и повторять его здесь значило бы завести второй экземпляр
// того же знания. Обработчику нужен ГОТОВЫЙ каталог — из чего он собран,
// его не касается.
//
// status ≠ 0 — отвечать этим кодом на всё: так изображается недоступный
// GitHub, не трогая тела деревьев.
func newCatalogStand(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	tree := func(w http.ResponseWriter, sha string, entries ...string) {
		w.Header().Set("ETag", `W/"catalog-stand"`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[%s]}`, sha, strings.Join(entries, ","))
	}
	blob := func(name string) string {
		return fmt.Sprintf(`{"path":%q,"type":"blob","url":""}`, name)
	}
	sub := func(name string) string {
		// Адрес поддерева GitHub кладёт в тело ответа целиком, и клиент
		// обязан ходить именно по нему, а не собирать путь сам.
		return fmt.Sprintf(`{"path":%q,"type":"tree","url":%q}`, name, srv.URL+"/trees/"+name)
	}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		switch r.URL.Path {
		case "/repos/MetaCubeX/meta-rules-dat/git/trees/meta":
			tree(w, standCommit, sub("geo"))
		case "/trees/geo":
			tree(w, "geo-sha", sub("geosite"), sub("geoip"))
		case "/trees/geosite":
			tree(w, "geosite-sha", blob("openai.mrs"), blob("telegram.mrs"), blob("youtube.mrs"))
		case "/trees/geoip":
			// cn лежит только здесь: без доменов имя в каталог не попадает.
			tree(w, "geoip-sha", blob("cn.mrs"), blob("telegram.mrs"))
		default:
			http.NotFound(w, r)
		}
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// writeMixin кладёт файл по тому пути, который читает демон.
func writeMixin(t *testing.T, s *Server, b []byte) {
	t.Helper()
	if err := os.WriteFile(s.cfg.MixinPath, b, 0o600); err != nil {
		t.Fatalf("запись mixin.yaml: %v", err)
	}
}

// rulesetsGet — успешный GET /api/nikki/rulesets, разобранный в карту.
func rulesetsGet(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, "GET", "/api/nikki/rulesets", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/nikki/rulesets → %d, тело %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не разбирается: %v", err)
	}
	return body
}

// setByName достаёт один набор из ответа.
func setByName(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	sets, ok := body["sets"].([]any)
	if !ok {
		t.Fatalf("sets не массив: %#v", body["sets"])
	}
	for _, raw := range sets {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("набор не объект: %#v", raw)
		}
		if item["name"] == name {
			return item
		}
	}
	t.Fatalf("в ответе нет набора %q: %#v", name, sets)
	return nil
}

// onlyMixin — файл с политикой «только эти» и тремя наборами.
//
// Собирается Render-ом, а не литералом: тело файла порождается из состояния,
// и литерал в тесте разошёлся бы с ним от первой же правки формата — тест
// начал бы проверять форму, которой демон уже не пишет.
func onlyMixin() []byte {
	return rulesets.Render(rulesets.Config{
		Policy:   rulesets.PolicyOnly,
		Download: rulesets.DownloadDirect,
		Sets: []rulesets.Set{
			{Name: "youtube"},
			{Name: "telegram", IP: true},
			{Name: "openai"},
		},
	})
}

// TestRulesetsFreshInstallIsProfile — свежая установка: файла нет вовсе.
//
// Это НОРМАЛЬНОЕ состояние, а не отказ: nikki ставится со своим профилем, и
// пока владелец наборов не выбирал, правила целиком профильные. Ответить
// здесь 404 или 500 значило бы показать панели поломку там, где её нет.
func TestRulesetsFreshInstallIsProfile(t *testing.T) {
	s, _ := newServer(t)

	body := rulesetsGet(t, s)

	if body["policy"] != string(rulesets.PolicyProfile) {
		t.Errorf("policy %v, ожидалась %q", body["policy"], rulesets.PolicyProfile)
	}
	if body["download"] != string(rulesets.DownloadDirect) {
		t.Errorf("download %v, ожидалось %q", body["download"], rulesets.DownloadDirect)
	}
	if body["foreign"] != false {
		t.Errorf("foreign %v: отсутствующий файл нашим быть не перестаёт", body["foreign"])
	}
	if body["tunnel_group"] != rulesets.TunnelGroup {
		t.Errorf("tunnel_group %v, ожидалась %q", body["tunnel_group"], rulesets.TunnelGroup)
	}
	// Отпечаток есть и на пустом выборе: без него первый же PUT панели
	// нечем сопроводить в If-Match, и запись отбилась бы навсегда.
	fp, _ := body["fingerprint"].(string)
	if !strings.HasPrefix(fp, "sha256:") {
		t.Errorf("fingerprint %q — не отпечаток", fp)
	}
	// Пустой массив, а не null: панель перебирает поле без проверки на nil.
	sets, ok := body["sets"].([]any)
	if !ok || len(sets) != 0 {
		t.Errorf("sets %#v, ожидался пустой массив", body["sets"])
	}
}

// TestRulesetsReportsEngineState — сверка выбора с движком.
//
// Файл говорит, что владелец выбрал; движок — что реально скачалось. Это
// разные вопросы, и ответ обязан различать их по каждому набору: набор
// «выбран, но не скачался» — самая частая жалоба («включил, не работает»),
// и увидеть её можно только здесь.
//
// telegram проверяет главное правило: набор с подсетями загружен, только
// когда загружены ОБА его провайдера. Домены скачались, подсети нет — это
// половина набора, и показывать её готовой значит врать.
func TestRulesetsReportsEngineState(t *testing.T) {
	s, _ := newServer(t)
	writeMixin(t, s, onlyMixin())

	at := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	nikkiFake(t, s).ruleProviders = map[string]nikki.RuleProvider{
		"nm-geosite-youtube":  {Name: "nm-geosite-youtube", RuleCount: 1284, UpdatedAt: at},
		"nm-geosite-telegram": {Name: "nm-geosite-telegram", RuleCount: 512, UpdatedAt: at},
		// Подсети telegram движок ещё не скачал: время нулевое.
		"nm-geoip-telegram": {Name: "nm-geoip-telegram", RuleCount: 34},
		// openai не скачан вовсе — ни правил, ни времени.
		"nm-geosite-openai": {Name: "nm-geosite-openai"},
	}

	body := rulesetsGet(t, s)

	if body["live"] != true {
		t.Fatalf("live %v: движок ответил, состояние сверено", body["live"])
	}
	if body["policy"] != string(rulesets.PolicyOnly) {
		t.Errorf("policy %v", body["policy"])
	}

	yt := setByName(t, body, "youtube")
	if yt["loaded"] != true || yt["rules"] != float64(1284) {
		t.Errorf("youtube: loaded %v, rules %v — ожидались true и 1284", yt["loaded"], yt["rules"])
	}
	if yt["updated_at"] != "2026-09-06T10:00:00Z" {
		t.Errorf("youtube updated_at %v", yt["updated_at"])
	}
	if yt["ip"] != false {
		t.Errorf("youtube ip %v: подсетей у него нет", yt["ip"])
	}

	tg := setByName(t, body, "telegram")
	if tg["ip"] != true {
		t.Errorf("telegram ip %v: признак подсетей записан в файле", tg["ip"])
	}
	if tg["loaded"] != false {
		t.Error("telegram числится загруженным, хотя скачались только домены — это половина набора")
	}
	// Правила считаются по тому, что движок показывает, и у половины
	// набора они есть: число не критерий загруженности, оно справка.
	if tg["rules"] != float64(546) {
		t.Errorf("telegram rules %v, ожидалось 546 (512 доменов и 34 подсети)", tg["rules"])
	}
	if tg["updated_at"] != nil {
		t.Errorf("telegram updated_at %v, ожидался null у незагруженного набора", tg["updated_at"])
	}

	oa := setByName(t, body, "openai")
	if oa["loaded"] != false || oa["rules"] != float64(0) || oa["updated_at"] != nil {
		t.Errorf("openai: loaded %v, rules %v, updated_at %v — ожидались false, 0 и null",
			oa["loaded"], oa["rules"], oa["updated_at"])
	}
}

// TestRulesetsWithoutEngineKeepsSets — движок лежит, выбор всё равно виден.
//
// live:false — это ОТВЕТ, а не отказ: выбор владельца лежит в нашем файле и
// читается без движка. Отдать здесь 503 значило бы, что при упавшем mihomo
// панель не показывает даже того, что сама записала, — и владелец решил бы,
// что потерял настройку.
//
// loaded/rules/updated_at при этом строго null, а не false и не ноль:
// «набор не загружен» и «неизвестно, загружен ли» — разные утверждения, и
// показывать второе первым значит соврать ровно там, где владелец ищет
// причину неработающего обхода.
func TestRulesetsWithoutEngineKeepsSets(t *testing.T) {
	s, _ := newServer(t)
	writeMixin(t, s, onlyMixin())
	nikkiFake(t, s).err = nikki.ErrUnavailable

	body := rulesetsGet(t, s)

	if body["live"] != false {
		t.Fatalf("live %v: Clash API не отвечает", body["live"])
	}
	sets, _ := body["sets"].([]any)
	if len(sets) != 3 {
		t.Fatalf("наборов %d, ожидалось 3: выбор читается из файла, а не из движка", len(sets))
	}
	for _, name := range []string{"youtube", "telegram", "openai"} {
		item := setByName(t, body, name)
		for _, field := range []string{"loaded", "rules", "updated_at"} {
			v, present := item[field]
			if !present {
				t.Errorf("%s: ключа %s нет вовсе — панель не отличит его от отсутствия набора", name, field)
			}
			if v != nil {
				t.Errorf("%s: %s = %v, ожидался null — движок не отвечал, сверять было не с чем", name, field, v)
			}
		}
	}
}

// TestRulesetsForeignFileIsNotOurs — чужой mixin.yaml не переписывается и не
// выдаётся за наш.
//
// Владелец мог написать его сам или получить с чужой инструкцией. Показать
// его содержимое как «выбор наборов» значило бы предложить кнопку
// «Применить», которая молча затрёт чужую работу.
func TestRulesetsForeignFileIsNotOurs(t *testing.T) {
	s, _ := newServer(t)
	writeMixin(t, s, []byte("dns:\n  enable: true\n"))

	body := rulesetsGet(t, s)

	if body["foreign"] != true {
		t.Fatalf("foreign %v: в файле чужое содержимое без нашей шапки", body["foreign"])
	}
	if body["policy"] != string(rulesets.PolicyProfile) {
		t.Errorf("policy %v: чужой файл наборов не выбирает", body["policy"])
	}
	if sets, _ := body["sets"].([]any); len(sets) != 0 {
		t.Errorf("sets %#v, ожидался пустой массив", sets)
	}
}

// TestRulesetsCorruptFileIsAnError — файл наш, а содержимому верить нельзя.
//
// Отпечаток врал бы, а PUT с ним записал бы поверх непонятно чего. 500, а не
// 200 с признаком: показать выбор, которого в правилах нет, хуже, чем
// попросить применить заново.
func TestRulesetsCorruptFileIsAnError(t *testing.T) {
	s, _ := newServer(t)
	// Шапка наша, состояние обещает три набора, правил в теле ни одного.
	writeMixin(t, s, []byte(
		"# netmoded: файл пишет панель.\n"+
			"# Не правьте руками.\n"+
			"# netmoded-rulesets: policy=only download=direct sets=youtube,telegram+ip,openai\n"))

	rec := do(t, s, "GET", "/api/nikki/rulesets", true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код %d, ожидался 500; тело %s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не разбирается: %v", err)
	}
	if e.Code != "mixin_corrupt" {
		t.Errorf("код ошибки %q, ожидался mixin_corrupt", e.Code)
	}
	// Путь в тексте: чинить это идут по ssh, и первый вопрос — какой файл.
	if !strings.Contains(e.Error, s.cfg.MixinPath) {
		t.Errorf("в тексте %q нет пути к файлу", e.Error)
	}
}

// TestRulesetsUnreadableFileIsNotCorrupt — файл не прочитан вовсе.
//
// Код другой, чем у испорченного файла, и это не педантизм: про содержимое
// нечитаемого файла неизвестно НИЧЕГО, и совет «примените заново» тут не
// поможет — чинят это правами или каталогом. Один код на оба случая увёл бы
// владельца перезаписывать файл, который и записать-то нельзя.
func TestRulesetsUnreadableFileIsNotCorrupt(t *testing.T) {
	s, _ := newServer(t)
	// Каталог вместо файла: os.ReadFile отказывает, а прав root для этого
	// не нужно — тест обязан идти и не от суперпользователя.
	if err := os.Mkdir(s.cfg.MixinPath, 0o755); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	rec := do(t, s, "GET", "/api/nikki/rulesets", true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код %d, ожидался 500; тело %s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не разбирается: %v", err)
	}
	if e.Code != "read_failed" {
		t.Errorf("код ошибки %q, ожидался read_failed", e.Code)
	}
}

// TestRulesetProviderNamesForDevStand — имена провайдеров, которые порождает
// нынешний выбор. Ими дев-стенд наполняет подделку Clash API: иначе на
// стенде каждый набор вечно «не загрузился».
func TestRulesetProviderNamesForDevStand(t *testing.T) {
	s, _ := newServer(t)
	if got := s.RulesetProviderNames(context.Background()); len(got) != 0 {
		t.Errorf("на свежей установке имена %v, ожидалась пустота", got)
	}

	writeMixin(t, s, onlyMixin())
	want := "nm-geosite-youtube,nm-geosite-telegram,nm-geoip-telegram,nm-geosite-openai"
	if got := strings.Join(s.RulesetProviderNames(context.Background()), ","); got != want {
		t.Errorf("имена %q, ожидались %q", got, want)
	}

	writeMixin(t, s, []byte("dns:\n  enable: true\n"))
	if got := s.RulesetProviderNames(context.Background()); got != nil {
		t.Errorf("у чужого файла имена %v, ожидался nil", got)
	}
}

// --- GET /api/nikki/rulesets/catalog ---

// catalogGet — успешный запрос каталога, разобранный в карту.
func catalogGet(t *testing.T, s *Server, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := getPanel(t, s, "/api/nikki/rulesets/catalog", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET каталога → %d, тело %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не разбирается: %v", err)
	}
	return rec, body
}

// TestCatalogServesNamesAndPacks — каталог отдаётся из памяти демона.
//
// Паки фильтруются по каталогу: имя из пака могло исчезнуть из репозитория,
// и предлагать его на первом экране значило бы предлагать набор, который
// отобьётся при сохранении. Пак, от которого ничего не осталось, исчезает
// целиком — пустая рубрика на экране хуже, чем её отсутствие.
func TestCatalogServesNamesAndPacks(t *testing.T) {
	s, _ := newServer(t)

	rec, body := catalogGet(t, s, nil)

	if body["commit"] != standCommit {
		t.Errorf("commit %v, ожидался %q", body["commit"], standCommit)
	}
	if body["stale"] != false {
		t.Errorf("stale %v: каталог только что загружен", body["stale"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(body["fetched_at"])); err != nil {
		t.Errorf("fetched_at %v — не RFC 3339: %v", body["fetched_at"], err)
	}
	if got := joinAny(body["names"]); got != "openai,telegram,youtube" {
		t.Errorf("names %q, ожидался отсортированный список только из *.mrs", got)
	}
	// cn есть только в дереве geoip: без доменов предлагать его нечего.
	if got := joinAny(body["ip"]); got != "telegram" {
		t.Errorf("ip %q, ожидался telegram", got)
	}

	packs, ok := body["packs"].([]any)
	if !ok {
		t.Fatalf("packs %#v", body["packs"])
	}
	var ids []string
	for _, raw := range packs {
		p, _ := raw.(map[string]any)
		ids = append(ids, fmt.Sprint(p["id"]))
		if len(p["sets"].([]any)) == 0 {
			t.Errorf("пак %v остался пустым — его следовало убрать целиком", p["id"])
		}
	}
	// social и work состоят из имён, которых в этом каталоге нет.
	if strings.Join(ids, ",") != "messengers,ai,video" {
		t.Errorf("паки %v, ожидались messengers, ai, video в порядке geosite.Packs", ids)
	}
	if got := joinAny(packs[2].(map[string]any)["sets"]); got != "youtube" {
		t.Errorf("в паке video осталось %q, ожидался только youtube", got)
	}

	if rec.Header().Get("ETag") == "" {
		t.Error("ответ без ETag: условный запрос невозможен, а список весит 60 КБ")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=3600") {
		t.Errorf("Cache-Control %q, ожидался с max-age=3600", cc)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("ответ без Content-Length")
	}
}

// TestCatalogIsCompressedAndConditional — 60 КБ имён едут сжатыми и не едут
// вовсе, когда у браузера уже есть та же версия.
//
// Ради этого каталог и отдаётся мимо writeJSON: тот ставит no-store и ни
// ETag, ни сжатия не умеет.
func TestCatalogIsCompressedAndConditional(t *testing.T) {
	s, _ := newServer(t)

	// Не через catalogGet: тело сжато, и разбирать его как JSON нечем —
	// в этом и смысл проверки.
	rec := getPanel(t, s, "/api/nikki/rulesets/catalog", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET каталога → %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding %q, ожидался gzip", rec.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("без Vary кэш-посредник отдаст сжатое тело тому, кто сжатия не просил")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("тело не распаковывается: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("чтение распакованного: %v", err)
	}
	if !strings.Contains(string(raw), standCommit) {
		t.Error("в распакованном теле нет коммита каталога")
	}

	etag := rec.Header().Get("ETag")
	again := getPanel(t, s, "/api/nikki/rulesets/catalog", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("повтор с If-None-Match → %d, ожидался 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Errorf("304 приехал с телом в %d байт", again.Body.Len())
	}
}

// TestCatalogUnavailableWhenGitHubIsDown — пустой кэш и лежащий GitHub.
//
// 503 с отдельным кодом, а не 500: чинится это ожиданием или починкой
// аплинка, а не нами. Панель на этот код показывает поиск по именам
// недоступным, но НЕ прячет уже применённые наборы — они читаются из файла.
func TestCatalogUnavailableWhenGitHubIsDown(t *testing.T) {
	s, _ := newServer(t)
	// Подменяется поле, а не пересобирается сервер: проводку
	// Config.CatalogBaseURL → s.catalog проверяет отдельный тест ниже, а
	// здесь нужен ровно лежащий GitHub при пустом кэше.
	s.catalog = &geosite.Client{BaseURL: newCatalogStand(t, http.StatusInternalServerError).URL, Logf: s.logf}

	rec := do(t, s, "GET", "/api/nikki/rulesets/catalog", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503; тело %s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не разбирается: %v", err)
	}
	if e.Code != "catalog_unavailable" {
		t.Errorf("код ошибки %q, ожидался catalog_unavailable", e.Code)
	}
	// Причина в тексте: 403 при исчерпанном лимите и 500 у GitHub чинятся
	// по-разному, и владелец пойдёт разбираться с этой строкой.
	if !strings.Contains(e.Error, "500") {
		t.Errorf("в тексте %q нет причины отказа", e.Error)
	}
}

// TestRulesetsRequireToken — оба пути под токеном, как и всё остальное.
func TestRulesetsRequireToken(t *testing.T) {
	s, _ := newServer(t)
	for _, p := range []string{"/api/nikki/rulesets", "/api/nikki/rulesets/catalog"} {
		if rec := do(t, s, "GET", p, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s без токена → %d, ожидался 401", p, rec.Code)
		}
	}
}

// TestNewServerWiresCatalogAndMixinPath — боевые умолчания.
//
// Проверка идёт через NewServer, без подмен: ровно так уже терялся клиент
// Nikki у читателя статуса. Пустой BaseURL здесь означает боевой GitHub, а
// пустой MixinPath — файл не там, где его читает nikki, то есть выбор
// владельца, который движок никогда не увидит.
func TestNewServerWiresCatalogAndMixinPath(t *testing.T) {
	s, _ := newServer(t)
	if s.catalog == nil {
		t.Fatal("клиент каталога не собран: /rulesets/catalog отвечал бы паникой")
	}
	if s.catalog.BaseURL != s.cfg.CatalogBaseURL {
		t.Errorf("BaseURL каталога %q, а в конфигурации %q", s.catalog.BaseURL, s.cfg.CatalogBaseURL)
	}

	def, err := NewServer(Config{
		Listen: "192.168.9.1", Port: 8088, Token: testToken,
		LogPath: s.cfg.LogPath,
	}, executor.NewFake())
	if err != nil {
		t.Fatalf("NewServer с боевыми умолчаниями: %v", err)
	}
	if def.cfg.MixinPath != mixinPath {
		t.Errorf("MixinPath по умолчанию %q, ожидался %q", def.cfg.MixinPath, mixinPath)
	}
	if def.catalog.BaseURL != "" {
		t.Errorf("BaseURL каталога %q, ожидался пустой — это боевой api.github.com", def.catalog.BaseURL)
	}
}

// joinAny склеивает массив строк из разобранного JSON.
func joinAny(v any) string {
	items, ok := v.([]any)
	if !ok {
		return fmt.Sprintf("НЕ МАССИВ: %#v", v)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprint(it))
	}
	return strings.Join(out, ",")
}
