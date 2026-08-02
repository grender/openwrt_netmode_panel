package main

// Фикстурные клиенты Nikki и b4 для инструмента разработки.
//
// Живут здесь, а не в internal/{nikki,b4}: так они физически не попадают
// в бинарь для роутера. Обработчики при этом настоящие — подменяется
// только то, что на ноуте недостижимо.

import (
	"context"
	"sync"

	"netmoded/internal/b4"
	"netmoded/internal/nikki"
)

// devNikki повторяет раскладку живого роутера: PROXY типа URLTest с 29
// участниками, автовыбор по умолчанию.
type devNikki struct {
	mu  sync.Mutex
	all map[string]nikki.Proxy
}

func newDevNikki() *devNikki {
	d := func(v int) *int { return &v }
	nodes := []struct {
		name  string
		delay *int
	}{
		{"🇵🇱⚡Польша", d(38)},
		{"🇨🇭⚡Швейцария 2", d(27)},
		{"🇩🇪⚡Германия 1", d(44)},
		{"🇳🇱⚡Нидерланды 1", d(61)},
		{"🇺🇸США 1", d(148)},
		{"🇯🇵Япония", d(210)},
		{"⬇️ Обходы белых списков ⬇️", nil},
		{"🇹🇷💳Турция", nil}, // мёртвый: delay:0 у mihomo → null у нас
	}

	all := map[string]nikki.Proxy{}
	var members []string
	for _, n := range nodes {
		members = append(members, n.name)
		all[n.name] = nikki.Proxy{
			Name: n.name, Type: "Vless", Alive: n.delay != nil, DelayMS: n.delay,
		}
	}
	all["PROXY"] = nikki.Proxy{
		Name: "PROXY", Type: "URLTest", Alive: true,
		Members: members, Now: "🇨🇭⚡Швейцария 2", Selectable: true,
	}
	all["BYPASS"] = nikki.Proxy{
		Name: "BYPASS", Type: "Fallback", Alive: true,
		Members: []string{"PROXY", "REJECT"}, Now: "PROXY", Selectable: true,
	}
	return &devNikki{all: all}
}

func (f *devNikki) Version(context.Context) (string, error) { return "v1.19.27", nil }

func (f *devNikki) Proxies(context.Context) (map[string]nikki.Proxy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]nikki.Proxy, len(f.all))
	for k, v := range f.all {
		out[k] = v
	}
	return out, nil
}

func (f *devNikki) Select(_ context.Context, group, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.all[group]
	if !ok || !g.Selectable {
		return nikki.ErrNotFound
	}
	for _, m := range g.Members {
		if m == member {
			g.Now, g.Fixed, g.Pinned = member, member, true
			f.all[group] = g
			return nil
		}
	}
	return nikki.ErrNotFound
}

func (f *devNikki) Unfix(_ context.Context, group string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.all[group]
	if !ok {
		return nikki.ErrNotFound
	}
	if g.Type == "Selector" {
		return nikki.ErrNotSelectable
	}
	// Движок снова выбирает сам — берём узел с наименьшей задержкой.
	best, bestDelay := "", 1<<30
	for _, m := range g.Members {
		p := f.all[m]
		if p.DelayMS != nil && *p.DelayMS < bestDelay {
			best, bestDelay = m, *p.DelayMS
		}
	}
	g.Fixed, g.Pinned, g.Now = "", false, best
	f.all[group] = g
	return nil
}

// devB4 повторяет сеты живого роутера.
type devB4 struct {
	mu   sync.Mutex
	sets []b4.Set
}

func newDevB4() *devB4 {
	return &devB4{sets: []b4.Set{
		{ID: "909a6fb1-b9b1-4af1-8ee0-bdce82e3d8ff", Name: "workki", Enabled: false},
		{ID: "d98efbc7-96b6-401c-90c4-744f3ec3ccbd", Name: "HomeSet", Enabled: true},
	}}
}

func (f *devB4) Version(context.Context) (b4.Version, error) {
	return b4.Version{Version: "1.74.1", Commit: "99808ed"}, nil
}

func (f *devB4) Sets(context.Context) ([]b4.Set, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]b4.Set(nil), f.sets...), nil
}

func (f *devB4) SelectOnly(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for i := range f.sets {
		on := f.sets[i].ID == id
		f.sets[i].Enabled = on
		if on {
			found = true
		}
	}
	if !found {
		return b4.ErrNotFound
	}
	return nil
}
