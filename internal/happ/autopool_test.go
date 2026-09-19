package happ

import (
	"encoding/json"
	"strings"
	"testing"
)

// «Авто | Быстрый узел» у провайдера — балансировщик Xray поверх
// отобранных им серверов, а не узел. Раньше конвертер знал про него ровно
// одно: что балансировщик есть. Состав пула при этом выбрасывался, и
// режим «как в подписке» взять его было неоткуда.
func TestAutoEntryCarriesProviderPool(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var auto Entry
	names := map[string]bool{}
	for _, e := range entries {
		if e.Kind == KindAuto {
			auto = e
		}
		if e.Kind == KindNode {
			names[e.Name] = true
		}
	}
	if auto.Name == "" {
		t.Fatal("записи «Авто» в подписке не нашлось")
	}
	// В снятой подписке балансировщик собран из двенадцати серверов —
	// это меньше, чем узлов в списке: провайдер отбирает их сам.
	if len(auto.Pool) != 12 {
		t.Fatalf("в пуле %d имён, ожидалось 12: %v", len(auto.Pool), auto.Pool)
	}
	for _, n := range auto.Pool {
		if !names[n] {
			t.Errorf("в пуле имя %q, которого нет среди узлов подписки", n)
		}
	}
	// Пул — это имена узлов, а не теги outbound: тег провайдера меняется
	// вместе с адресом сервера, а фильтр mihomo работает по именам.
	for _, n := range auto.Pool {
		if strings.HasPrefix(n, "proxy-") {
			t.Errorf("в пул уехал тег outbound, а не имя узла: %q", n)
		}
	}
}

// Пул собирается ПОСЛЕ разведения одинаковых имён. Иначе он ссылался бы
// на имя, которого в файле провайдера уже нет: mihomo держит узлы
// объектом, и два «Дома» там схлопнулись бы в один.
func TestPoolFollowsDedupedNames(t *testing.T) {
	raw := twoHomesWithBalancer(t)

	entries, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var auto Entry
	for _, e := range entries {
		if e.Kind == KindAuto {
			auto = e
		}
	}
	if len(auto.Pool) != 1 {
		t.Fatalf("в пуле %d имён, ожидалось 1: %v", len(auto.Pool), auto.Pool)
	}
	if auto.Pool[0] != "Дом (2)" {
		t.Errorf("в пуле %q, ожидалось разведённое имя «Дом (2)»", auto.Pool[0])
	}
}

// Подписка без балансировщика — обычное дело у другого провайдера. Поле
// пула тогда пустое, и в манифест оно не пишется вовсе: старый манифест
// обязан читаться новым демоном, а новый — старым.
func TestNoBalancerLeavesPoolEmpty(t *testing.T) {
	entries, err := Parse(oneNode(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, e := range entries {
		if len(e.Pool) != 0 {
			t.Errorf("у записи %q без балансировщика появился пул %v", e.Name, e.Pool)
		}
	}

	b, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("сериализация: %v", err)
	}
	if strings.Contains(string(b), "pool") {
		t.Errorf("пустой пул уехал в манифест: %s", b)
	}
}

// twoHomesWithBalancer — два узла с ОДИНАКОВЫМ именем и балансировщик,
// который указывает на второй из них.
//
// Записи берутся из снятой подписки, а не сочиняются: форма узла у
// провайдера богаче, чем кажется (одного «адрес, порт, id» конвертеру
// мало), и сочинённая запись проверяла бы разбор выдуманного формата.
func twoHomesWithBalancer(t *testing.T) []byte {
	t.Helper()
	var records []map[string]any
	if err := json.Unmarshal(subscription(t), &records); err != nil {
		t.Fatalf("фикстура: %v", err)
	}

	nodes := make([]map[string]any, 0, 2)
	for _, r := range records {
		if _, busy := r["routing"].(map[string]any)["balancers"]; busy {
			continue // «Авто» и «обходы» — у них свой балансировщик
		}
		nodes = append(nodes, r)
		if len(nodes) == 2 {
			break
		}
	}
	if len(nodes) != 2 {
		t.Fatalf("в фикстуре не нашлось двух простых узлов")
	}
	nodes[0]["remarks"], nodes[1]["remarks"] = "Дом", "Дом"

	// Теги у авто-записи провайдера выглядят как
	// «proxy-31-56-150-7-direct» — ни «proxy», ни «proxy-wl*». Именно по
	// отсутствию полезного outbound запись и опознаётся как «Авто»,
	// поэтому копию тега надо переименовать: с тегом «proxy» она стала бы
	// обычным узлом.
	outs, err := cloneWithTag(nodes[1]["outbounds"], "proxy-second-direct")
	if err != nil {
		t.Fatalf("копия outbound: %v", err)
	}
	auto := map[string]any{
		"remarks":   "Авто",
		"routing":   map[string]any{"balancers": []any{map[string]any{"tag": "b"}}},
		"outbounds": outs,
	}
	b, err := json.Marshal([]any{auto, nodes[0], nodes[1]})
	if err != nil {
		t.Fatalf("сборка фикстуры: %v", err)
	}
	return b
}

// oneNode — подписка из одной обычной записи, без «Авто».
func oneNode(t *testing.T) []byte {
	t.Helper()
	var records []map[string]any
	if err := json.Unmarshal(subscription(t), &records); err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	for _, r := range records {
		if _, busy := r["routing"].(map[string]any)["balancers"]; busy {
			continue
		}
		b, err := json.Marshal([]any{r})
		if err != nil {
			t.Fatalf("сборка фикстуры: %v", err)
		}
		return b
	}
	t.Fatal("в фикстуре не нашлось простого узла")
	return nil
}

// cloneWithTag копирует список outbound-ов, переименовав тег у каждого,
// чей тег провайдер дал полезному узлу.
func cloneWithTag(v any, tag string) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var outs []map[string]any
	if err := json.Unmarshal(b, &outs); err != nil {
		return nil, err
	}
	for _, o := range outs {
		if t, _ := o["tag"].(string); t == "proxy" || strings.HasPrefix(t, "proxy-wl") {
			o["tag"] = tag
		}
	}
	return outs, nil
}
