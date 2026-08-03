package httpapi

import (
	"context"

	"netmoded/internal/nikki"
)

// fakeNikkiClient повторяет раскладку с живого роутера: PROXY типа
// URLTest, то есть ручной выбор невозможен.
type fakeNikkiClient struct {
	all map[string]nikki.Proxy
	err error
	// panelErr — чем отвечает проба статики дашборда. nil означает «/ui/
	// отдаёт 200», как на живом роутере (raw/73-nikki-ui-probe.txt).
	panelErr error
}

func newFakeNikkiClient() *fakeNikkiClient {
	d := func(v int) *int { return &v }
	return &fakeNikkiClient{all: map[string]nikki.Proxy{
		"PROXY": {Name: "PROXY", Type: "URLTest", Alive: true,
			Members: []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2", "мёртвый"},
			Now:     "🇨🇭⚡Швейцария 2", Selectable: true},
		"GLOBAL": {Name: "GLOBAL", Type: "Selector", Alive: true,
			Members: []string{"DIRECT", "PROXY"}, Now: "DIRECT", Selectable: true},
		"🇵🇱⚡Польша":      {Name: "🇵🇱⚡Польша", Type: "Vless", Alive: true, DelayMS: d(38)},
		"🇨🇭⚡Швейцария 2": {Name: "🇨🇭⚡Швейцария 2", Type: "Vless", Alive: true, DelayMS: d(27)},
		"мёртвый":        {Name: "мёртвый", Type: "Vless", Alive: false, DelayMS: nil},
	}}
}

func (f *fakeNikkiClient) Version(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "v1.19.27", nil
}

func (f *fakeNikkiClient) Proxies(context.Context) (map[string]nikki.Proxy, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.all, nil
}

func (f *fakeNikkiClient) Select(_ context.Context, group, member string) error {
	if f.err != nil {
		return f.err
	}
	g, ok := f.all[group]
	if !ok {
		return nikki.ErrNotFound
	}
	if !g.Selectable {
		return nikki.ErrNotSelectable
	}
	found := false
	for _, m := range g.Members {
		if m == member {
			found = true
		}
	}
	if !found {
		return nikki.ErrNotFound
	}
	g.Now, g.Fixed, g.Pinned = member, member, true
	f.all[group] = g
	return nil
}

// PanelAlive повторяет пробу /ui/: по умолчанию панель на месте.
func (f *fakeNikkiClient) PanelAlive(context.Context) error { return f.panelErr }

func (f *fakeNikkiClient) Unfix(_ context.Context, group string) error {
	if f.err != nil {
		return f.err
	}
	g, ok := f.all[group]
	if !ok {
		return nikki.ErrNotFound
	}
	if g.Type == "Selector" {
		// У обычного Selector автовыбора нет — снимать нечего.
		return nikki.ErrNotSelectable
	}
	g.Fixed, g.Pinned = "", false
	g.Now = "🇨🇭⚡Швейцария 2" // движок снова выбирает сам
	f.all[group] = g
	return nil
}
