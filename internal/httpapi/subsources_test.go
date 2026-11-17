package httpapi

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
	"time"

	"netmoded/internal/happ"
	"netmoded/internal/sched"
)

// Несколько подписок (ADR-0052). Адрес второй — без параметров: секрет в
// пути, как у провайдера, с которого форма снята.
const (
	secondURL   = "https://second.example.invalid/S3C0NDSECRETPATH"
	secondBody  = `[{"remarks":"🇫🇯 Alpha-1","outbounds":[{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"203.0.113.20","port":443,"users":[{"id":"00000000-0000-0000-0000-000000000002","flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"sni2.example.net","publicKey":"REDACTED_PUBKEY-02AAAAAAAAAAAAAAAAAAAAAAAAA","shortId":"5ecec50000000002","fingerprint":"chrome"}}}]}]`
	mainBody    = `[{"remarks":"🇧🇧 Браво","outbounds":[{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"203.0.113.10","port":443,"users":[{"id":"00000000-0000-0000-0000-000000000001","flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"sni1.example.net","publicKey":"REDACTED_PUBKEY-01AAAAAAAAAAAAAAAAAAAAAAAAA","shortId":"5ecec50000000001","fingerprint":"chrome"}}}]}]`
	testSubHWID = "5ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2"
)

func delSub(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s, http.MethodDelete, path, true)
}

type subsList struct {
	Max     int `json:"max"`
	Sources []struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		Configured bool   `json:"configured"`
		Masked     string `json:"masked"`
		HWID       bool   `json:"hwid"`
		Last       *struct {
			Status string `json:"status"`
			Nodes  int    `json:"nodes"`
			Error  string `json:"error"`
		} `json:"last"`
	} `json:"sources"`
}

func listSubs(t *testing.T, s *Server) (subsList, string) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/subscriptions", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	var l subsList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	return l, rec.Body.String()
}

func addSecond(t *testing.T, s *Server, hwid bool) string {
	t.Helper()
	body := `{"label":"Вторая","url":"` + secondURL + `","hwid":false}`
	if hwid {
		body = `{"label":"Вторая","url":"` + secondURL + `","hwid":true}`
	}
	rec := post(t, s, "/api/subscriptions", body, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}
	l, _ := listSubs(t, s)
	return l.Sources[len(l.Sources)-1].ID
}

func TestSubscriptionsListStartsWithMain(t *testing.T) {
	s, _ := newServer(t)
	l, _ := listSubs(t, s)
	if l.Max != maxSubSources || len(l.Sources) != 1 || l.Sources[0].ID != happ.MainSource || l.Sources[0].Configured {
		t.Fatalf("список: %+v", l)
	}
}

func TestSubscriptionsAddWritesSectionAndHidesSecrets(t *testing.T) {
	s, f := newServer(t)
	id := addSecond(t, s, true)
	if id == "" || id == happ.MainSource {
		t.Fatalf("id %q", id)
	}
	for _, c := range []string{
		"add-named netmode." + id + "=subscription",
		"set netmode." + id + ".url=" + secondURL,
		"set netmode." + id + ".hwid=1",
		"commit netmode",
	} {
		if !hasCall(f.Calls, c) {
			t.Errorf("нет вызова %q: %v", c, f.Calls)
		}
	}
	// ID устройства заведён и записан, но наружу не уходит — как и адрес.
	hwid := s.subExtra.deviceID()
	if len(hwid) != 64 || !hasCall(f.Calls, "set netmode.main.hwid="+hwid) {
		t.Fatalf("ID устройства не заведён: %d знаков", len(hwid))
	}
	l, body := listSubs(t, s)
	if strings.Contains(body, "S3C0NDSECRETPATH") || strings.Contains(body, hwid) {
		t.Errorf("секрет в ответе: %s", body)
	}
	got := l.Sources[1]
	if got.Label != "Вторая" || !got.Configured || !got.HWID || got.Masked != "https://second.example.invalid/…" {
		t.Errorf("подписка: %+v", got)
	}
}

// ID устройства один на роутер: вторая подписка с ним не заводит новый.
func TestSubscriptionsReuseDeviceID(t *testing.T) {
	s, f := newServer(t)
	s.subExtra.setDeviceID(testSubHWID)
	addSecond(t, s, true)
	for _, c := range f.Calls {
		if strings.Contains(c, "netmode.main.hwid") {
			t.Errorf("ID устройства перезаписан: %s", c)
		}
	}
}

func TestSubscriptionsAddRejects(t *testing.T) {
	s, _ := newServer(t)
	cases := map[string]string{
		"пустое имя":        `{"label":"","url":"` + secondURL + `"}`,
		"длинное имя":       `{"label":"` + strings.Repeat("я", 25) + `","url":"` + secondURL + `"}`,
		"разделитель":       `{"label":"A · B","url":"` + secondURL + `"}`,
		"имя основной":      `{"label":"основная","url":"` + secondURL + `"}`,
		"знак из регулярок": `{"label":"a|b","url":"` + secondURL + `"}`,
		"нет адреса":        `{"label":"X","url":""}`,
		"не URL":            `{"label":"X","url":"ftp://x/y"}`,
		"не JSON":           `{`,
	}
	for name, body := range cases {
		rec := post(t, s, "/api/subscriptions", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: код %d", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "S3C0NDSECRETPATH") {
			t.Errorf("%s: адрес в тексте отказа", name)
		}
	}
	addSecond(t, s, false)
	if rec := post(t, s, "/api/subscriptions", `{"label":"вторая","url":"`+secondURL+`"}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("повтор имени: код %d", rec.Code)
	}
}

func TestSubscriptionsLimit(t *testing.T) {
	s, _ := newServer(t)
	for _, l := range []string{"A", "B", "C"} {
		if rec := post(t, s, "/api/subscriptions", `{"label":"`+l+`","url":"`+secondURL+`"}`, ""); rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", l, rec.Code, rec.Body.String())
		}
	}
	rec := post(t, s, "/api/subscriptions", `{"label":"D","url":"`+secondURL+`"}`, "")
	if got := errCode(t, rec); rec.Code != http.StatusConflict || got != "too_many_subscriptions" {
		t.Errorf("пятая подписка: %d %s", rec.Code, got)
	}
}

func TestSubscriptionsEdit(t *testing.T) {
	s, f := newServer(t)
	id := addSecond(t, s, false)
	// Пустой адрес — «оставить прежний»: панель его не знает.
	rec := put(t, s, "/api/subscriptions/"+id, `{"label":"Другая","url":"","hwid":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	c, _ := s.subExtra.find(id)
	if c.Label != "Другая" || c.URL != secondURL || !c.HWID {
		t.Errorf("после правки: label=%q hwid=%v, адрес сохранён: %v", c.Label, c.HWID, c.URL == secondURL)
	}
	if !hasCall(f.Calls, "set netmode."+id+".label=Другая") {
		t.Errorf("имя не записано: %v", f.Calls)
	}
	if rec := put(t, s, "/api/subscriptions/nosuch", `{"label":"X"}`); errCode(t, rec) != "unknown_subscription" {
		t.Errorf("чужой id: %d", rec.Code)
	}
	// Основная правится тем же путём, что всегда.
	if rec := put(t, s, "/api/subscriptions/main", `{"url":"`+secretURL+`"}`); rec.Code != http.StatusOK || s.subURL.get() != secretURL {
		t.Errorf("основная: %d", rec.Code)
	}
}

// Обновление сливает обе подписки, ID устройства уходит только второй,
// а отказ второй оставляет её узлы и помечает итог «частично».
func TestSubscriptionsUpdateMergesAndSurvivesFailure(t *testing.T) {
	s, _ := newServer(t)
	s.subExtra.setDeviceID(testSubHWID)
	if rec := put(t, s, "/api/subscription", `{"url":"`+secretURL+`"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	id := addSecond(t, s, true)

	// Стенд по умолчанию ставит планировщику подделку обновления; здесь
	// нужен настоящий Updater за ним.
	s.sched = sched.New(s.subRefresher, s.logs, 0, s.logf)
	var failSecond error
	seen := map[string]string{}
	s.subUpdater.FetchWith = func(_ context.Context, url string, o happ.FetchOpts) ([]byte, error) {
		seen[url] = o.HWID
		if url == secondURL {
			if failSecond != nil {
				return nil, failSecond
			}
			return []byte(secondBody), nil
		}
		return []byte(mainBody), nil
	}
	f := nikkiFake(t, s)
	f.mu.Lock()
	f.providerNames = []string{"🇧🇧 Браво", "🇫🇯 Alpha-1"}
	f.mu.Unlock()

	run := func() {
		t.Helper()
		rec := post(t, s, "/api/subscription/update", ``, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
		}
		s.jobs.Wait(3 * time.Second)
	}
	run()
	if seen[secretURL] != "" || seen[secondURL] != testSubHWID {
		t.Errorf("ID устройства ушёл не туда")
	}
	var names []string
	for _, e := range s.manifestEntries() {
		names = append(names, e.Name+"@"+e.Source)
	}
	if strings.Join(names, "|") != "🇧🇧 Браво@|🇫🇯 Alpha-1@"+id {
		t.Fatalf("манифест: %v", names)
	}
	f.mu.Lock()
	reloaded := strings.Join(f.reloaded, ",")
	f.mu.Unlock()
	if !strings.Contains(reloaded, "sub") || !strings.Contains(reloaded, "sub-auto") {
		t.Errorf("перечитаны провайдеры: %s", reloaded)
	}

	failSecond = &happ.StatusError{Code: 403}
	run()
	if n := len(s.manifestEntries()); n != 2 {
		t.Errorf("после отказа второй в манифесте %d записей", n)
	}
	l, body := listSubs(t, s)
	if l.Sources[0].Last == nil || l.Sources[0].Last.Status != "ok" ||
		l.Sources[1].Last == nil || l.Sources[1].Last.Status != "fail" || l.Sources[1].Last.Nodes != 1 {
		t.Errorf("итоги: %s", body)
	}
	cur := time.Now().Add(StatusCacheTTL + time.Second)
	s.status.now = func() time.Time { return cur }
	sub := statusSub(t, s)
	if sub["status"] != "ok" || sub["partial"] != true || sub["nodes"] != float64(2) {
		t.Errorf("статус: %v", sub)
	}
	// Ни адрес, ни ID устройства — ни в статусе, ни в списке, ни в журнале.
	st := do(t, s, http.MethodGet, "/api/status", true).Body.String()
	lg := do(t, s, http.MethodGet, "/api/logs", true).Body.String()
	for _, where := range []string{st, lg, body} {
		for _, secret := range []string{"S3C0NDSECRETPATH", testSubHWID, secretURL} {
			if strings.Contains(where, secret) {
				t.Errorf("секрет %q утёк: %.200s", secret[:8], where)
			}
		}
	}
}

func TestSubscriptionsDeleteRebuildsPool(t *testing.T) {
	s, f := newServer(t)
	if rec := put(t, s, "/api/subscription", `{"url":"`+secretURL+`"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	id := addSecond(t, s, false)
	s.subUpdater.FetchWith = func(_ context.Context, url string, _ happ.FetchOpts) ([]byte, error) {
		if url == secondURL {
			return []byte(secondBody), nil
		}
		return []byte(mainBody), nil
	}
	s.sched = sched.New(s.subRefresher, s.logs, 0, s.logf)
	nf := nikkiFake(t, s)
	nf.mu.Lock()
	nf.providerNames = []string{"🇧🇧 Браво", "🇫🇯 Alpha-1"}
	nf.mu.Unlock()
	post(t, s, "/api/subscription/update", ``, "")
	s.jobs.Wait(3 * time.Second)
	if len(s.manifestEntries()) != 2 {
		t.Fatalf("перед удалением записей %d", len(s.manifestEntries()))
	}

	s.subUpdater.FetchWith = func(context.Context, string, happ.FetchOpts) ([]byte, error) {
		return nil, errors.New("удаление не должно ходить в сеть")
	}
	nf.mu.Lock()
	nf.providerNames = []string{"🇧🇧 Браво"}
	nf.mu.Unlock()
	rec := delSub(t, s, "/api/subscriptions/"+id)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	s.jobs.Wait(3 * time.Second)
	if es := s.manifestEntries(); len(es) != 1 || es[0].Name != "🇧🇧 Браво" {
		t.Errorf("после удаления: %+v", es)
	}
	if !hasCall(f.Calls, "delete netmode."+id) {
		t.Errorf("секция не удалена: %v", f.Calls)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.SubsCacheDir, id+".json")); !os.IsNotExist(err) {
		t.Errorf("кэш удалённой подписки остался: %v", err)
	}
	if l, _ := listSubs(t, s); len(l.Sources) != 1 {
		t.Errorf("в списке осталось %d", len(l.Sources))
	}
	if rec := delSub(t, s, "/api/subscriptions/main"); errCode(t, rec) != "main_subscription" {
		t.Errorf("основная: %d", rec.Code)
	}
	if rec := delSub(t, s, "/api/subscriptions/nosuch"); errCode(t, rec) != "unknown_subscription" {
		t.Errorf("чужой id: %d", rec.Code)
	}
}

func TestParseSubSources(t *testing.T) {
	raw := []byte("netmode.main=netmode\nnetmode.main.mode='nikki'\nnetmode.main.hwid='" + testSubHWID + "'\n" +
		"netmode.second=subscription\nnetmode.second.label='Вторая'\nnetmode.second.url='" + secondURL + "'\nnetmode.second.hwid='1'\n" +
		"netmode.other=subscription\nnetmode.other.label='Третья'\nnetmode.other.url='https://x.example.invalid/a?t=1'\n")
	extra, hwid := ParseSubSources(raw)
	if hwid != testSubHWID || len(extra) != 2 {
		t.Fatalf("hwid %d знаков, подписок %d", len(hwid), len(extra))
	}
	if extra[0].ID != "second" || extra[0].Label != "Вторая" || extra[0].URL != secondURL || !extra[0].HWID || extra[1].HWID {
		t.Errorf("разбор: %+v", extra)
	}
	if e, h := ParseSubSources(nil); e != nil || h != "" {
		t.Error("пустой вывод обязан дать пустой список")
	}
}

func TestMaskSecretPath(t *testing.T) {
	cases := map[string]string{
		secondURL:                                 "https://second.example.invalid/…",
		"https://h.example.invalid/a/b/SEC":       "https://h.example.invalid/a/b/…",
		"https://h.example.invalid/sub?token=SEC": "https://h.example.invalid/sub?token=…",
		"": "",
	}
	for in, want := range cases {
		if got := maskSecretPath(in); got != want {
			t.Errorf("%q → %q, ждали %q", in, got, want)
		}
	}
}
