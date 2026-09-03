package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"netmoded/internal/happ"
	"netmoded/internal/nikki"
)

func TestNikkiProxiesShape(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/nikki/proxies", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Available  bool          `json:"available"`
		Group      string        `json:"group"`
		Type       string        `json:"type"`
		Selected   string        `json:"selected"`
		Selectable bool          `json:"selectable"`
		Pinned     bool          `json:"pinned"`
		Members    []nikki.Proxy `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !got.Available || got.Group != "PROXY" || got.Type != "URLTest" {
		t.Errorf("%+v", got)
	}
	// URLTest принимает закрепление: mihomo проверяет интерфейс SelectAble,
	// а не конкретный тип группы.
	if !got.Selectable {
		t.Error("URLTest обязан принимать ручное закрепление")
	}
	if got.Pinned {
		t.Error("исходно ничего не закреплено — pinned должен быть false")
	}
	if got.Selected != "🇨🇭⚡Швейцария 2" {
		t.Errorf("selected = %q", got.Selected)
	}
	if len(got.Members) != 3 {
		t.Errorf("участников %d, ожидалось 3", len(got.Members))
	}
}

// Мёртвый узел отдаётся с delay_ms: null, а не 0: ноль в ответе mihomo
// означает несостоявшуюся пробу, и покрасить его как самый быстрый было
// бы прямо неверно.
func TestNikkiDeadNodeHasNullDelay(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/nikki/proxies", true)

	var got struct {
		Members []struct {
			Name    string `json:"name"`
			DelayMS *int   `json:"delay_ms"`
		} `json:"members"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	for _, m := range got.Members {
		if m.Name == "мёртвый" {
			if m.DelayMS != nil {
				t.Errorf("мёртвый узел отдан с задержкой %d, ожидался null", *m.DelayMS)
			}
			return
		}
	}
	t.Error("мёртвый узел не найден в ответе")
}

// Закрепление узла работает и у URLTest — правка профиля не нужна.
func TestNikkiSelectPinsNode(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	s.SetNikkiClient(fake)

	rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Selected string `json:"selected"`
		Fixed    string `json:"fixed"`
		Pinned   bool   `json:"pinned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Selected != "🇵🇱⚡Польша" || got.Fixed != "🇵🇱⚡Польша" || !got.Pinned {
		t.Errorf("после закрепления: %+v", got)
	}
}

// AUTO из SPEC §7 снимает закрепление. Спека предполагала вложенную группу
// в профиле; движок умеет это сам, поэтому внешний контракт сохранён,
// а профиль не трогается.
func TestNikkiAutoUnpins(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	s.SetNikkiClient(fake)

	if rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("закрепление: %d", rec.Code)
	}
	rec := post(t, s, "/api/nikki/proxy", `{"name":"AUTO"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("AUTO: код %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Fixed  string `json:"fixed"`
		Pinned bool   `json:"pinned"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Pinned || got.Fixed != "" {
		t.Errorf("после AUTO закрепление осталось: %+v", got)
	}
}

func TestNikkiSelectUnknownNodeIs404(t *testing.T) {
	s, _ := newServer(t)
	rec := post(t, s, "/api/nikki/proxy", `{"name":"нет-такого-узла"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("код %d, ожидался 404", rec.Code)
	}
}

func TestNikkiSelectRequiresName(t *testing.T) {
	s, _ := newServer(t)
	for _, body := range []string{`{}`, `{"name":""}`, `{`} {
		if rec := post(t, s, "/api/nikki/proxy", body, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("тело %q → %d, ожидался 400", body, rec.Code)
		}
	}
}

func TestNikkiUnavailableDegradesNotFails(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	fake.err = nikki.ErrUnavailable
	s.SetNikkiClient(fake)

	rec := do(t, s, "GET", "/api/nikki/proxies", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d, ожидался 503", rec.Code)
	}
	if got := errCode(t, rec); got != "nikki_unavailable" {
		t.Errorf("код ошибки %q", got)
	}

	// Статус продолжает отвечать.
	st := do(t, s, "GET", "/api/status", true)
	if st.Code != http.StatusOK {
		t.Fatalf("статус упал из-за Nikki: %d", st.Code)
	}
}

func TestStatusCarriesNikkiNode(t *testing.T) {
	s, _ := newServer(t)
	rec := do(t, s, "GET", "/api/status", true)

	var got struct {
		Nikki struct {
			Available bool   `json:"available"`
			Set       string `json:"set"`
		} `json:"nikki"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !got.Nikki.Available || got.Nikki.Set != "🇨🇭⚡Швейцария 2" {
		t.Errorf("nikki в статусе: %+v", got.Nikki)
	}
}

// --- POST /api/nikki/test ---

// nikkiFake достаёт подделку, которую newServer положил в сервер: тесты
// замера смотрят не только на ответ, но и на то, КОГО опрашивали.
func nikkiFake(t *testing.T, s *Server) *fakeNikkiClient {
	t.Helper()
	f, ok := s.nikki.(*fakeNikkiClient)
	if !ok {
		t.Fatalf("клиент nikki подменён на %T", s.nikki)
	}
	return f
}

type testBody struct {
	Members []struct {
		Name    string `json:"name"`
		DelayMS *int   `json:"delay_ms"`
	} `json:"members"`
	Test *struct {
		Total     int `json:"total"`
		Measured  int `json:"measured"`
		Failed    int `json:"failed"`
		Skipped   int `json:"skipped"`
		ElapsedMS int `json:"elapsed_ms"`
	} `json:"test"`
}

// Замер обязан опросить каждого участника группы и доложить итог.
//
// Без поля test ответ неотличим от простого чтения списка: задержки в нём
// есть и без всякого замера — mihomo обновляет history сам (RQ-06). То есть
// нажатие, ничего не измерившее, выглядело бы точно так же, как удачное.
func TestNikkiTestProbesEveryMemberAndReports(t *testing.T) {
	s, _ := newServer(t)
	f := nikkiFake(t, s)

	rec := do(t, s, "POST", "/api/nikki/test", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Test == nil {
		t.Fatal("в ответе нет поля test — исход замера потерян")
	}
	// Трое участников: двое живых и «мёртвый».
	if got.Test.Total != 3 || got.Test.Measured != 2 || got.Test.Failed != 1 || got.Test.Skipped != 0 {
		t.Errorf("итог %+v, ожидалось 3/2/1/0", *got.Test)
	}
	// Ответ несёт и свежий список — панель обновляет числа тем же запросом.
	if len(got.Members) != 3 {
		t.Errorf("участников в ответе %d", len(got.Members))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delayed) != 3 {
		t.Fatalf("проб %d: %v", len(f.delayed), f.delayed)
	}
	for _, n := range f.delayed {
		if n == ProxyGroup || n == "GLOBAL" {
			t.Errorf("замерялась группа %q, а не узел: её задержка — это задержка "+
				"выбранного члена, то есть двойная проба одного узла", n)
		}
	}
}

// Замер — триггер, а не источник чисел, но числа он обязан обновить: иначе
// проверить, что проба вообще состоялась, нечем.
func TestNikkiTestRefreshesDelays(t *testing.T) {
	s, _ := newServer(t)

	rec := do(t, s, "POST", "/api/nikki/test", true)
	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	for _, m := range got.Members {
		switch m.Name {
		case "мёртвый":
			// Провал пробы не сочиняет число: прочерк остаётся прочерком.
			if m.DelayMS != nil {
				t.Errorf("мёртвому узлу приписана задержка %d", *m.DelayMS)
			}
		default:
			if m.DelayMS == nil || *m.DelayMS != 11 {
				t.Errorf("узел %q: задержка не обновилась (%v)", m.Name, m.DelayMS)
			}
		}
	}
}

// Разделители подписки («⬇️ Обходы белых списков ⬇️») в ответе /proxies
// отдельными узлами не значатся. Проба по такому имени провалилась бы, и
// кнопка честно докладывала бы о мёртвом узле, которого не существует.
func TestNikkiTestSkipsSubscriptionSeparators(t *testing.T) {
	s, _ := newServer(t)
	f := nikkiFake(t, s)

	f.mu.Lock()
	g := f.all[ProxyGroup]
	g.Members = append(g.Members, "⬇️ Обходы белых списков ⬇️")
	f.all[ProxyGroup] = g
	f.mu.Unlock()

	rec := do(t, s, "POST", "/api/nikki/test", true)
	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Test == nil || got.Test.Total != 3 || got.Test.Failed != 1 {
		t.Fatalf("итог %+v: разделитель попал в замер", got.Test)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.delayed {
		if n == "⬇️ Обходы белых списков ⬇️" {
			t.Error("разделитель подписки опрошен как узел")
		}
	}
}

// Молчащий Clash API — это 503, а не «замерили ноль узлов»: замерять нечего,
// и притворяться, что операция прошла, нельзя.
func TestNikkiTestOnDeadEngineIs503(t *testing.T) {
	s, _ := newServer(t)
	f := nikkiFake(t, s)
	f.mu.Lock()
	f.err = nikki.ErrUnavailable
	f.mu.Unlock()

	rec := do(t, s, "POST", "/api/nikki/test", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503: %s", rec.Code, rec.Body.String())
	}
}

// Маршрут закрыт токеном, как и остальные: без него — 401, а не замер.
func TestNikkiTestNeedsToken(t *testing.T) {
	s, _ := newServer(t)
	if rec := do(t, s, "POST", "/api/nikki/test", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("код %d без токена", rec.Code)
	}
}

// Поле test не может подменить поля контракта: добавка кладётся так, чтобы
// members или selected остались тем, что отдал движок.
func TestNikkiTestExtraCannotOverwriteContract(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/nikki/test", nil)
	s.writeProxies(rec, req, map[string]any{"members": "подменено", "test": 1})

	var got struct {
		Members []nikki.Proxy `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v — members затёрты строкой", err)
	}
	if len(got.Members) != 3 {
		t.Errorf("участников %d", len(got.Members))
	}
}

// ─────────── порядок и виды строк из манифеста подписки ───────────

// withManifest кладёт манифест подписки прямо в кэш сервера.
//
// Не через файл: путь манифеста — константа /etc/netmoded/subscription.json,
// которой на машине разработчика нет, а проверяется здесь склейка порядка с
// живым списком, а не чтение файла (оно проверено в internal/subs).
func withManifest(s *Server, entries []happ.Entry) {
	s.manifestMu.Lock()
	s.manifest = entries
	s.manifestMu.Unlock()
}

// subManifest — раскладка провайдера в миниатюре: все четыре вида записи и
// порядок, отличный от порядка группы в подделке mihomo.
//
// «мёртвый» в манифест НЕ входит намеренно: он живой участник группы мимо
// подписки, и его место — в хвосте списка, а не в небытии.
func subManifest() []happ.Entry {
	return []happ.Entry{
		{Name: "Авто | Лучший сервер", Kind: happ.KindAuto},
		{Name: "🇨🇭⚡Швейцария 2", Kind: happ.KindNode, Type: "vless"},
		{Name: "⬇️ Обходы белых списков ⬇️", Kind: happ.KindSeparator},
		{Name: "🇵🇱⚡Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "🇷🇺 Россия (wl)", Kind: happ.KindUnsupported,
			Reason: "транспорт xhttp выключен настройкой"},
	}
}

type memberRow struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Alive  bool   `json:"alive"`
}

func membersOf(t *testing.T, rec *httptest.ResponseRecorder) []memberRow {
	t.Helper()
	var got struct {
		Members []memberRow `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v — %s", err, rec.Body.String())
	}
	return got.Members
}

// Порядок в ответе повторяет манифест, а не раскладку mihomo.
//
// Движок отдаёт узлы объектом и авторский порядок провайдера теряет целиком;
// без манифеста «Авто» и заголовок раздела в списке не появятся вовсе, а
// узлы придут в порядке группы. Здесь порядок группы отличается от порядка
// подписки — иначе тест был бы зелёным и при полностью проигнорированном
// манифесте.
func TestNikkiProxiesFollowManifestOrder(t *testing.T) {
	s, _ := newServer(t)
	withManifest(s, subManifest())

	got := membersOf(t, do(t, s, "GET", "/api/nikki/proxies", true))

	want := []struct{ name, kind string }{
		{"Авто | Лучший сервер", "auto"},
		{"🇨🇭⚡Швейцария 2", "node"},
		{"⬇️ Обходы белых списков ⬇️", "separator"},
		{"🇵🇱⚡Польша", "node"},
		{"🇷🇺 Россия (wl)", "unsupported"},
		// Живой участник мимо манифеста — в хвост, а не в небытие: молча
		// выброшенный узел неотличим от узла, которого больше нет.
		{"мёртвый", "node"},
	}
	if len(got) != len(want) {
		t.Fatalf("строк %d, ожидалось %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Kind != w.kind {
			t.Errorf("строка %d: %+v, ожидалось %s/%s", i, got[i], w.name, w.kind)
		}
	}
	// Причина непригодности уезжает в панель как есть — без неё строка
	// выглядит просто исчезнувшим узлом.
	if got[4].Reason == "" {
		t.Error("у unsupported пустой reason: владельцу не сказано, что случилось")
	}
}

// Строка не-node не выбирается, и отказ приходит ДО похода в движок.
//
// 409, а не 404: строка в списке ЕСТЬ, владелец её видит, и «узел не
// найден» было бы неправдой. И не 200: заголовок раздела в подписке —
// побайтовая копия соседнего узла, и «выбор» по нему увёл бы трафик не
// туда молча.
func TestNikkiSelectRejectsNonNodeRows(t *testing.T) {
	for _, name := range []string{
		"⬇️ Обходы белых списков ⬇️", // separator
		"Авто | Лучший сервер",       // auto
		"🇷🇺 Россия (wl)",             // unsupported
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newServer(t)
			withManifest(s, subManifest())
			f := nikkiFake(t, s)

			rec := post(t, s, "/api/nikki/proxy", `{"name":`+quote(name)+`}`, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != "member_not_selectable" {
				t.Errorf("код ошибки %q", got)
			}
			// Ни одного закрепления в движке: отказ обязан случиться ДО
			// запроса, иначе mihomo успел бы выбрать двойника.
			f.mu.Lock()
			defer f.mu.Unlock()
			if g := f.all[ProxyGroup]; g.Pinned || g.Fixed != "" {
				t.Errorf("в движок всё-таки сходили: fixed=%q pinned=%v", g.Fixed, g.Pinned)
			}
		})
	}
}

// AUTO — наш сентинель снятия закрепления, а не имя строки, и правило про
// виды его не касается. Проверяется при ЗАПОЛНЕННОМ манифесте, где строка
// «Авто» существует и отбивается 409: перепутать эти два пути легко.
func TestNikkiAutoStillWorksWithManifest(t *testing.T) {
	s, _ := newServer(t)
	withManifest(s, subManifest())

	if rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("закрепление узла: код %d: %s", rec.Code, rec.Body.String())
	}
	rec := post(t, s, "/api/nikki/proxy", `{"name":"AUTO"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("AUTO: код %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fixed  string `json:"fixed"`
		Pinned bool   `json:"pinned"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Pinned || got.Fixed != "" {
		t.Errorf("после AUTO закрепление осталось: %+v", got)
	}
}

// Узел, которого в манифесте нет, выбирается как раньше: вид неизвестен —
// это не «неподходящий вид». Манифест отстаёт от файла провайдера чаще, чем
// хотелось бы, и запрет по незнанию отнял бы у владельца рабочий узел.
func TestNikkiSelectAllowsNodeOutsideManifest(t *testing.T) {
	s, _ := newServer(t)
	withManifest(s, subManifest())

	rec := post(t, s, "/api/nikki/proxy", `{"name":"мёртвый"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200: %s", rec.Code, rec.Body.String())
	}
}

// Замер трогает только узлы: разделитель, «Авто» и непереводимая запись в
// движке не существуют, и проба по такому имени осела бы в failed — то есть
// кнопка доложила бы о мёртвом узле, которого никогда не было.
func TestNikkiTestProbesOnlyNodes(t *testing.T) {
	s, _ := newServer(t)
	withManifest(s, subManifest())
	f := nikkiFake(t, s)

	rec := do(t, s, "POST", "/api/nikki/test", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var got testBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	// Трое: два живых узла манифеста и «мёртвый» из хвоста. Строки «Авто»,
	// разделителя и unsupported в замере нет.
	if got.Test == nil || got.Test.Total != 3 || got.Test.Measured != 2 || got.Test.Failed != 1 {
		t.Fatalf("итог %+v, ожидалось 3/2/1", got.Test)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	probed := map[string]bool{}
	for _, n := range f.delayed {
		probed[n] = true
	}
	for _, n := range []string{"Авто | Лучший сервер", "⬇️ Обходы белых списков ⬇️", "🇷🇺 Россия (wl)"} {
		if probed[n] {
			t.Errorf("строка %q опрошена как узел", n)
		}
	}
	for _, n := range []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2", "мёртвый"} {
		if !probed[n] {
			t.Errorf("узел %q не опрошен", n)
		}
	}
}

// Свежая установка: манифеста нет ни одного байта. Это НЕ запасной путь, а
// состояние всякого роутера до первого успешного обновления, и вести себя
// демон обязан ровно так, как вёл до появления подписки.
func TestNikkiWithoutManifestBehavesAsBefore(t *testing.T) {
	s, _ := newServer(t) // манифест пуст

	// Список — в порядке mihomo, все строки node, ни одного отсева.
	got := membersOf(t, do(t, s, "GET", "/api/nikki/proxies", true))
	want := []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2", "мёртвый"}
	if len(got) != len(want) {
		t.Fatalf("строк %d, ожидалось %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Name != w || got[i].Kind != "node" {
			t.Errorf("строка %d: %+v, ожидалось %s/node", i, got[i], w)
		}
	}

	// Видов не существует — проверять нечего, и запрос уходит в движок.
	// Отказ приходит ОТ ДВИЖКА (404), а не от нашей проверки (409): иначе
	// на свежей установке нельзя было бы выбрать ни один узел.
	rec := post(t, s, "/api/nikki/proxy", `{"name":`+quote("⬇️ Обходы белых списков ⬇️")+`}`, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404 от движка: %s", rec.Code, rec.Body.String())
	}

	// Замер опрашивает всех участников группы, как и раньше.
	if rec := do(t, s, "POST", "/api/nikki/test", true); rec.Code != http.StatusOK {
		t.Fatalf("замер: код %d", rec.Code)
	}
	f := nikkiFake(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delayed) != 3 {
		t.Errorf("проб %d, ожидалось 3: %v", len(f.delayed), f.delayed)
	}
}

// quote — имя строки в JSON-тело запроса. Имена содержат эмодзи и пробелы,
// и склеивать их вручную значит однажды получить невалидный JSON вместо
// проверяемого отказа.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
