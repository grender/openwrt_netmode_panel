package nikki

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// delayServer отвечает на /proxies/{имя}/delay формой, снятой с роутера
// (RQ-06): ровно одно поле delay.
type delayServer struct {
	mu     sync.Mutex
	lastQ  string
	lastP  string
	status int
	body   string
}

func (d *delayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.lastP, d.lastQ = r.URL.Path, r.URL.RawQuery
	st, body := d.status, d.body
	d.mu.Unlock()
	if st == 0 {
		st = http.StatusOK
	}
	if body == "" {
		body = `{"delay":25}`
	}
	w.WriteHeader(st)
	_, _ = w.Write([]byte(body))
}

func TestDelayReadsMeasuredShape(t *testing.T) {
	d := &delayServer{}
	srv := httptest.NewServer(d)
	defer srv.Close()

	got, err := New(srv.URL, "sec").Delay(context.Background(), "🇵🇱⚡Польша")
	if err != nil {
		t.Fatalf("Delay: %v", err)
	}
	if got != 25 {
		t.Errorf("задержка %d, в замере было 25", got)
	}
	// Имя узла едет в пути и экранируется: в подписке они с эмодзи и
	// пробелами. r.URL.Path у сервера уже раскодирован, поэтому сверяем
	// с исходным именем — важно, что оно доехало целым, а не что байты
	// совпали с конкретной кодировкой.
	if d.lastP != "/proxies/🇵🇱⚡Польша/delay" {
		t.Errorf("путь %q", d.lastP)
	}
	// Оба параметра обязательны: без url mihomo возьмёт свой по умолчанию,
	// без timeout — свой, и замер перестанет быть тем, что мы описали.
	if !strings.Contains(d.lastQ, "timeout=1500") || !strings.Contains(d.lastQ, "generate_204") {
		t.Errorf("строка запроса %q — нет timeout или url", d.lastQ)
	}
}

// Ноль в ответе — не «ноль миллисекунд», а несостоявшаяся проба: ровно то же
// значение, которое mihomo пишет в history мёртвому узлу.
func TestDelayZeroIsNotAMeasurement(t *testing.T) {
	d := &delayServer{body: `{"delay":0}`}
	srv := httptest.NewServer(d)
	defer srv.Close()

	if _, err := New(srv.URL, "sec").Delay(context.Background(), "мёртвый"); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("ошибка %v, ожидалась ErrProbeFailed", err)
	}
}

// Несуществующее имя движок отвергает — успеху можно верить, отдельная
// проверка существования узла не нужна (RQ-06).
func TestDelayUnknownNameIsAnError(t *testing.T) {
	d := &delayServer{status: http.StatusNotFound, body: `{"message":"Resource not found"}`}
	srv := httptest.NewServer(d)
	defer srv.Close()

	if _, err := New(srv.URL, "sec").Delay(context.Background(), "нет такого"); err == nil {
		t.Fatal("несуществующее имя принято как успешный замер")
	}
}

// probeClient — клиент только для ProbeAll: всё остальное не вызывается.
type probeClient struct {
	mu      sync.Mutex
	calls   []string
	dead    map[string]bool
	hold    time.Duration
	inFlyht int32
	peak    int32
}

func (p *probeClient) Version(context.Context) (string, error) { return "", nil }
func (p *probeClient) Proxies(context.Context) (map[string]Proxy, error) {
	return nil, nil
}
func (p *probeClient) Select(context.Context, string, string) error { return nil }
func (p *probeClient) Unfix(context.Context, string) error          { return nil }
func (p *probeClient) ReloadProvider(context.Context, string) error { return nil }
func (p *probeClient) ProviderProxies(context.Context, string) ([]string, error) {
	return nil, nil
}
func (p *probeClient) RuleProviders(context.Context) (map[string]RuleProvider, error) {
	return nil, nil
}
func (p *probeClient) PanelAlive(context.Context) error { return nil }
func (p *probeClient) Connections(context.Context) (Snapshot, error) {
	return Snapshot{}, ErrUnavailable
}
func (p *probeClient) LogStream(context.Context, string) (*LogStream, error) {
	return nil, ErrUnavailable
}

func (p *probeClient) Delay(ctx context.Context, name string) (int, error) {
	n := atomic.AddInt32(&p.inFlyht, 1)
	for {
		old := atomic.LoadInt32(&p.peak)
		if n <= old || atomic.CompareAndSwapInt32(&p.peak, old, n) {
			break
		}
	}
	defer atomic.AddInt32(&p.inFlyht, -1)

	if p.hold > 0 {
		select {
		case <-time.After(p.hold):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	p.mu.Lock()
	p.calls = append(p.calls, name)
	dead := p.dead[name]
	p.mu.Unlock()
	if dead {
		return 0, ErrProbeFailed
	}
	return 42, nil
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune('a' + i%26))
	}
	return out
}

// Мёртвый узел не обрывает замер остальных. Это и есть смысл кнопки:
// в подписке на два десятка серверов часть заведомо не отвечает.
func TestProbeAllSurvivesDeadNodes(t *testing.T) {
	c := &probeClient{dead: map[string]bool{"b": true, "d": true}}
	sum := ProbeAll(context.Background(), c, []string{"a", "b", "c", "d", "e"})

	if sum.Total != 5 || sum.Measured != 3 || sum.Failed != 2 || sum.Skipped != 0 {
		t.Fatalf("итог %+v, ожидалось 5/3/2/0", sum)
	}
	if len(c.calls) != 5 {
		t.Errorf("проб %d, ожидалось 5 — мёртвый узел оборвал остальные", len(c.calls))
	}
}

// Одновременных проб не больше probeParallel: каждая — настоящий запрос
// наружу через свой туннель.
func TestProbeAllKeepsConcurrencyBounded(t *testing.T) {
	c := &probeClient{hold: 20 * time.Millisecond}
	ProbeAll(context.Background(), c, names(20))

	if int(c.peak) > probeParallel {
		t.Errorf("одновременно шло %d проб при пределе %d", c.peak, probeParallel)
	}
	if c.peak < 2 {
		t.Errorf("пик %d — пробы шли по очереди, замер 26 узлов так не уложится", c.peak)
	}
}

// Кончившийся бюджет — это skipped, а не failed. Разница не косметическая:
// failed говорит владельцу «узел мёртв», и записать туда собственный таймаут
// значило бы оболгать исправный сервер.
func TestProbeAllCountsBudgetAsSkippedNotFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &probeClient{}
	sum := ProbeAll(ctx, c, []string{"a", "b", "c"})
	if sum.Skipped != 3 || sum.Failed != 0 || sum.Measured != 0 {
		t.Fatalf("итог %+v, ожидалось 0 измеренных и 3 не успевших", sum)
	}
	if len(c.calls) != 0 {
		t.Errorf("при истёкшем бюджете сделано %d проб", len(c.calls))
	}
}

func TestProbeAllOnEmptyListDoesNothing(t *testing.T) {
	c := &probeClient{}
	sum := ProbeAll(context.Background(), c, nil)
	if sum != (ProbeSummary{}) {
		t.Fatalf("итог %+v, ожидался пустой", sum)
	}
}
