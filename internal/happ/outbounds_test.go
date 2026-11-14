package happ

import (
	"encoding/json"
	"strings"
	"testing"
)

// У каждого узла остаётся ИСХОДНЫЙ outbound Xray из подписки: подписка для
// телефонов (internal/happsub) отдаёт хоп в том виде, в каком его прислал
// провайдер, а не обратный перевод из mihomo — перевод теряет поля, которых
// mihomo не знает.
func TestNodesKeepRawOutbound(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatal(err)
	}
	nodes := 0
	for _, e := range entries {
		if e.Kind != KindNode {
			if len(e.Outbound) != 0 {
				t.Errorf("%s (%s): outbound у записи, которая не узел", e.Name, e.Kind)
			}
			continue
		}
		nodes++
		var ob map[string]any
		if err := json.Unmarshal(e.Outbound, &ob); err != nil {
			t.Fatalf("%s: outbound не JSON: %v", e.Name, err)
		}
		tag, _ := ob["tag"].(string)
		if tag != "proxy" && !strings.HasPrefix(tag, "proxy-wl") {
			t.Errorf("%s: взят outbound %q, а не тот, из которого сделан узел", e.Name, tag)
		}
		if ob["protocol"] == nil || ob["settings"] == nil {
			t.Errorf("%s: outbound обрезан: %v", e.Name, ob)
		}
	}
	if nodes == 0 {
		t.Fatal("в фикстуре нет узлов")
	}
}

// Файл outbounds.json: имя узла → его outbound, только узлы. Имена — уже
// разведённые dedupeNames: по ним ищет подписка для телефонов.
func TestRenderOutbounds(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(RenderOutbounds(entries), &m); err != nil {
		t.Fatalf("RenderOutbounds не JSON-объект: %v", err)
	}
	want := 0
	for _, e := range entries {
		if e.Kind == KindNode {
			want++
			if string(m[e.Name]) != string(e.Outbound) {
				t.Errorf("%s: в файле не тот outbound", e.Name)
			}
		}
	}
	if len(m) != want {
		t.Errorf("в файле %d записей, узлов %d", len(m), want)
	}
}

// В манифест исходный outbound не попадает: манифест — порядок и виды, а
// не вторая копия серверов с секретами.
func TestManifestHasNoOutbound(t *testing.T) {
	entries, err := Parse(subscription(t))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entries)
	if strings.Contains(string(b), `"outbound"`) || strings.Contains(string(b), `"vnext"`) {
		t.Fatal("в манифест утёк outbound")
	}
}
