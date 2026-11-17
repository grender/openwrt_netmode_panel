package happ

import (
	"reflect"
	"testing"
)

func node(name string) Entry {
	return Entry{Name: name, Kind: KindNode, Type: "vless", Proxy: map[string]any{"name": name, "type": "vless"}}
}

func names(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}

func TestMergeKeepsNamesWithoutCollisions(t *testing.T) {
	a := []Entry{node("🇭🇳 Отель"), node("🇨🇻 Чарли")}
	b := []Entry{node("🇫🇯 Alpha-1")}
	got, renamed := Merge([]Source{{ID: MainSource, Label: "Основная", Entries: a}, {ID: "second", Label: "Вторая", Entries: b}})
	if !reflect.DeepEqual(names(got), []string{"🇭🇳 Отель", "🇨🇻 Чарли", "🇫🇯 Alpha-1"}) {
		t.Fatalf("имена %v", names(got))
	}
	if len(renamed) != 0 {
		t.Errorf("переименования без совпадений: %v", renamed)
	}
	if got[0].Source != "" || got[2].Source != "second" {
		t.Errorf("источники: %q %q", got[0].Source, got[2].Source)
	}
}

// Совпавшее имя получает хвост у ПОЗДНЕГО источника; пул его балансировщика
// переезжает вместе с именем, а чужой пул не трогается.
func TestMergeSuffixesLaterSourceAndRemapsItsPool(t *testing.T) {
	a := []Entry{
		{Name: "Авто", Kind: KindAuto, Pool: []string{"🇩🇪 Германия"}},
		node("🇩🇪 Германия"),
	}
	b := []Entry{
		{Name: "Авто", Kind: KindAuto, Pool: []string{"🇩🇪 Германия", "🇵🇱 Польша"}},
		node("🇩🇪 Германия"),
		node("🇵🇱 Польша"),
	}
	got, renamed := Merge([]Source{{ID: MainSource, Label: "Основная", Entries: a}, {ID: "second", Label: "Вторая", Entries: b}})
	want := []string{"Авто", "🇩🇪 Германия", "Авто · Вторая", "🇩🇪 Германия · Вторая", "🇵🇱 Польша"}
	if !reflect.DeepEqual(names(got), want) {
		t.Fatalf("имена %v", names(got))
	}
	if !reflect.DeepEqual(got[0].Pool, []string{"🇩🇪 Германия"}) {
		t.Errorf("пул первой подписки изменился: %v", got[0].Pool)
	}
	if !reflect.DeepEqual(got[2].Pool, []string{"🇩🇪 Германия · Вторая", "🇵🇱 Польша"}) {
		t.Errorf("пул второй подписки: %v", got[2].Pool)
	}
	if got[3].Proxy["name"] != "🇩🇪 Германия · Вторая" {
		t.Errorf("имя в узле для движка: %v", got[3].Proxy["name"])
	}
	if len(renamed) != 2 {
		t.Errorf("переименования: %v", renamed)
	}
	// Вход — кэш источника: слияние его не портит.
	if b[1].Name != "🇩🇪 Германия" || b[1].Proxy["name"] != "🇩🇪 Германия" || b[0].Pool[0] != "🇩🇪 Германия" {
		t.Error("Merge изменил входные записи")
	}
}

// Имя с хвостом само может оказаться занято — тогда номер.
func TestMergeSuffixCollision(t *testing.T) {
	a := []Entry{node("X"), node("X · Вторая")}
	b := []Entry{node("X")}
	got, _ := Merge([]Source{{ID: MainSource, Label: "Основная", Entries: a}, {ID: "second", Label: "Вторая", Entries: b}})
	if got[2].Name != "X · Вторая (2)" {
		t.Fatalf("имя %q", got[2].Name)
	}
}

// Настоящие ответы двух провайдеров: имена не пересекаются, всё на месте.
func TestMergeTwoRecordedSubscriptions(t *testing.T) {
	first, err := Parse(subscription(t))
	if err != nil {
		t.Fatal(err)
	}
	second := secondFixture(t)
	got, renamed := Merge([]Source{{ID: MainSource, Label: "Основная", Entries: first}, {ID: "second", Label: "Вторая", Entries: second}})
	if len(got) != len(first)+len(second) {
		t.Fatalf("записей %d, ждали %d", len(got), len(first)+len(second))
	}
	if len(renamed) != 0 {
		t.Errorf("переименования: %v", renamed)
	}
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.Name] {
			t.Errorf("дубль имени %q", e.Name)
		}
		seen[e.Name] = true
	}
	if Summarize(got).Nodes != Summarize(first).Nodes+Summarize(second).Nodes {
		t.Error("число узлов после слияния не равно сумме")
	}
}
