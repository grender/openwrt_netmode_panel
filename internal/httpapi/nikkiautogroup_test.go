package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"netmoded/internal/mixin"
)

// Тело ответа /api/nikki/proxies в объёме, который проверяет авто-группа.
type autoBody struct {
	Selected string `json:"selected"`
	Fixed    string `json:"fixed"`
	Pinned   bool   `json:"pinned"`
	AutoPool *int   `json:"auto_pool"`
	Members  []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"members"`
}

func readAuto(t *testing.T, body []byte) autoBody {
	t.Helper()
	var got autoBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("разбор тела: %v\n%s", err, body)
	}
	return got
}

// Профиль владельца демону не принадлежит (ADR-0002), значит обе
// раскладки обязаны работать: и старая (PROXY — url-test, «Авто» это
// снятие закрепления), и новая (PROXY — Selector с участником AUTO,
// «Авто» это выбор участника). Живой движок на DELETE у селектора
// отвечает 400 (raw/93), так что перепутать пути нельзя.
func TestAutoUsesSelectOnSelectorProfile(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiSelectorClient()
	s.SetNikkiClient(fake)

	// Сначала закрепим узел, чтобы возврат к «Авто» было видно.
	if rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("закрепление: код %d: %s", rec.Code, rec.Body.String())
	}
	rec := post(t, s, "/api/nikki/proxy", `{"name":"AUTO"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("возврат к авто: код %d: %s", rec.Code, rec.Body.String())
	}
	if now := fake.all[ProxyGroup].Now; now != mixin.AutoGroup {
		t.Errorf("после AUTO группа работает через %q, ожидалась %q", now, mixin.AutoGroup)
	}
}

// Старый профиль не сломан: там «Авто» по-прежнему снятие закрепления.
func TestAutoUsesUnfixOnURLTestProfile(t *testing.T) {
	s, _ := newServer(t)
	fake := newFakeNikkiClient()
	s.SetNikkiClient(fake)

	if rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("закрепление: код %d", rec.Code)
	}
	rec := post(t, s, "/api/nikki/proxy", `{"name":"AUTO"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("возврат к авто: код %d: %s", rec.Code, rec.Body.String())
	}
	if g := fake.all[ProxyGroup]; g.Fixed != "" || g.Pinned {
		t.Errorf("закрепление не снято: %+v", g)
	}
}

// У Selector поля fixed нет вовсе, поэтому «закреплено руками» и
// «работает авто» различаются только тем, на что группа указывает.
// Панель на этой разнице держит кнопку возврата, и вычислить её обязан
// демон, а не она.
func TestSelectorPinnedIsComputedFromNow(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())

	got := readAuto(t, do(t, s, http.MethodGet, "/api/nikki/proxies", true).Body.Bytes())
	if got.Pinned || got.Fixed != "" {
		t.Errorf("группа указывает на AUTO, а выглядит закреплённой: %+v", got)
	}

	rec := post(t, s, "/api/nikki/proxy", `{"name":"🇵🇱⚡Польша"}`, "")
	got = readAuto(t, rec.Body.Bytes())
	if !got.Pinned || got.Fixed != "🇵🇱⚡Польша" {
		t.Errorf("узел закреплён, а признака нет: %+v", got)
	}
}

// Пока работает авто, владельцу важно, ЧЕРЕЗ КАКОЙ узел. У селектора
// now — это «AUTO», то есть имя группы; отдать его как выбранный узел
// значило бы показать в панели «сейчас AUTO» вместо имени сервера.
func TestSelectorSelectedShowsNodeBehindAuto(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())

	got := readAuto(t, do(t, s, http.MethodGet, "/api/nikki/proxies", true).Body.Bytes())
	if got.Selected != "🇨🇭⚡Швейцария 2" {
		t.Errorf("selected = %q, ожидался узел, через который работает AUTO", got.Selected)
	}
}

// AUTO — группа из профиля, а не узел подписки. В списке узлов ей не
// место: панель рисует «Авто» отдельной строкой, и вторая такая же в
// общем ряду читалась бы как ещё один сервер.
func TestAutoGroupIsNotListedAmongNodes(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())

	got := readAuto(t, do(t, s, http.MethodGet, "/api/nikki/proxies", true).Body.Bytes())
	for _, m := range got.Members {
		if m.Name == mixin.AutoGroup {
			t.Errorf("группа AUTO попала в список узлов строкой вида %q", m.Kind)
		}
	}
	if len(got.Members) == 0 {
		t.Error("список узлов опустел вместе с AUTO")
	}
}

// Размер пула — единственное, по чему владелец заметит, что фильтр
// съел больше, чем он думал. Молчание здесь хуже неверного числа.
func TestAutoPoolSizeIsReported(t *testing.T) {
	s, _ := newServer(t)
	s.SetNikkiClient(newFakeNikkiSelectorClient())

	got := readAuto(t, do(t, s, http.MethodGet, "/api/nikki/proxies", true).Body.Bytes())
	if got.AutoPool == nil {
		t.Fatal("размер авто-пула не отдан")
	}
	if *got.AutoPool != 2 {
		t.Errorf("в пуле %d узлов, ожидалось 2", *got.AutoPool)
	}

	// На старом профиле группы AUTO нет, и число браться неоткуда:
	// ноль соврал бы про пустой пул, поэтому поля быть не должно.
	s2, _ := newServer(t)
	s2.SetNikkiClient(newFakeNikkiClient())
	if old := readAuto(t, do(t, s2, http.MethodGet, "/api/nikki/proxies", true).Body.Bytes()); old.AutoPool != nil {
		t.Errorf("на профиле без AUTO отдан размер пула %d", *old.AutoPool)
	}
}
