package happ

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestRenderKeepsOnlyNodesInOrder: в файл провайдера едут только узлы, и
// ровно в порядке подписки. Порядок здесь существует в явном виде последний
// раз — mihomo вернёт узлы объектом и потеряет его.
func TestRenderKeepsOnlyNodesInOrder(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var doc struct {
		Proxies []map[string]any `json:"proxies"`
	}
	// Разбираем результат обратно: JSON здесь не случайность и не хак —
	// YAML 1.2 является надмножеством JSON, и mihomo примет этот файл под
	// именем .yaml ровно так же, как принимал вывод прежнего happ2clash.
	if err := json.Unmarshal(Render(entries), &doc); err != nil {
		t.Fatalf("результат Render не разбирается обратно: %v", err)
	}

	var want []string
	for _, e := range entries {
		if e.Kind == KindNode {
			want = append(want, e.Name)
		}
	}
	if len(doc.Proxies) != len(want) {
		t.Fatalf("узлов в файле %d, ожидалось %d", len(doc.Proxies), len(want))
	}
	for i, p := range doc.Proxies {
		if p["name"] != want[i] {
			t.Errorf("узел %d: %v, ожидался %q", i, p["name"], want[i])
		}
	}

	// Ни «Авто», ни разделителя, ни неподдержанных в файле быть не может:
	// движку про них знать нечего, они живут в манифесте и в панели.
	for _, p := range doc.Proxies {
		switch p["name"] {
		case "🇦🇶 🚀Авто | Быстрый узел ⚡⚡", "⬇️ Обходы белых списков ⬇️":
			t.Errorf("в файл провайдера попала служебная запись %v", p["name"])
		}
	}
}

// TestRenderShapeIsProviderFile проверяет форму файла: объект с единственным
// ключом proxies, а не голый массив.
func TestRenderShapeIsProviderFile(t *testing.T) {
	entries := []Entry{
		{Name: "🇧🇧Браво", Kind: KindNode, Type: "vless", Proxy: map[string]any{
			"name": "🇧🇧Браво", "type": "vless", "server": "203.0.113.1", "port": 443,
			"alpn": []string{"h2", "http/1.1"},
			"reality-opts": map[string]any{
				"public-key": "PK", "short-id": "aa",
			},
		}},
	}

	var doc map[string]any
	if err := json.Unmarshal(Render(entries), &doc); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc) != 1 {
		t.Fatalf("ключей в корне %d, ожидался один (proxies): %v", len(doc), doc)
	}
	proxies, ok := doc["proxies"].([]any)
	if !ok || len(proxies) != 1 {
		t.Fatalf("proxies отсутствуют или не массив: %v", doc["proxies"])
	}
	p := proxies[0].(map[string]any)
	if got := p["reality-opts"].(map[string]any)["public-key"]; got != "PK" {
		t.Errorf("вложенная карта потерялась: %v", p["reality-opts"])
	}
	if got := p["alpn"].([]any); len(got) != 2 || got[0] != "h2" {
		t.Errorf("список alpn потерялся: %v", p["alpn"])
	}
}

// TestRenderWithoutNodes: узлов нет — файл всё равно валидный документ с
// пустым списком, а не пустые байты. Пустой файл mihomo отверг бы, и отказ
// пришёл бы из движка вместо внятного места.
func TestRenderWithoutNodes(t *testing.T) {
	entries := []Entry{
		{Name: "🇦🇶 Авто", Kind: KindAuto},
		{Name: "⬇️ Заголовок ⬇️", Kind: KindSeparator},
		{Name: "🇧🇧Браво", Kind: KindUnsupported, Reason: "протокол vmess не переводится в узел mihomo"},
	}

	var doc struct {
		Proxies []map[string]any `json:"proxies"`
	}
	body := Render(entries)
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc.Proxies) != 0 {
		t.Errorf("узлов %d, ожидалось 0", len(doc.Proxies))
	}
	if doc.Proxies == nil {
		t.Error("proxies должен быть пустым списком, а не null: null mihomo не примет")
	}
}

// TestRenderPanicsOnManifestEntries — записи из манифеста в Render не идут.
//
// У них Proxy == nil (json:"-"), и молчаливый пропуск дал бы файл с меньшим
// числом узлов, чем насчитал Summarize, а в худшем случае — пустой
// {"proxies":[]}, который движок примет. Паника здесь — тот же выбор, что у
// соседней ветки про несериализуемый узел.
func TestRenderPanicsOnManifestEntries(t *testing.T) {
	entries := []Entry{{Name: "🇧🇧⚡Браво", Kind: KindNode, Type: "vless"}}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Render молча принял узел без объекта")
		}
		if !strings.Contains(fmt.Sprint(r), "манифест") {
			t.Fatalf("паника не называет причину: %v", r)
		}
	}()
	Render(entries)
}
