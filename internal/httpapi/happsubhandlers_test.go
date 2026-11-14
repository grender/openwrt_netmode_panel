package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"netmoded/internal/happ"
	"netmoded/internal/nikki"
	"os"
	"strings"
	"testing"

	"netmoded/internal/mixin"
)

func getSub(t *testing.T, s *Server, path, ua string, panelToken bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("User-Agent", ua)
	if panelToken {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func rotateSub(t *testing.T, s *Server) string {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/sub/rotate", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	return subLink(t, s)
}

func subLink(t *testing.T, s *Server) string {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/sub/link", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		URL *string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.URL == nil {
		return ""
	}
	return *got.URL
}

func pathOf(url string) string { return url[strings.Index(url, "/sub/"):] }

// Подписки нет, пока владелец её не выпустил: ни ссылки, ни ответа.
func TestSubNoTokenYet(t *testing.T) {
	s, _ := newServer(t)
	if u := subLink(t, s); u != "" {
		t.Fatalf("ссылка до выпуска: %q", u)
	}
	if rec := getSub(t, s, "/sub/happ/anything", "Happ/4.8.0", false); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Fatalf("без токена: %d %q", rec.Code, rec.Body.String())
	}
}

// Свой токен в пути, без токена панели: Happ умеет только адрес. Неверный —
// пустой 404, ничем не отличимый от «такого пути нет».
func TestSubServesWithOwnToken(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	url := rotateSub(t, s)
	if !strings.HasPrefix(url, "http://192.168.9.1:8088/sub/happ/") {
		t.Fatalf("ссылка %q", url)
	}

	rec := getSub(t, s, pathOf(url), "Happ/4.8.0/Android", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var cs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cs); err != nil {
		t.Fatalf("не JSON-массив: %v", err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c["remarks"].(string))
	}
	if !strings.Contains(strings.Join(names, "|"), "kz-nqz-exit-01 · dc · прямо") {
		t.Errorf("записи %v", names)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("подписка кэшируется")
	}
	title := rec.Header().Get("profile-title")
	if !strings.HasPrefix(title, "base64:") {
		t.Errorf("profile-title %q", title)
	} else if b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(title, "base64:")); len(b) == 0 {
		t.Errorf("profile-title не base64: %q", title)
	}

	for _, bad := range []string{"/sub/happ/x" + pathOf(url)[len("/sub/happ/"):], "/sub/happ/" + testToken, "/sub/happ/"} {
		if rec := getSub(t, s, bad, "Happ", false); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Errorf("%s: %d %q", bad, rec.Code, rec.Body.String())
		}
	}
	// Токен панели обходом не служит: путь подписки сверяет только свой.
	if rec := getSub(t, s, "/sub/happ/wrong", "Happ", true); rec.Code != http.StatusNotFound {
		t.Errorf("с токеном панели и чужим токеном подписки: %d", rec.Code)
	}
}

// Хопы — из пула миксина и outbounds.json: записи через хоп появляются
// только для узлов, у которых есть исходный outbound.
func TestSubIncludesHopsFromPool(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	writeMixin(t, s, mixin.Render(mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops:    mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇭🇳⚡Отель", "🇦🇬💳Альфа"}, Pool: []string{"🇭🇳⚡Отель", "🇦🇬💳Альфа"}},
		SkipSNI: []string{"krisha.kz"}}))
	ob := `{"🇭🇳⚡Отель":{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"203.0.113.10","port":443,"users":[{"id":"00000000-0000-0000-0000-000000000001","encryption":"none"}]}]}}}`
	if err := os.WriteFile(s.cfg.OutboundsPath, []byte(ob), 0o600); err != nil {
		t.Fatal(err)
	}
	url := rotateSub(t, s)
	rec := getSub(t, s, pathOf(url), "Happ", false)
	body := rec.Body.String()
	if !strings.Contains(body, "kz-nqz-exit-01 · dc ⇢ 🇭🇳⚡Отель") || !strings.Contains(body, "Умный ⇢ хоп") {
		t.Errorf("нет записей через хоп: %s", body[:min(len(body), 400)])
	}
	if strings.Contains(body, "⇢ 🇦🇬💳Альфа") {
		t.Error("хоп без outbound попал в подписку")
	}
}

// Перевыпуск: старая ссылка перестаёт работать сразу.
func TestSubRotateRevokes(t *testing.T) {
	s, _ := newServer(t)
	old := rotateSub(t, s)
	fresh := rotateSub(t, s)
	if old == fresh {
		t.Fatal("токен не сменился")
	}
	if rec := getSub(t, s, pathOf(old), "Happ", false); rec.Code != http.StatusNotFound {
		t.Errorf("старая ссылка: %d", rec.Code)
	}
	if rec := getSub(t, s, pathOf(fresh), "Happ", false); rec.Code != http.StatusOK {
		t.Errorf("новая ссылка: %d", rec.Code)
	}
	fi, err := os.Stat(s.cfg.SubTokenPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("файл токена: %v %v", err, fi)
	}
}

// Кто забирал подписку — видно в панели по User-Agent, без IP.
func TestSubLinkListsClients(t *testing.T) {
	s, _ := newServer(t)
	url := rotateSub(t, s)
	getSub(t, s, pathOf(url), "Happ/4.8.0/Android/123", false)
	rec := do(t, s, http.MethodGet, "/api/sub/link", true)
	var got struct {
		Clients []struct {
			UA string `json:"user_agent"`
			At string `json:"at"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Clients) != 1 || got.Clients[0].UA != "Happ/4.8.0/Android/123" || got.Clients[0].At == "" {
		t.Errorf("clients %+v", got.Clients)
	}
	if strings.Contains(rec.Body.String(), "192.0.2.") {
		t.Error("в ответе адрес клиента")
	}
}

// Туннель «Умного» — узлы авто-пула (как BYPASS на роутере), а не хопы:
// хоп с выходом в РФ годится до своей ноды, но не для обхода.
func TestSubTunnelFollowsAutopool(t *testing.T) {
	s, _ := newServer(t)
	seedManifest(t, s, threeNodes())
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	writeMixin(t, s, mixin.Render(mixin.Config{Policy: mixin.PolicyTunnel, Download: mixin.DownloadDirect,
		Auto: mixin.AutoConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇨🇻⚡Чарли 2"}},
		Hops: mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: []string{"🇭🇳⚡Отель"}, Pool: []string{"🇭🇳⚡Отель"}}}))
	ob := func(addr string) string {
		return `{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"` + addr + `","port":443,"users":[{"id":"00000000-0000-0000-0000-000000000001","encryption":"none"}]}]}}`
	}
	all := `{"🇭🇳⚡Отель":` + ob("203.0.113.1") + `,"🇨🇻⚡Чарли 2":` + ob("203.0.113.2") + `,"🇦🇬💳Альфа":` + ob("203.0.113.3") + `}`
	if err := os.WriteFile(s.cfg.OutboundsPath, []byte(all), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := getSub(t, s, pathOf(rotateSub(t, s)), "Happ", false)
	var cs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cs); err != nil {
		t.Fatal(err)
	}
	var tun []string
	for _, o := range cs[0]["outbounds"].([]any) {
		m := o.(map[string]any)
		if strings.HasPrefix(m["tag"].(string), "tun-") {
			addr := m["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["address"].(string)
			tun = append(tun, addr)
		}
	}
	if strings.Join(tun, ",") != "203.0.113.2" {
		t.Errorf("туннель «Умного» через %v, ждали только узел авто-пула 203.0.113.2", tun)
	}
}

// Пул хопов теперь до 40, а Xray повторителя не умеет: в подписку
// телефона идут только 3 лучших хопа по замерам цепочек dc на роутере —
// иначе 80+ записей и пробы resi через каждый хоп с каждого телефона.
func TestSubTopHopsByRouterRanking(t *testing.T) {
	s, _ := newServer(t)
	names := []string{"h1", "h2", "h3", "h4", "h5"}
	var entries []happ.Entry
	obs := map[string]any{}
	for i, n := range names {
		entries = append(entries, happ.Entry{Name: n, Kind: happ.KindNode, Type: "vless"})
		obs[n] = map[string]any{"tag": "proxy", "protocol": "vless", "settings": map[string]any{"vnext": []any{
			map[string]any{"address": fmt.Sprintf("203.0.113.%d", i+1), "port": 443,
				"users": []any{map[string]any{"id": "00000000-0000-0000-0000-000000000001", "encryption": "none"}}}}}}
	}
	seedManifest(t, s, entries)
	seedExport(t, s, "kz-nqz-exit-01", "krisha.kz")
	writeMixin(t, s, mixin.Render(mixin.Config{Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect,
		Hops:        mixin.HopConfig{Mode: mixin.AutoAllow, Nodes: names, Pool: names},
		FollowNodes: []string{"kz-nqz-exit-01"}}))
	b, _ := json.Marshal(obs)
	if err := os.WriteFile(s.cfg.OutboundsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f := nikkiFake(t, s)
	f.mu.Lock()
	for n, d := range map[string]int{"h2": 300, "h4": 100, "h5": 200, "h1": 900} {
		name := "kz-nqz-exit-01-dc ⇢ " + n
		dd := d
		f.all[name] = nikki.Proxy{Name: name, Alive: true, DelayMS: &dd, Provider: "exits"}
	}
	f.mu.Unlock()

	rec := getSub(t, s, pathOf(rotateSub(t, s)), "Happ", false)
	var cs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cs); err != nil {
		t.Fatal(err)
	}
	var viaHop []string
	for _, c := range cs {
		r := c["remarks"].(string)
		if strings.HasPrefix(r, "kz-nqz-exit-01 · dc ⇢ ") {
			viaHop = append(viaHop, strings.TrimPrefix(r, "kz-nqz-exit-01 · dc ⇢ "))
		}
	}
	if strings.Join(viaHop, ",") != "h4,h5,h2" {
		t.Fatalf("хопы в подписке %v, ждали 3 лучших по замеру: h4,h5,h2", viaHop)
	}
}
