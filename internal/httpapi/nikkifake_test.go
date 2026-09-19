package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"netmoded/internal/nikki"
)

// fakeNikkiClient повторяет раскладку с живого роутера: PROXY типа
// URLTest, то есть ручной выбор невозможен.
type fakeNikkiClient struct {
	// snapshots — очередь ответов Connections, последний повторяется.
	snapshots     []nikki.Snapshot
	snapshotErr   error
	snapshotCalls int
	// logLines — что отдаёт LogStream. streamCalls считает переподключения:
	// «поток переоткрыли» и «поток не рвали» по одному результату
	// неразличимы.
	logLines    []nikki.LogLine
	streamErr   error
	streamCalls int

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
	// ruleProvidersCalls — сколько раз движок спрашивали. Ожидание докачки
	// иначе непроверяемо: «дождались второго ответа» и «повезло с первого»
	// по одному результату неразличимы.
	ruleProvidersCalls int
	// onRuleProviders — крючок, который зовётся ПОД замком перед ответом, с
	// номером вызова (с единицы). Им тест изображает mihomo, который поднял
	// Clash API раньше, чем докачал .mrs: правка ruleProviders прямо
	// отсюда безопасна и видна следующему ответу.
	onRuleProviders func(n int)
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
	// У Selector поля fixed нет вовсе — проверено на живом движке
	// (docs/recon/raw/93-autopool-spike.txt). Подделка обязана это
	// повторять: иначе тест на признак закрепления прошёл бы на
	// выдуманном поле, которого в ответе mihomo не бывает.
	g.Now = member
	if g.Type != "Selector" {
		g.Fixed, g.Pinned = member, true
	}
	f.all[group] = g
	return nil
}

// newFakeNikkiSelectorClient — раскладка нового профиля: PROXY это
// Selector, в участниках которого группа AUTO и узлы подписки, а сама
// AUTO — url-test над суженным пулом.
func newFakeNikkiSelectorClient() *fakeNikkiClient {
	d := func(v int) *int { return &v }
	return &fakeNikkiClient{all: map[string]nikki.Proxy{
		"PROXY": {Name: "PROXY", Type: "Selector", Alive: true,
			Members: []string{"AUTO", "🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2", "мёртвый"},
			Now:     "AUTO", Selectable: true},
		"AUTO": {Name: "AUTO", Type: "URLTest", Alive: true,
			Members: []string{"🇵🇱⚡Польша", "🇨🇭⚡Швейцария 2"},
			Now:     "🇨🇭⚡Швейцария 2", Selectable: true},
		// BYPASS в новую раскладку входит наравне с AUTO: наборы целятся
		// именно в него. Без него проверка «наборы применились» упёрлась
		// бы в group_missing раньше, чем дошла до сути.
		"BYPASS": {Name: "BYPASS", Type: "Fallback", Alive: true,
			Members: []string{"PROXY", "REJECT"}, Now: "PROXY", Selectable: true},
		"GLOBAL": {Name: "GLOBAL", Type: "Selector", Alive: true,
			Members: []string{"DIRECT", "PROXY"}, Now: "DIRECT", Selectable: true},
		"🇵🇱⚡Польша":      {Name: "🇵🇱⚡Польша", Type: "Vless", Alive: true, DelayMS: d(38)},
		"🇨🇭⚡Швейцария 2": {Name: "🇨🇭⚡Швейцария 2", Type: "Vless", Alive: true, DelayMS: d(27)},
		"мёртвый":        {Name: "мёртвый", Type: "Vless", Alive: false, DelayMS: nil},
	}}
}

// PanelAlive повторяет пробу /ui/: по умолчанию панель на месте.
func (f *fakeNikkiClient) PanelAlive(context.Context) error { return f.panelErr }

// Connections отдаёт снимки по очереди, а последний повторяет: сессия
// наблюдения берёт снимок каждую секунду, и сценарий «один тик и всё»
// проверял бы не её, а первую итерацию цикла. snapshotErr сильнее очереди —
// им изображается молчащий движок.
func (f *fakeNikkiClient) Connections(context.Context) (nikki.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	if f.snapshotErr != nil {
		return nikki.Snapshot{}, f.snapshotErr
	}
	if len(f.snapshots) == 0 {
		return nikki.Snapshot{At: time.Now()}, nil
	}
	i := f.snapshotCalls - 1
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	return f.snapshots[i], nil
}

// LogStream отдаёт поток поверх заранее записанных строк. Пустой logLines
// даёт поток, который сразу кончается чистым EOF: так выглядит движок,
// который поднял Clash API и пока молчит.
func (f *fakeNikkiClient) LogStream(context.Context, string) (*nikki.LogStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamCalls++
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	var buf bytes.Buffer
	for _, l := range f.logLines {
		b, _ := json.Marshal(l)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return nikki.NewLogStreamFromReader(io.NopCloser(&buf)), nil
}

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
	f.ruleProvidersCalls++
	if f.onRuleProviders != nil {
		f.onRuleProviders(f.ruleProvidersCalls)
	}
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

// ruleProviderCalls — счётчик под замком: читать поле напрямую значило бы
// гоняться с горутиной джоба.
func (f *fakeNikkiClient) ruleProviderCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ruleProvidersCalls
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
