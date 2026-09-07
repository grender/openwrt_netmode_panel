package httpapi

import (
	"context"
	"sync"

	"netmoded/internal/nikki"
)

// fakeNikkiClient повторяет раскладку с живого роутера: PROXY типа
// URLTest, то есть ручной выбор невозможен.
type fakeNikkiClient struct {
	// mu защищает all, delayed и reloaded: ProbeAll зовёт Delay из нескольких
	// горутин сразу, и без замка тест ловил бы не поведение обработчика,
	// а гонку в собственной подделке (go test -race).
	mu  sync.Mutex
	all map[string]nikki.Proxy
	err error
	// panelErr — чем отвечает проба статики дашборда. nil означает «/ui/
	// отдаёт 200», как на живом роутере (raw/73-nikki-ui-probe.txt).
	panelErr error
	// delayed — имена, по которым прошла проба, в порядке вызова. Нужен,
	// чтобы проверить, КОГО именно опрашивали: замер разделителей подписки
	// или самой группы иначе неотличим от правильного.
	delayed []string
	// reloaded — имена провайдеров, которые просили перечитать, в порядке
	// вызова. По одному только списку узлов «перезагрузку позвали» и «файл
	// записали и промолчали» неразличимы: подделка отдаёт всё тот же all.
	reloaded []string
	// reloadErr — чем отвечает перезагрузка провайдера. nil означает
	// 204 живого mihomo.
	reloadErr error
	// providerNames — что движок показывает в провайдере после
	// перечитывания. nil означает «все узлы из all, кроме групп» — то есть
	// движок, который честно прочитал наш файл.
	providerNames []string
	providerErr   error
	// ruleProviders — что движок отдаёт на RuleProviders. nil даёт пустую
	// карту: обработчику важно не содержимое по умолчанию, а то, что он
	// сделает с конкретно заданным набором (нулевой UpdatedAt и прочее).
	ruleProviders    map[string]nikki.RuleProvider
	ruleProvidersErr error
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

// Delay повторяет пробу задержки: живой узел отвечает, мёртвый — нет.
//
// Успех записывается в DelayMS, потому что настоящий замер обновляет history
// у mihomo, и следующее чтение /proxies отдаёт уже новое число. Без этого
// тест не отличил бы «замерили» от «сходили и выбросили».
func (f *fakeNikkiClient) Delay(_ context.Context, name string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.delayed = append(f.delayed, name)
	p, ok := f.all[name]
	if !ok {
		return 0, nikki.ErrNotFound
	}
	if !p.Alive {
		return 0, nikki.ErrProbeFailed
	}
	v := 11
	p.DelayMS = &v
	f.all[name] = p
	return v, nil
}

// ReloadProvider повторяет PUT /providers/proxies/{имя}: успех молчит.
//
// Списка провайдеров у подделки нет намеренно — обработчику важно не то,
// какие провайдеры бывают, а позвал ли он перезагрузку после записи файла
// и что с ней случилось; и то и другое задаётся reloadErr.
func (f *fakeNikkiClient) ProviderProxies(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	if f.providerNames != nil {
		return f.providerNames, nil
	}
	var names []string
	for name, p := range f.all {
		if len(p.Members) == 0 {
			names = append(names, name)
		}
	}
	return names, nil
}

// RuleProviders повторяет GET /providers/rules: f.err имеет приоритет над
// ruleProvidersErr, как и у прочих вызовов подделки, — недоступность самого
// движка обязана перекрывать более частную ошибку.
func (f *fakeNikkiClient) RuleProviders(context.Context) (map[string]nikki.RuleProvider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.ruleProvidersErr != nil {
		return nil, f.ruleProvidersErr
	}
	out := make(map[string]nikki.RuleProvider, len(f.ruleProviders))
	for name, p := range f.ruleProviders {
		out[name] = p
	}
	return out, nil
}

func (f *fakeNikkiClient) ReloadProvider(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloaded = append(f.reloaded, name)
	if f.reloadErr != nil {
		return f.reloadErr
	}
	return f.err
}

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
