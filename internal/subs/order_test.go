package subs

import (
	"strings"
	"testing"

	"netmoded/internal/happ"
	"netmoded/internal/nikki"
)

func d(v int) *int { return &v }

// liveFixture повторяет раскладку живого роутера в миниатюре: группа
// URLTest и её участники. Разделителей и «Авто» здесь нет намеренно —
// в Clash API их и не бывает, они существуют только в подписке.
func liveFixture() map[string]nikki.Proxy {
	return map[string]nikki.Proxy{
		"PROXY": {Name: "PROXY", Type: "URLTest", Alive: true, Selectable: true,
			Members: []string{"Польша", "Швейцария", "Германия"},
			Now:     "Швейцария"},
		"Польша":    {Name: "Польша", Type: "Vless", Alive: true, DelayMS: d(38)},
		"Швейцария": {Name: "Швейцария", Type: "Vless", Alive: true, DelayMS: d(27)},
		"Германия":  {Name: "Германия", Type: "Hysteria2", Alive: false, DelayMS: nil},
	}
}

func names(ms []Member) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func kinds(ms []Member) []happ.Kind {
	out := make([]happ.Kind, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Kind)
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Правило 1. Свежая установка: обновление ещё ни разу не проходило,
// манифеста нет. Список обязан быть ровно тем, что показывал mihomo, —
// иначе демон сломан у того, у кого подписка не качается.
func TestOrderWithoutManifestFallsBackToLive(t *testing.T) {
	for _, tt := range []struct {
		name     string
		manifest []happ.Entry
	}{
		{"nil", nil},
		{"пустой", []happ.Entry{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Order(liveFixture(), "PROXY", tt.manifest)

			want := []string{"Польша", "Швейцария", "Германия"}
			if !eqStrings(names(got), want) {
				t.Fatalf("порядок %v, ожидался %v", names(got), want)
			}
			for _, m := range got {
				if m.Kind != happ.KindNode {
					t.Errorf("%q без манифеста получил вид %q", m.Name, m.Kind)
				}
				if m.Reason != "" {
					t.Errorf("%q без манифеста получил причину %q", m.Name, m.Reason)
				}
			}
			// Живое состояние обязано доехать целиком, а не одним именем.
			if got[0].DelayMS == nil || *got[0].DelayMS != 38 || !got[0].Alive {
				t.Errorf("узел приехал без живого состояния: %+v", got[0])
			}
		})
	}
}

// Правило 2. Порядок и виды берутся из манифеста строка за строкой,
// а живое состояние подмешивается только к узлам.
func TestOrderFollowsManifest(t *testing.T) {
	manifest := []happ.Entry{
		{Name: "Авто | Лучший сервер", Kind: happ.KindAuto},
		{Name: "Германия", Kind: happ.KindNode, Type: "hysteria2"},
		{Name: "Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "⬇️ Обходы белых списков ⬇️", Kind: happ.KindSeparator},
		{Name: "Швейцария", Kind: happ.KindNode, Type: "vless"},
	}

	got := Order(liveFixture(), "PROXY", manifest)

	// Порядок манифеста, а НЕ порядок участников группы в mihomo:
	// там Польша шла первой, а Германия последней.
	want := []string{
		"Авто | Лучший сервер", "Германия", "Польша",
		"⬇️ Обходы белых списков ⬇️", "Швейцария",
	}
	if !eqStrings(names(got), want) {
		t.Fatalf("порядок %v, ожидался %v", names(got), want)
	}

	wantKinds := []happ.Kind{
		happ.KindAuto, happ.KindNode, happ.KindNode,
		happ.KindSeparator, happ.KindNode,
	}
	for i, k := range kinds(got) {
		if k != wantKinds[i] {
			t.Errorf("%q: вид %q, ожидался %q", got[i].Name, k, wantKinds[i])
		}
	}

	// Германия жива=false и без задержки, Польша — с задержкой: живое
	// состояние приехало из mihomo, а не из манифеста.
	if got[1].Alive || got[1].DelayMS != nil {
		t.Errorf("Германия: %+v", got[1])
	}
	if got[2].DelayMS == nil || *got[2].DelayMS != 38 {
		t.Errorf("Польша: %+v", got[2])
	}
}

// Правило 2, продолжение. У строк не-node пробы не было, поэтому Alive
// всегда false, а DelayMS — nil. Панель различает строки по kind, и
// «мёртвый разделитель» — это не состояние, а отсутствие состояния.
func TestNonNodeRowsCarryNoLiveState(t *testing.T) {
	live := liveFixture()
	// Ловушка: имя разделителя совпало с именем живого узла. Провайдер
	// делает ровно это — заголовок раздела побайтово копирует соседний
	// узел, — и подмешать сюда живое состояние значило бы показать
	// заголовок подключаемым.
	manifest := []happ.Entry{
		{Name: "Польша", Kind: happ.KindSeparator},
		{Name: "Авто | Лучший сервер", Kind: happ.KindAuto},
		{Name: "Гонконг", Kind: happ.KindUnsupported, Reason: "транспорт tuic не поддержан"},
	}

	got := Order(live, "PROXY", manifest)

	for _, m := range got[:3] {
		if m.Alive {
			t.Errorf("%q (%s) объявлен живым без пробы", m.Name, m.Kind)
		}
		if m.DelayMS != nil {
			t.Errorf("%q (%s) получил задержку %d", m.Name, m.Kind, *m.DelayMS)
		}
	}
	if got[0].Kind != happ.KindSeparator {
		t.Errorf("разделитель с именем живого узла стал %q", got[0].Kind)
	}
	// Причина из манифеста уезжает в панель как есть.
	if got[2].Reason != "транспорт tuic не поддержан" {
		t.Errorf("причина потеряна: %q", got[2].Reason)
	}
}

// Правило 2, главный случай. Узел записан в файл провайдера, а движок его
// не показывает. Строка обязана остаться видимой с причиной: выброшенная
// молча, она неотличима от узла, которого у провайдера больше нет, и
// владелец пойдёт искать пропажу в подписке вместо движка.
func TestNodeMissingInLiveStaysVisibleAsUnsupported(t *testing.T) {
	manifest := []happ.Entry{
		{Name: "Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "Гонконг", Kind: happ.KindNode, Type: "tuic"},
	}

	got := Order(liveFixture(), "PROXY", manifest)

	if len(got) != 4 { // две строки манифеста плюс два живых хвоста
		t.Fatalf("строк %d: %v", len(got), names(got))
	}
	hk := got[1]
	if hk.Name != "Гонконг" {
		t.Fatalf("пропавший узел выпал из списка: %v", names(got))
	}
	if hk.Kind != happ.KindUnsupported {
		t.Errorf("вид %q, ожидался unsupported", hk.Kind)
	}
	if hk.Alive || hk.DelayMS != nil {
		t.Errorf("пропавший узел получил живое состояние: %+v", hk)
	}
	// Причина обязана называть обе версии случившегося: движок не принял
	// узел или ещё не перечитал файл. Иначе владелец чинит не то.
	for _, want := range []string{"подписк", "mihomo", "провайдер"} {
		if !strings.Contains(hk.Reason, want) {
			t.Errorf("в причине %q нет %q", hk.Reason, want)
		}
	}
}

// Правило 3. Манифест отстал от файла провайдера — например, провайдера
// обновили мимо демона. Прятать реально доступный узел из-за устаревшего
// манифеста нельзя: список стал бы хуже, чем вовсе без манифеста.
func TestLiveMembersMissingFromManifestGoLast(t *testing.T) {
	manifest := []happ.Entry{
		{Name: "⬇️ Обходы ⬇️", Kind: happ.KindSeparator},
		{Name: "Швейцария", Kind: happ.KindNode, Type: "vless"},
	}

	got := Order(liveFixture(), "PROXY", manifest)

	// Хвост идёт в порядке mihomo (Польша, потом Германия), а не по алфавиту.
	want := []string{"⬇️ Обходы ⬇️", "Швейцария", "Польша", "Германия"}
	if !eqStrings(names(got), want) {
		t.Fatalf("порядок %v, ожидался %v", names(got), want)
	}
	for _, m := range got[2:] {
		if m.Kind != happ.KindNode {
			t.Errorf("дописанный %q получил вид %q", m.Name, m.Kind)
		}
		if m.Reason != "" {
			t.Errorf("дописанный %q получил причину %q", m.Name, m.Reason)
		}
	}
	if got[2].DelayMS == nil || *got[2].DelayMS != 38 {
		t.Errorf("дописанный узел приехал без живого состояния: %+v", got[2])
	}
}

// Правило 4. Провайдер повторяет одно имя в разных разделах. Одна строка
// на имя, первая по порядку: второй пункт с тем же именем ничего нового
// не выбирает, а выглядит как ещё один сервер.
func TestDuplicateNamesCollapseToFirst(t *testing.T) {
	manifest := []happ.Entry{
		{Name: "Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "⬇️ Обходы ⬇️", Kind: happ.KindSeparator},
		{Name: "Польша", Kind: happ.KindSeparator}, // копия под заголовком
		{Name: "Швейцария", Kind: happ.KindNode, Type: "vless"},
	}

	got := Order(liveFixture(), "PROXY", manifest)

	want := []string{"Польша", "⬇️ Обходы ⬇️", "Швейцария", "Германия"}
	if !eqStrings(names(got), want) {
		t.Fatalf("порядок %v, ожидался %v", names(got), want)
	}
	// Победила ПЕРВАЯ запись: узел, а не позднейший разделитель.
	if got[0].Kind != happ.KindNode {
		t.Errorf("дубликат перебил первую запись: вид %q", got[0].Kind)
	}
}

// Пустой live — движок только что поднялся и ещё ничего не отдал. Манифест
// при этом уже есть, и список обязан остаться видимым целиком: полностью
// пустая панель неотличима от «подписка не скачалась».
func TestEmptyLiveDoesNotPanic(t *testing.T) {
	manifest := []happ.Entry{
		{Name: "Авто | Лучший сервер", Kind: happ.KindAuto},
		{Name: "Польша", Kind: happ.KindNode, Type: "vless"},
	}

	for _, live := range []map[string]nikki.Proxy{nil, {}} {
		got := Order(live, "PROXY", manifest)
		if len(got) != 2 {
			t.Fatalf("строк %d: %v", len(got), names(got))
		}
		if got[0].Kind != happ.KindAuto {
			t.Errorf("«Авто» стал %q", got[0].Kind)
		}
		if got[1].Kind != happ.KindUnsupported || got[1].Reason == "" {
			t.Errorf("узел при пустом live: %+v", got[1])
		}
	}

	// Без манифеста тот же пустой live даёт nil, а не пустой список:
	// «группы нет» и «группа пуста» — разные вещи для панели.
	if got := Order(nil, "PROXY", nil); got != nil {
		t.Errorf("без манифеста и без группы вернулось %v", got)
	}
}

// Группы с таким именем в live нет: имя разошлось с профилем. Манифест при
// этом остаётся единственным источником списка, и дописывать в хвост нечего.
func TestUnknownGroupKeepsManifest(t *testing.T) {
	manifest := []happ.Entry{
		{Name: "Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "⬇️ Обходы ⬇️", Kind: happ.KindSeparator},
	}

	got := Order(liveFixture(), "НЕТТАКОЙ", manifest)

	want := []string{"Польша", "⬇️ Обходы ⬇️"}
	if !eqStrings(names(got), want) {
		t.Fatalf("порядок %v, ожидался %v", names(got), want)
	}
	// Группы нет — значит и членства в ней нет: узел, которого движок
	// знает, но который не входит в группу, показывать выбираемым нельзя,
	// Select по нему ответил бы 404. Строка остаётся, но непригодной.
	if got[0].Kind != happ.KindUnsupported || got[0].Reason != reasonNotInGroup {
		t.Errorf("узел вне группы должен быть unsupported с причиной о группе: %+v", got[0])
	}
	if got[0].DelayMS != nil {
		t.Errorf("непригодная строка не должна нести живое состояние: %+v", got[0])
	}

	if got := Order(liveFixture(), "НЕТТАКОЙ", nil); got != nil {
		t.Errorf("неизвестная группа без манифеста → %v, ожидался nil", got)
	}
}

// TestNodeOutsideGroupIsNotSelectable — узел есть в /proxies, но профиль
// движка не включил его в группу (filter). Такой узел нельзя показывать
// выбираемым: Select(PROXY, name) ответит 404, и владелец получит кнопку,
// которая не работает, без объяснения.
func TestNodeOutsideGroupIsNotSelectable(t *testing.T) {
	live := liveFixture()
	// Группа знает всех, кроме Польши; сама Польша при этом жива.
	g := live["PROXY"]
	var members []string
	for _, m := range g.Members {
		if m != "Польша" {
			members = append(members, m)
		}
	}
	g.Members = members
	live["PROXY"] = g

	manifest := []happ.Entry{
		{Name: "Польша", Kind: happ.KindNode, Type: "vless"},
		{Name: "Германия", Kind: happ.KindNode, Type: "vless"},
	}
	got := Order(live, "PROXY", manifest)
	// Третья строка — участник группы, которого нет в манифесте: он
	// честно уходит в хвост, это правило 3 и оно здесь не предмет.
	if len(got) < 2 {
		t.Fatalf("строк %d, ожидалось не меньше 2: %+v", len(got), got)
	}
	if got[0].Name != "Польша" || got[0].Kind != happ.KindUnsupported || got[0].Reason != reasonNotInGroup {
		t.Errorf("Польша вне группы должна быть unsupported с причиной о группе: %+v", got[0])
	}
	if got[1].Name != "Германия" || got[1].Kind != happ.KindNode {
		t.Errorf("Германия в группе должна остаться узлом: %+v", got[1])
	}
}
