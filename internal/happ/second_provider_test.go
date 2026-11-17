package happ

import (
	"os"
	"testing"
)

// Второй провайдер (17.11.2026): панель типа Remnawave, формат тот же —
// массив Xray-конфигов. Фикстура обезличена по правилам docs/recon/README.md;
// длинные списки доменов и подсетей в dns и routing обрезаны до четырёх
// записей — конвертер их не читает.
func secondFixture(t *testing.T) []Entry {
	t.Helper()
	raw, err := os.ReadFile("testdata/subscription-second.json")
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	es, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return es
}

func TestSecondProviderParses(t *testing.T) {
	es := secondFixture(t)
	if len(es) != 14 {
		t.Fatalf("записей %d, ждали 14", len(es))
	}
	kinds := map[Kind]int{}
	types := map[string]int{}
	for _, e := range es {
		kinds[e.Kind]++
		types[e.Type]++
		if e.Kind == KindUnsupported {
			t.Errorf("%q не стал узлом: %s", e.Name, e.Reason)
		}
		if e.Kind == KindNode && len(e.Outbound) == 0 {
			t.Errorf("%q без outbound для подписки телефонов", e.Name)
		}
	}
	if kinds[KindNode] != 13 || kinds[KindAuto] != 1 {
		t.Errorf("виды: %v", kinds)
	}
	if types["vless"] != 12 || types["trojan"] != 1 {
		t.Errorf("типы: %v", types)
	}
	// Балансировщик «самый быстрый» собран из тех же серверов, что идут
	// отдельными записями: его пул обязан разрешиться в их имена.
	auto := es[0]
	if auto.Kind != KindAuto || len(auto.Pool) == 0 {
		t.Fatalf("первая запись: kind=%s pool=%v", auto.Kind, auto.Pool)
	}
	names := map[string]bool{}
	for _, e := range es {
		if e.Kind == KindNode {
			names[e.Name] = true
		}
	}
	for _, n := range auto.Pool {
		if !names[n] {
			t.Errorf("в пуле имя %q, которого нет среди узлов", n)
		}
	}
}
