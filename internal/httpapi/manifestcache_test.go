package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"netmoded/internal/executor"
	"netmoded/internal/happ"
)

// twoNodes — подписка из двух узлов в порядке, ОБРАТНОМ порядку группы в
// подделке mihomo (там Отель первая). Иначе тест был бы зелёным и при
// полностью проигнорированном манифесте.
const twoNodes = `[
  {"remarks":"🇨🇻⚡Чарли 2","outbounds":[{"tag":"proxy","protocol":"vless",
    "settings":{"vnext":[{"address":"203.0.113.2","port":443,"users":[{"id":"00000000-0000-0000-0000-000000000002","flow":"xtls-rprx-vision"}]}]},
    "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"a.example","publicKey":"pk2","shortId":"02","fingerprint":"chrome"}}}]},
  {"remarks":"🇭🇳⚡Отель","outbounds":[{"tag":"proxy","protocol":"vless",
    "settings":{"vnext":[{"address":"203.0.113.1","port":443,"users":[{"id":"00000000-0000-0000-0000-000000000001","flow":"xtls-rprx-vision"}]}]},
    "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"b.example","publicKey":"pk1","shortId":"01","fingerprint":"chrome"}}}]}
]`

// liveServer собирает сервер БОЕВЫМ NewServer, не подменяя расписание:
// проверяется проводка «обновление → файлы → кэш манифеста → ответ панели»,
// которую подмена s.sched выкидывает целиком.
func liveServer(t *testing.T) *Server {
	t.Helper()
	f := executor.NewFake()
	f.LoadFixtures(t, filepath.Join("..", "..", "docs", "recon", "raw"))
	f.UCIValues["netmode.main.mode"] = "nikki"
	dir := t.TempDir()
	s, err := NewServer(Config{
		Listen:          "192.168.9.1",
		Port:            8088,
		Token:           testToken,
		NikkiURL:        testNikkiURL,
		NikkiSecret:     testNikkiSecret,
		LogPath:         filepath.Join(dir, "updates.log"),
		ProviderPath:    filepath.Join(dir, "sub.yaml"),
		ManifestPath:    filepath.Join(dir, "subscription.json"),
		SubscriptionURL: "https://example.invalid/sub/secret-id",
		SubscriptionFetch: func(context.Context, string) ([]byte, error) {
			return []byte(twoNodes), nil
		},
	}, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.SetB4Client(newFakeB4Client())
	s.SetNikkiClient(newFakeNikkiClient())
	return s
}

// TestSubscriptionUpdateRefreshesManifestCache — единственное место, где
// новый манифест попадает в кэш, — manifestRefresher. Без него панель до
// перезапуска показывала бы вчерашний порядок, а entryKind по протухшему
// кэшу отбивал бы рабочий узел кодом 409.
func TestSubscriptionUpdateRefreshesManifestCache(t *testing.T) {
	t.Run("успех", func(t *testing.T) {
		s := liveServer(t)
		if got := s.manifestEntries(); len(got) != 0 {
			t.Fatalf("до обновления кэш должен быть пуст: %+v", got)
		}

		sum, err := s.sched.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if sum.Nodes != 2 {
			t.Fatalf("узлов %d, ожидалось 2", sum.Nodes)
		}

		rec := do(t, s, "GET", "/api/nikki/proxies", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
		}
		rows := membersOf(t, rec)
		if len(rows) < 2 || rows[0].Name != "🇨🇻⚡Чарли 2" || rows[1].Name != "🇭🇳⚡Отель" {
			t.Fatalf("порядок пришёл не из манифеста: %+v", rows)
		}
		for _, r := range rows[:2] {
			if r.Kind != string(happ.KindNode) {
				t.Errorf("%s: вид %q, ожидался node", r.Name, r.Kind)
			}
		}
	})

	t.Run("отказ перечитывания движком", func(t *testing.T) {
		s := liveServer(t)
		// Клиента нет — Reload отказывает ПОСЛЕ записи файлов. Манифест на
		// диске уже новый, и кэш обязан обновиться безусловно: иначе он
		// врал бы про порядок до перезапуска демона.
		s.SetNikkiClient(nil)

		_, err := s.sched.RunOnce(context.Background())
		if err == nil || !strings.Contains(err.Error(), "записаны") {
			t.Fatalf("ожидался отказ с оговоркой «файлы записаны», получено %v", err)
		}
		if got := s.manifestEntries(); len(got) != 2 || got[0].Name != "🇨🇻⚡Чарли 2" {
			t.Fatalf("кэш не перечитан после частичного отказа: %+v", got)
		}
	})
}

// TestEntryKindFirstWins — правило «первое побеждает» в entryKind обязано
// совпадать с subs.Order: иначе панель покажет строку выбираемой, а POST
// ответит 409.
func TestEntryKindFirstWins(t *testing.T) {
	s, _ := newServer(t)
	withManifest(s, []happ.Entry{
		{Name: "🇭🇳⚡Отель", Kind: happ.KindNode, Type: "vless"},
		{Name: "🇭🇳⚡Отель", Kind: happ.KindSeparator},
	})

	rec := do(t, s, "GET", "/api/nikki/proxies", true)
	rows := membersOf(t, rec)
	seen := 0
	for _, r := range rows {
		if r.Name == "🇭🇳⚡Отель" {
			seen++
			if r.Kind != string(happ.KindNode) {
				t.Errorf("первая запись — узел, получен вид %q", r.Kind)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("строка должна быть ровно одна, получено %d", seen)
	}

	req := httptest.NewRequest("POST", "/api/nikki/proxy", strings.NewReader(`{"name":"🇭🇳⚡Отель"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("выбор узла: код %d, ожидался 200: %s", rec.Code, rec.Body.String())
	}
}
