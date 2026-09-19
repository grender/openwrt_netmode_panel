package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"netmoded/internal/happ"
	"netmoded/internal/job"
	"netmoded/internal/mixin"
)

// seedManifest кладёт манифест подписки и перечитывает кэш.
//
// Список узлов экрана берётся из манифеста, а не у движка: выбор владельца
// читается без mihomo, и экран обязан работать при погашенном движке.
func seedManifest(t *testing.T, s *Server, entries []happ.Entry) {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("манифест: %v", err)
	}
	if err := os.WriteFile(s.cfg.ManifestPath, b, 0o600); err != nil {
		t.Fatalf("запись манифеста: %v", err)
	}
	s.reloadManifest()
}

// threeNodes — подписка из трёх узлов и «Авто» с пулом из двух.
func threeNodes() []happ.Entry {
	return []happ.Entry{
		{Name: "Авто", Kind: happ.KindAuto, Pool: []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2"}},
		{Name: "🇵🇱⚡Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "🇨🇭⚡Швейцария 2", Kind: happ.KindNode, Type: "vless"},
		{Name: "🇷🇺💳Россия", Kind: happ.KindNode, Type: "vless"},
	}
}

func getAutopool(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/nikki/autopool", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET код %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	return got
}

func putAutopool(t *testing.T, s *Server, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/nikki/autopool", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func strList(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("ожидался список, пришло %T", v)
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		out = append(out, x.(string))
	}
	return out
}

// Свежая установка: файла нет, выбор умолчательный. Это НОРМАЛЬНОЕ
// состояние, а не отказ, и список узлов при этом уже должен быть — иначе
// отмечать нечего.
func TestAutopoolFreshInstall(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	seedManifest(t, s, threeNodes())

	got := getAutopool(t, s)
	if got["mode"] != string(mixin.AutoDeny) {
		t.Errorf("режим %v, ожидался deny", got["mode"])
	}
	if n := strList(t, got["nodes"]); len(n) != 0 {
		t.Errorf("отмеченных узлов %v, ожидалось пусто", n)
	}
	if a := strList(t, got["available"]); len(a) != 3 {
		t.Errorf("узлов для выбора %v, ожидалось 3", a)
	}
	if p := strList(t, got["provider_pool"]); len(p) != 2 {
		t.Errorf("пул провайдера %v, ожидалось 2 имени", p)
	}
	if got["fingerprint"] == "" {
		t.Error("отпечатка нет — первый PUT будет нечем сопроводить")
	}
	// Движок жив: размер группы AUTO известен.
	if got["pool_size"] == nil {
		t.Error("размер живого пула не отдан")
	}
}

// Движок погашен — экран обязан остаться рабочим: выбор лежит в нашем
// файле. Размер живого пула при этом null, а не ноль: «спросить не
// смогли» и «пул пуст» — разные утверждения.
func TestAutopoolWorksWithEngineDown(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())

	got := getAutopool(t, s)
	if a := strList(t, got["available"]); len(a) != 3 {
		t.Errorf("при погашенном движке список узлов %v", a)
	}
	if got["pool_size"] != nil {
		t.Errorf("pool_size = %v, ожидался null", got["pool_size"])
	}
}

// Запись без отпечатка не доказывает, что видела нынешний выбор.
func TestAutopoolPutNeedsIfMatch(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())

	rec := putAutopool(t, s, `{"mode":"deny","nodes":["🇷🇺💳Россия"]}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
	}
	if code := errCodeOf(t, rec); code != "stale_autopool" {
		t.Errorf("код отказа %q", code)
	}

	rec = putAutopool(t, s, `{"mode":"deny","nodes":["🇷🇺💳Россия"]}`, "sha256:деревянный")
	if rec.Code != http.StatusConflict {
		t.Errorf("устаревший отпечаток: код %d", rec.Code)
	}
}

// Главный путь: отметили Россию, применили — в файле появился
// exclude-filter, а секция наборов осталась нетронутой.
func TestAutopoolPutWritesExcludeFilterAndKeepsSets(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	seedManifest(t, s, threeNodes())

	before := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Sets: []mixin.Set{{Name: "youtube", Action: mixin.ActionTunnel}}}
	writeMixin(t, s, mixin.Render(before))

	rec := putAutopool(t, s, `{"mode":"deny","nodes":["🇷🇺💳Россия"]}`,
		mixin.AutoFingerprint(before.Auto))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	raw, ok := mixinBytes(t, s)
	if !ok {
		t.Fatal("файл не создан")
	}
	if !strings.Contains(string(raw), "exclude-filter") {
		t.Errorf("в файле нет exclude-filter:\n%s", raw)
	}
	got, _, err := mixin.Parse(raw)
	if err != nil {
		t.Fatalf("разбор: %v\n%s", err, raw)
	}
	if got.Auto.Mode != mixin.AutoDeny || len(got.Auto.Nodes) != 1 {
		t.Errorf("пул записан как %+v", got.Auto)
	}
	// Вторая секция того же файла обязана пережить запись первой.
	if len(got.Sets) != 1 || got.Sets[0].Name != "youtube" {
		t.Errorf("наборы потерялись: %+v", got.Sets)
	}
}

// Режим «как в подписке» состава не принимает: список берёт демон из
// балансировщика, и в этом весь смысл режима.
func TestAutopoolProviderModeTakesPoolFromManifest(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	seedManifest(t, s, threeNodes())

	rec := putAutopool(t, s, `{"mode":"provider","nodes":["🇷🇺💳Россия"]}`,
		mixin.AutoFingerprint(mixin.AutoConfig{}))
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
	if got.Auto.Mode != mixin.AutoProvider {
		t.Fatalf("режим %q", got.Auto.Mode)
	}
	if strings.Join(got.Auto.Nodes, "|") != "🇵🇱⚡Польша|🇨🇭⚡Швейцария 2" {
		t.Errorf("состав %v, ожидался пул провайдера, а не присланный список", got.Auto.Nodes)
	}
}

// У провайдера своего авторежима может не быть вовсе — тогда режим брать
// неоткуда, и отказ обязан это назвать, а не записать пустоту.
func TestAutopoolProviderModeWithoutBalancer(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, []happ.Entry{{Name: "🇵🇱⚡Польша", Kind: happ.KindNode, Type: "vless"}})

	rec := putAutopool(t, s, `{"mode":"provider"}`, mixin.AutoFingerprint(mixin.AutoConfig{}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
	}
	if code := errCodeOf(t, rec); code != "no_provider_pool" {
		t.Errorf("код отказа %q", code)
	}
}

func TestAutopoolPutRefusals(t *testing.T) {
	for _, tt := range []struct {
		name, body, code string
	}{
		{"незнакомый режим", `{"mode":"как-нибудь"}`, "bad_request"},
		{"не JSON", `{`, "bad_request"},
		{"имени нет в подписке", `{"mode":"allow","nodes":["🇿🇼Нет такого"]}`, "unknown_node"},
		{"пустой пул у allow", `{"mode":"allow","nodes":[]}`, "bad_autopool"},
		// Исключили всё, что есть, — в пуле не осталось ничего. Движок
		// переживёт (empty-fallback), но записывать мёртвую группу нельзя.
		{"исключили всех", `{"mode":"deny","nodes":["🇵🇱⚡Польша","🇨🇭⚡Швейцария 2","🇷🇺💳Россия"]}`, "empty_pool"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newServer(t)
			seedManifest(t, s, threeNodes())
			rec := putAutopool(t, s, tt.body, mixin.AutoFingerprint(mixin.AutoConfig{}))
			if rec.Code < 400 {
				t.Fatalf("код %d, ожидался отказ: %s", rec.Code, rec.Body.String())
			}
			if code := errCodeOf(t, rec); code != tt.code {
				t.Errorf("код отказа %q, ожидался %q: %s", code, tt.code, rec.Body.String())
			}
			if _, ok := mixinBytes(t, s); ok {
				t.Error("отказ, а файл всё-таки записан")
			}
		})
	}
}

// Имя, которое провайдер переименовал или убрал. Молчать нельзя: в
// режиме «только отмеченные» оно молча уменьшает пул.
func TestAutopoolReportsMissingNames(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	writeMixin(t, s, mixin.Render(mixin.Config{
		Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Auto: mixin.AutoConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇵🇱⚡Польша", "🇩🇪Которого нет"}},
	}))

	got := getAutopool(t, s)
	miss := strList(t, got["missing"])
	if len(miss) != 1 || miss[0] != "🇩🇪Которого нет" {
		t.Errorf("пропавшие имена %v", miss)
	}
}

// Склейка включается по ВСЕМУ файлу. Наборы могут стоять в «правила из
// профиля» — это про правила, а не про узлы; выключив флаг, демон молча
// обесценил бы выбор пула.
func TestAutopoolTurnsMixinFlagOnEvenWithProfilePolicy(t *testing.T) {
	s, f := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	seedManifest(t, s, threeNodes())
	f.UCIValues["nikki.mixin.mixin_file_content"] = "0"

	before := mixin.Config{Policy: mixin.PolicyProfile, Download: mixin.DownloadDirect}
	writeMixin(t, s, mixin.Render(before))

	rec := putAutopool(t, s, `{"mode":"allow","nodes":["🇵🇱⚡Польша"]}`,
		mixin.AutoFingerprint(before.Auto))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}
	if callIndex(f.Calls, "set nikki.mixin.mixin_file_content=1") < 0 {
		t.Errorf("флаг склейки не включён, файл лежит мёртвым грузом: %v", f.Calls)
	}
}

// Режим «как в подписке» обязан следовать за подпиской: провайдер
// добавил сервер в свой балансировщик — он появился и у нас. Иначе режим
// врал бы названием.
func TestProviderPoolFollowsSubscription(t *testing.T) {
	s, f := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	seedManifest(t, s, threeNodes())
	writeMixin(t, s, mixin.Render(mixin.Config{
		Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Auto: mixin.AutoConfig{Mode: mixin.AutoProvider, Nodes: []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2"}},
	}))

	// Провайдер добавил Россию в свой балансировщик.
	grown := threeNodes()
	grown[0].Pool = []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2", "🇷🇺💳Россия"}
	seedManifest(t, s, grown)

	s.resyncProviderPool(context.Background())

	raw, _ := mixinBytes(t, s)
	got, _, err := mixin.Parse(raw)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if len(got.Auto.Nodes) != 3 {
		t.Errorf("состав пула %v, ожидалось 3 имени", got.Auto.Nodes)
	}
	if callIndex(f.Calls, "apply-mode nikki") < 0 {
		t.Error("состав изменился, а движок не перезапущен — новый пул не действует")
	}
}

// А вот от НЕИЗМЕНИВШЕГОСЯ состава движок трогать нельзя. Обновление
// подписки идёт раз в двенадцать часов, и гасить обход на каждое из них
// ради файла, который не поменялся, значит платить перерывом связи за
// ничто. Перестановка изменением не считается: порядок участников группы
// решает mihomo.
func TestProviderPoolDoesNotRestartOnSameComposition(t *testing.T) {
	s, f := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	seedManifest(t, s, threeNodes())
	writeMixin(t, s, mixin.Render(mixin.Config{
		Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Auto: mixin.AutoConfig{Mode: mixin.AutoProvider, Nodes: []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2"}},
	}))

	shuffled := threeNodes()
	shuffled[0].Pool = []string{"🇨🇭⚡Швейцария 2", "🇵🇱⚡Польша"}
	seedManifest(t, s, shuffled)

	s.resyncProviderPool(context.Background())

	if callIndex(f.Calls, "apply-mode nikki") >= 0 {
		t.Errorf("движок перезапущен из-за перестановки имён: %v", f.Calls)
	}
}

// Ручной выбор подписка трогать не смеет: владелец отметил узлы сам.
func TestManualPoolIgnoresSubscription(t *testing.T) {
	s, f := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())
	before := mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Auto: mixin.AutoConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇵🇱⚡Польша"}}}
	writeMixin(t, s, mixin.Render(before))
	grown := threeNodes()
	grown[0].Pool = []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2", "🇷🇺💳Россия"}
	seedManifest(t, s, grown)

	s.resyncProviderPool(context.Background())

	raw, _ := mixinBytes(t, s)
	if string(raw) != string(mixin.Render(before)) {
		t.Errorf("ручной выбор переписан обновлением подписки:\n%s", raw)
	}
	if callIndex(f.Calls, "apply-mode nikki") >= 0 {
		t.Error("движок перезапущен из-за чужого режима")
	}
}
