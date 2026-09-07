package httpapi

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	"netmoded/internal/job"
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
	// Окно ожидания докачки: помощник newServer ужимает его до
	// миллисекунд, и без этой проверки боевое значение мог бы забыть
	// проставить кто угодно — джоб докладывал бы «не скачалось ни одного»
	// мгновенно, ни разу никого не дождавшись.
	if def.rulesetsWait != rulesetsWait || def.rulesetsPoll != rulesetsPoll {
		t.Errorf("ожидание наборов %s/%s, ожидались боевые %s/%s",
			def.rulesetsWait, def.rulesetsPoll, rulesetsWait, rulesetsPoll)
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

// --- PUT /api/nikki/rulesets ---

// putRulesets — применение выбора с отпечатком в If-Match.
func putRulesets(t *testing.T, s *Server, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/nikki/rulesets", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// rulesetsFP — отпечаток, который панель берёт из GET и кладёт в If-Match.
//
// Берётся именно из ответа, а не считается тестом по rulesets.Fingerprint:
// иначе проверка сверяла бы отпечаток сам с собой, а разрыв «GET отдаёт одно,
// PUT ждёт другое» остался бы невидимым — то есть панель не смогла бы
// записать ничего никогда.
func rulesetsFP(t *testing.T, s *Server) string {
	t.Helper()
	fp, _ := rulesetsGet(t, s)["fingerprint"].(string)
	if fp == "" {
		t.Fatal("GET не отдал отпечаток — сопроводить им PUT нечем")
	}
	return fp
}

// addBypass кладёт в подделку движка группу BYPASS.
//
// В newFakeNikkiClient её нет намеренно — так проверяется отказ
// group_missing. Добавить её туда «для всех» значило бы остаться без этой
// проверки, поэтому каждый тест, которому нужна работающая запись, просит
// группу сам.
func addBypass(t *testing.T, s *Server) {
	t.Helper()
	f := nikkiFake(t, s)
	f.all[rulesets.TunnelGroup] = nikki.Proxy{
		Name: rulesets.TunnelGroup, Type: "Selector", Alive: true,
		Members: []string{"DIRECT", "PROXY"}, Now: "PROXY", Selectable: true,
	}
}

// loadedProviders — движок, который скачал ВСЕ провайдеры перечисленных
// наборов: у каждого ненулевое время, иначе rulesets.Verify считает набор
// недокачанным.
func loadedProviders(sets ...rulesets.Set) map[string]nikki.RuleProvider {
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	out := map[string]nikki.RuleProvider{}
	for _, set := range sets {
		for _, name := range rulesets.ProviderNames(set) {
			out[name] = nikki.RuleProvider{Name: name, RuleCount: 100, UpdatedAt: at}
		}
	}
	return out
}

// jobOutcome ждёт конца джоба и отдаёт исход с текстом ошибки.
func jobOutcome(t *testing.T, s *Server) (job.State, string) {
	t.Helper()
	if !s.jobs.Wait(3 * time.Second) {
		t.Fatal("джоб не завершился за 3 с")
	}
	j := s.jobs.Current()
	if j == nil {
		t.Fatal("джоба нет вовсе — операция не запускалась")
	}
	msg := ""
	if j.Error != nil {
		msg = *j.Error
	}
	return j.State, msg
}

// mixinBytes — содержимое файла наборов; ok=false, если файла нет вовсе.
func mixinBytes(t *testing.T, s *Server) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(s.cfg.MixinPath)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("чтение %s: %v", s.cfg.MixinPath, err)
	}
	return b, true
}

// callCount — сколько раз вызов встретился в журнале.
//
// Рядом с готовым callIndex, а не вместо него: «ровно один раз» — отдельное
// утверждение от «был и стоял вот здесь», и второй перезапуск движка тот
// нашёл бы, а этот нет.
func callCount(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// TestRulesetsPutAppliesFileFlagAndRestart — успешный путь целиком.
//
// Три шага и их ПОРЯДОК: файл на флеш, флаг в UCI, перезапуск движка.
// Обратный порядок означал бы перезапуск под старым файлом — то есть
// «Применено» в панели над правилами, которых mihomo не видел.
//
// Признак подсетей у telegram проставляет ДЕМОН из каталога: панель шлёт
// одни имена. Пришли бы они от клиента — вкладка, открытая до обновления
// каталога, молча теряла бы половину набора.
func TestRulesetsPutAppliesFileFlagAndRestart(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	want := rulesets.Config{
		Policy: rulesets.PolicyOnly, Download: rulesets.DownloadDirect,
		Sets: []rulesets.Set{{Name: "youtube"}, {Name: "telegram", IP: true}},
	}
	nikkiFake(t, s).ruleProviders = loadedProviders(want.Sets...)

	// download не прислан: умолчание direct — часть контракта.
	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube","telegram"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	got, ok := mixinBytes(t, s)
	if !ok {
		t.Fatal("файл наборов не создан")
	}
	if string(got) != string(rulesets.Render(want)) {
		t.Errorf("файл не совпал с Render:\n--- на диске ---\n%s\n--- ожидалось ---\n%s",
			got, rulesets.Render(want))
	}

	iSet, iCommit, iApply := callIndex(f.Calls, "set nikki.mixin.mixin_file_content=1"),
		callIndex(f.Calls, "commit nikki"), callIndex(f.Calls, "apply-mode nikki")
	nSet, nCommit, nApply := callCount(f.Calls, "set nikki.mixin.mixin_file_content=1"),
		callCount(f.Calls, "commit nikki"), callCount(f.Calls, "apply-mode nikki")
	if nSet != 1 || nCommit != 1 || nApply != 1 {
		t.Fatalf("вызовы не по разу: set=%d commit=%d apply=%d\n%v", nSet, nCommit, nApply, f.Calls)
	}
	if !(iSet < iCommit && iCommit < iApply) {
		t.Errorf("порядок нарушен: set=%d commit=%d apply=%d\n%v", iSet, iCommit, iApply, f.Calls)
	}

	// Записанное обязано читаться обратно: файл — единственное хранилище
	// выбора, и разойдись запись с чтением, панель показывала бы не то,
	// что применено.
	body := rulesetsGet(t, s)
	if body["policy"] != string(rulesets.PolicyOnly) || body["download"] != string(rulesets.DownloadDirect) {
		t.Errorf("GET после записи: policy %v, download %v", body["policy"], body["download"])
	}
	if tg := setByName(t, body, "telegram"); tg["ip"] != true {
		t.Errorf("telegram ip %v: признак подсетей ставит демон по каталогу", tg["ip"])
	}
	if body["foreign"] != false {
		t.Errorf("foreign %v: файл только что написали мы сами", body["foreign"])
	}
}

// TestRulesetsPutInOtherModeSkipsRestart — режим не nikki: пишем, но не
// перезапускаем.
//
// netmode-apply nikki поднял бы движок, который владелец выключил
// намеренно. Выбор при этом сохраняется и вступит в силу при включении
// Nikki — на то он и лежит в файле, который nikki читает сам.
func TestRulesetsPutInOtherModeSkipsRestart(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	f.UCIValues["netmode.main.mode"] = "b4"

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	if _, ok := mixinBytes(t, s); !ok {
		t.Error("файл наборов не записан: выбор потерян до включения Nikki")
	}
	if callCount(f.Calls, "set nikki.mixin.mixin_file_content=1") != 1 {
		t.Errorf("флаг не переключён: %v", f.Calls)
	}
	if got := f.CallsContaining("apply-mode"); len(got) != 0 {
		t.Errorf("движок перезапущен в чужом режиме: %v", got)
	}
}

// TestRulesetsPutKeepsFlagAlreadyOn — флаг уже 1: ни set, ни commit.
//
// Запись ради того же значения стоила бы коммита пакета nikki на флеше, а
// заодно опубликовала бы всё, что в стейджинге накопил кто-то ещё.
func TestRulesetsPutKeepsFlagAlreadyOn(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	f.UCIValues["nikki.mixin.mixin_file_content"] = "1"
	nikkiFake(t, s).ruleProviders = loadedProviders(rulesets.Set{Name: "youtube"})

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	if got := f.CallsContaining("mixin_file_content"); len(got) != 0 {
		t.Errorf("флаг переписан тем же значением: %v", got)
	}
	if got := f.CallsContaining("commit nikki"); len(got) != 0 {
		t.Errorf("коммит пакета nikki без своей правки: %v", got)
	}
	if callCount(f.Calls, "apply-mode nikki") != 1 {
		t.Errorf("движок не перезапущен: %v", f.Calls)
	}
}

// TestRulesetsPutRefusesBeforeAnyWrite — все отказы наступают ДО записи.
//
// Это главное свойство обработчика: отката нет (ADR-0006), и после
// uci commit отказаться уже нечем. Поэтому каждая строка таблицы проверяет
// не только код ответа, но и то, что файл на флеше не тронут, а журнал
// исполнителя пуст.
func TestRulesetsPutRefusesBeforeAnyWrite(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, s *Server, f *executor.Fake)
		body    string
		ifMatch string // пусто → отпечаток из GET
		status  int
		code    string
		text    []string
	}{{
		name:   "неизвестная политика",
		body:   `{"policy":"турбо","sets":[]}`,
		status: http.StatusBadRequest,
		code:   "bad_request",
	}, {
		name:   "имени нет в каталоге",
		body:   `{"policy":"only","sets":["нетакогонабора"]}`,
		status: http.StatusBadRequest,
		code:   "unknown_set",
		// Имя в тексте: панель подсвечивает именно эти чипы, и «неверный
		// запрос» не сказало бы, какое из тридцати имён написано с опечаткой.
		text: []string{"нетакогонабора"},
	}, {
		name:   "profile с наборами",
		body:   `{"policy":"profile","sets":["youtube"]}`,
		status: http.StatusBadRequest,
		code:   "bad_request",
	}, {
		name: "чужой стейджинг nikki",
		setup: func(t *testing.T, s *Server, f *executor.Fake) {
			f.Staged["nikki"] = "nikki.mixin.mixin_file_content='0'\n"
		},
		body:   `{"policy":"only","sets":["youtube"]}`,
		status: http.StatusConflict,
		code:   "foreign_staged_changes",
	}, {
		name:    "отпечаток разошёлся",
		body:    `{"policy":"only","sets":["youtube"]}`,
		ifMatch: "sha256:0000000000000000",
		status:  http.StatusConflict,
		code:    "stale_rulesets",
	}, {
		name: "чужой mixin.yaml",
		setup: func(t *testing.T, s *Server, f *executor.Fake) {
			writeMixin(t, s, []byte("dns:\n  enable: true\n"))
		},
		body:   `{"policy":"only","sets":["youtube"]}`,
		status: http.StatusConflict,
		code:   "foreign_mixin",
	}, {
		name: "каталог не загружен, имя новое",
		setup: func(t *testing.T, s *Server, f *executor.Fake) {
			s.catalog = &geosite.Client{BaseURL: newCatalogStand(t, http.StatusInternalServerError).URL, Logf: s.logf}
		},
		body:   `{"policy":"only","sets":["openai"]}`,
		status: http.StatusServiceUnavailable,
		code:   "catalog_unavailable",
	}, {
		name: "в профиле нет группы BYPASS",
		setup: func(t *testing.T, s *Server, f *executor.Fake) {
			delete(nikkiFake(t, s).all, rulesets.TunnelGroup)
		},
		body:   `{"policy":"only","sets":["youtube"]}`,
		status: http.StatusServiceUnavailable,
		code:   "group_missing",
		text:   []string{rulesets.TunnelGroup},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, f := newServer(t)
			// Группа есть у всех строк: иначе отказ приезжал бы от неё, а
			// не от проверяемой причины, и таблица доказывала бы одно и
			// то же восемь раз.
			addBypass(t, s)
			if tc.setup != nil {
				tc.setup(t, s, f)
			}
			before, existed := mixinBytes(t, s)

			ifMatch := tc.ifMatch
			if ifMatch == "" {
				ifMatch = rulesetsFP(t, s)
			}
			f.Calls = nil
			rec := putRulesets(t, s, tc.body, ifMatch)

			if rec.Code != tc.status {
				t.Fatalf("код %d, ожидался %d; тело %s", rec.Code, tc.status, rec.Body.String())
			}
			var e apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("тело ошибки не разбирается: %s", rec.Body.String())
			}
			if e.Code != tc.code {
				t.Errorf("код ошибки %q, ожидался %q (текст %q)", e.Code, tc.code, e.Error)
			}
			for _, sub := range tc.text {
				if !strings.Contains(e.Error, sub) {
					t.Errorf("в тексте %q нет %q", e.Error, sub)
				}
			}

			after, exists := mixinBytes(t, s)
			if exists != existed || string(after) != string(before) {
				t.Errorf("файл наборов тронут при отказе: было %q (%v), стало %q (%v)",
					before, existed, after, exists)
			}
			if len(f.Calls) != 0 {
				t.Errorf("отказ дошёл до записи в систему: %v", f.Calls)
			}
		})
	}
}

// TestRulesetsPutAcceptsAppliedNameWithoutCatalog — применённое имя валидно и
// без каталога.
//
// Иначе повторное применение без интернета — скажем, одна лишь смена
// политики — отбивалось бы на именах, которые сам же демон и записал.
// Признак подсетей при этом берётся из файла: потеряй мы его, у набора
// молча исчезла бы половина.
func TestRulesetsPutAcceptsAppliedNameWithoutCatalog(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	applied := rulesets.Set{Name: "telegram", IP: true}
	writeMixin(t, s, rulesets.Render(rulesets.Config{
		Policy: rulesets.PolicyOnly, Download: rulesets.DownloadDirect,
		Sets: []rulesets.Set{applied},
	}))
	s.catalog = &geosite.Client{BaseURL: newCatalogStand(t, http.StatusInternalServerError).URL, Logf: s.logf}
	nikkiFake(t, s).ruleProviders = loadedProviders(applied)

	rec := putRulesets(t, s, `{"policy":"except","sets":["telegram"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	want := rulesets.Render(rulesets.Config{
		Policy: rulesets.PolicyExcept, Download: rulesets.DownloadDirect,
		Sets: []rulesets.Set{applied},
	})
	got, _ := mixinBytes(t, s)
	if string(got) != string(want) {
		t.Errorf("файл:\n%s\nожидался:\n%s", got, want)
	}
}

// TestRulesetsPutKeepsFileWhenRestartFails — движок не поднялся: файл
// остаётся, отката нет.
//
// ADR-0006: возвращать прежний файл значило бы делать вторую запись на
// флеш поверх первой, чтобы прийти к состоянию, которого владелец не
// просил. Повтор возможен, и netmode-apply при загрузке доведёт дело сам.
func TestRulesetsPutKeepsFileWhenRestartFails(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	f.ApplyExitCodes["nikki"] = 5

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	state, msg := jobOutcome(t, s)
	if state != job.Failed {
		t.Fatalf("джоб %s, ожидался failed", state)
	}
	// Текст обязан сказать, что наборы записаны: иначе владелец пойдёт
	// применять их заново вместо того, чтобы разбираться с движком.
	if !strings.Contains(msg, "записаны") {
		t.Errorf("текст %q не говорит, что файл уже записан", msg)
	}
	if _, ok := mixinBytes(t, s); !ok {
		t.Error("файл убран после неудачного перезапуска — это откат, которого нет")
	}
	if got := f.CallsContaining("revert"); len(got) != 0 {
		t.Errorf("откат после коммита: %v", got)
	}
}

// TestRulesetsPutRevertsFlagWhenCommitFails — коммит флага не прошёл.
//
// Черновик в стейджинге пакета nikki уехал бы в систему при первом чужом
// uci commit nikki — то есть в момент, который никто не выбирал. Поэтому
// своя правка снимается revert-ом, а текст называет файл: он-то записан, и
// владелец должен знать, действует он сейчас или нет.
func TestRulesetsPutRevertsFlagWhenCommitFails(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	f.Errors["commit nikki"] = errors.New("uci: I/O error")

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	state, msg := jobOutcome(t, s)
	if state != job.Failed {
		t.Fatalf("джоб %s, ожидался failed", state)
	}
	if !strings.Contains(msg, s.cfg.MixinPath) {
		t.Errorf("в тексте %q нет пути к записанному файлу", msg)
	}
	if callCount(f.Calls, "revert nikki.mixin") != 1 {
		t.Errorf("черновик флага не снят: %v", f.Calls)
	}
	if got := f.CallsContaining("apply-mode"); len(got) != 0 {
		t.Errorf("движок перезапущен после провала флага: %v", got)
	}
}

// TestRulesetsPutFailsWhenNothingLoaded — движок не скачал ни одного набора.
//
// «Применено» здесь было бы прямой ложью: правила есть, файлов правил нет,
// и обход не работает. Текст называет имена и два места, где ищут причину.
func TestRulesetsPutFailsWhenNothingLoaded(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	// ruleProviders не заданы: движок отвечает, но провайдеров у него нет.

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	state, msg := jobOutcome(t, s)
	if state != job.Failed {
		t.Fatalf("джоб %s, ожидался failed; текст %q", state, msg)
	}
	for _, sub := range []string{"youtube", "raw.githubusercontent.com", "/var/log/nikki/core.log"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("в тексте %q нет %q", msg, sub)
		}
	}
}

// TestRulesetsPutDoneWhenPartlyLoaded — скачалась часть: это done.
//
// mihomo докачает остальное сам по своему циклу, а какие наборы не
// загрузились, видно в GET по полю loaded. Отдать здесь failed значило бы
// назвать неудачей применение, которое работает.
func TestRulesetsPutDoneWhenPartlyLoaded(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	nikkiFake(t, s).ruleProviders = loadedProviders(rulesets.Set{Name: "youtube"})

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube","openai"]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	if oa := setByName(t, rulesetsGet(t, s), "openai"); oa["loaded"] != false {
		t.Errorf("openai loaded %v: движок его не скачал", oa["loaded"])
	}
}

// TestRulesetsPutProfileWritesHeaderOnly — «вернуть профилю».
//
// Файл из одной шапки, флаг 0, перезапуск. Правила снова целиком из
// профиля nikki: оставь мы в файле хоть одно правило, склейка добавила бы
// его к профильным.
func TestRulesetsPutProfileWritesHeaderOnly(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	writeMixin(t, s, onlyMixin())
	// Флаг стоял включённым — иначе выключать было бы нечего.
	f.UCIValues["nikki.mixin.mixin_file_content"] = "1"

	rec := putRulesets(t, s, `{"policy":"profile","sets":[]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	got, _ := mixinBytes(t, s)
	want := rulesets.Render(rulesets.Config{Policy: rulesets.PolicyProfile, Download: rulesets.DownloadDirect})
	if string(got) != string(want) {
		t.Errorf("файл:\n%s\nожидался из одной шапки:\n%s", got, want)
	}
	if strings.Contains(string(got), "RULE-SET") {
		t.Error("в файле остались правила: склейка добавит их к правилам профиля")
	}
	if callCount(f.Calls, "set nikki.mixin.mixin_file_content=0") != 1 {
		t.Errorf("флаг не выключен: %v", f.Calls)
	}
	if callCount(f.Calls, "apply-mode nikki") != 1 {
		t.Errorf("движок не перезапущен: %v", f.Calls)
	}
}

// TestRulesetsPutJobBusy — джоб один на демона: второй отбивается, а не
// встаёт в очередь (SPEC §6).
func TestRulesetsPutJobBusy(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	fp := rulesetsFP(t, s)

	release := make(chan struct{})
	started := make(chan struct{})
	if _, err := s.jobs.Start("mode", "nikki", "тест", 1, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, fp)
	close(release)

	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409; тело %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "job_busy" {
		t.Errorf("код ошибки %q, ожидался job_busy", got)
	}
	if _, ok := mixinBytes(t, s); ok {
		t.Error("отбитый запрос успел записать файл")
	}
	if len(f.Calls) != 0 {
		t.Errorf("отбитый запрос дошёл до системы: %v", f.Calls)
	}
}

// corruptMixin — наш файл, состоянию которого верить нельзя: шапка на
// месте, состояние обещает два набора, правил в теле ни одного.
//
// Второе имя намеренно отсутствует в каталоге стенда: по нему видно, берёт
// ли обработчик «применённое» из испорченного состояния. Брать его оттуда
// нельзя — состоянию мы как раз и не верим.
func corruptMixin() []byte {
	return []byte(
		"# netmoded: файл пишет панель.\n" +
			"# Не правьте руками.\n" +
			"# netmoded-rulesets: policy=only download=direct sets=youtube,вымышленный\n")
}

// TestRulesetsPutRewritesCorruptFile — испорченный файл записи не мешает.
//
// GET на нём отвечает 500, и соблазн отбить тем же PUT велик. Но PUT
// переписывает файл ЦЕЛИКОМ, то есть чинит ровно эту поломку: отбить его
// значило бы запереть владельца — применить нельзя, потому что применённое
// нечитаемо. Отпечаток при этом не спрашивается: у нечитаемого файла он
// ничего не значит.
func TestRulesetsPutRewritesCorruptFile(t *testing.T) {
	t.Run("пишется без If-Match", func(t *testing.T) {
		s, _ := newServer(t)
		addBypass(t, s)
		writeMixin(t, s, corruptMixin())
		want := rulesets.Config{
			Policy: rulesets.PolicyOnly, Download: rulesets.DownloadDirect,
			Sets: []rulesets.Set{{Name: "youtube"}},
		}
		nikkiFake(t, s).ruleProviders = loadedProviders(want.Sets...)

		rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
		}
		if state, msg := jobOutcome(t, s); state != job.Done {
			t.Fatalf("джоб %s: %s", state, msg)
		}
		got, _ := mixinBytes(t, s)
		if string(got) != string(rulesets.Render(want)) {
			t.Errorf("файл:\n%s\nожидался:\n%s", got, rulesets.Render(want))
		}
	})

	t.Run("применённым считается пустой список", func(t *testing.T) {
		s, f := newServer(t)
		addBypass(t, s)
		writeMixin(t, s, corruptMixin())

		// Имя стоит в строке состояния испорченного файла. Считай мы его
		// применённым — оно проехало бы мимо каталога, и в mixin.yaml
		// уехал бы набор, которого в репозитории нет: провайдер с битой
		// ссылкой и вечное «не загрузился» в панели.
		rec := putRulesets(t, s, `{"policy":"only","sets":["вымышленный"]}`, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("код %d, ожидался 400; тело %s", rec.Code, rec.Body.String())
		}
		if got := errCode(t, rec); got != "unknown_set" {
			t.Errorf("код ошибки %q, ожидался unknown_set", got)
		}
		if len(f.Calls) != 0 {
			t.Errorf("отказ дошёл до системы: %v", f.Calls)
		}
	})
}

// TestRulesetsPutOnUnreadableFile — файл не прочитан вовсе: 500 read_failed.
//
// В отличие от испорченного, здесь неизвестно НИЧЕГО: ни что в файле, ни
// удастся ли в него записать. Переписать его «на всякий случай» значило бы
// затереть неизвестно что там, где сама запись, скорее всего, тоже
// откажет, — а чинится это правами и каталогом, не повтором.
func TestRulesetsPutOnUnreadableFile(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	// Каталог вместо файла: os.ReadFile отказывает, и прав root для этого
	// не нужно — тест обязан идти не от суперпользователя.
	if err := os.Mkdir(s.cfg.MixinPath, 0o755); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, "sha256:0000000000000000")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код %d, ожидался 500; тело %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "read_failed" {
		t.Errorf("код ошибки %q, ожидался read_failed", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("отказ дошёл до системы: %v", f.Calls)
	}
}

// TestRulesetsPutWithoutFingerprint — целый файл, а заголовка нет.
//
// Код тот же, что у разошедшегося отпечатка: лечится и то и другое
// перечитыванием GET. А вот текст обязан быть другим — сказать «выбор
// изменился, пока вы правили» тому, кто отпечатка не прислал вовсе, значит
// отправить его искать вторую вкладку, которой не было.
func TestRulesetsPutWithoutFingerprint(t *testing.T) {
	s, f := newServer(t)
	addBypass(t, s)
	writeMixin(t, s, onlyMixin())
	before, _ := mixinBytes(t, s)

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409; тело %s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не разбирается: %s", rec.Body.String())
	}
	if e.Code != "stale_rulesets" {
		t.Errorf("код ошибки %q, ожидался stale_rulesets", e.Code)
	}
	if !strings.Contains(e.Error, "If-Match") {
		t.Errorf("текст %q не называет недостающий заголовок", e.Error)
	}
	after, _ := mixinBytes(t, s)
	if string(after) != string(before) {
		t.Error("файл переписан отбитым запросом")
	}
	if len(f.Calls) != 0 {
		t.Errorf("отказ дошёл до системы: %v", f.Calls)
	}
}

// TestRulesetsPutWaitsForDownloads — джоб ждёт докачки, а не первого ответа
// движка.
//
// mihomo поднимает Clash API раньше, чем заканчивает initial-загрузку
// провайдеров. Сверка по первому же ответу докладывала бы «не скачалось ни
// одного» про наборы, которые приезжают секундой позже, — то есть красный
// джоб над работающим обходом.
func TestRulesetsPutWaitsForDownloads(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	fp := rulesetsFP(t, s)

	f := nikkiFake(t, s)
	// Счётчик обнуляется ПОСЛЕ отпечатка: GET тоже спрашивает движок, и
	// без обнуления «дождался» и «повезло с первого раза» опять слились бы
	// в одно число.
	f.ruleProvidersCalls = 0
	// Первый ответ пустой: API уже отвечает, .mrs ещё качаются.
	f.onRuleProviders = func(n int) {
		if n >= 2 {
			f.ruleProviders = loadedProviders(rulesets.Set{Name: "youtube"})
		}
	}

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, fp)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	state, msg := jobOutcome(t, s)
	// Число снимается ДО следующего GET: тот добавил бы движку вызов и
	// сделал бы проверку зелёной сам по себе.
	calls := f.ruleProviderCalls()
	if state != job.Done {
		t.Fatalf("джоб %s: %s — по первому пустому ответу движка докладывать рано", state, msg)
	}
	if calls < 2 {
		t.Errorf("движок спрошен %d раз: джоб доложил, не дождавшись докачки", calls)
	}
	if yt := setByName(t, rulesetsGet(t, s), "youtube"); yt["loaded"] != true {
		t.Errorf("youtube loaded %v, ожидалось true", yt["loaded"])
	}
}

// TestRulesetsPutNamesWhyEngineIsSilent — движок не ответил ни разу: причина
// уезжает целиком.
//
// «Clash API не ответил» одинаково подходит и неверному секрету, и мёртвому
// порту, а чинятся они в разных местах: один — правкой
// `nikki.mixin.api_secret`, другой — запуском движка. Без причины в тексте
// владелец перебирает оба наугад.
func TestRulesetsPutNamesWhyEngineIsSilent(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	fp := rulesetsFP(t, s)
	nikkiFake(t, s).ruleProvidersErr = errors.New("401 Unauthorized")

	rec := putRulesets(t, s, `{"policy":"only","sets":["youtube"]}`, fp)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	state, msg := jobOutcome(t, s)
	if state != job.Failed {
		t.Fatalf("джоб %s, ожидался failed", state)
	}
	if !strings.Contains(msg, "401 Unauthorized") {
		t.Errorf("в тексте %q нет причины молчания движка", msg)
	}
	// Файл при этом записан, и текст обязан это сказать: наборы применены,
	// неизвестно лишь, скачались ли они.
	if !strings.Contains(msg, "записаны") {
		t.Errorf("текст %q не говорит, что файл записан", msg)
	}
	if _, ok := mixinBytes(t, s); !ok {
		t.Error("файл убран — это откат, которого нет")
	}
}
