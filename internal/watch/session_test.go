package watch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"netmoded/internal/nikki"
)

// fakeSource — движок под управлением теста.
type fakeSource struct {
	mu sync.Mutex
	// snaps — очередь снимков; последний повторяется, потому что снимок
	// берётся каждую секунду, а сценарий «один тик и всё» проверял бы
	// первую итерацию, а не цикл.
	snaps    []nikki.Snapshot
	snapErr  error
	snapN    int
	lines    []string
	streamN  int
	streamer func(n int) (io.ReadCloser, error)
}

func (f *fakeSource) Connections(context.Context) (nikki.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapN++
	if f.snapErr != nil {
		return nikki.Snapshot{}, f.snapErr
	}
	if len(f.snaps) == 0 {
		return nikki.Snapshot{}, nil
	}
	i := f.snapN - 1
	if i >= len(f.snaps) {
		i = len(f.snaps) - 1
	}
	return f.snaps[i], nil
}

func (f *fakeSource) LogStream(context.Context, string) (*nikki.LogStream, error) {
	f.mu.Lock()
	n := f.streamN
	f.streamN++
	st := f.streamer
	lines := f.lines
	f.mu.Unlock()

	if st != nil {
		rc, err := st(n)
		if err != nil {
			return nil, err
		}
		return nikki.NewLogStreamFromReader(rc), nil
	}
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(nikki.LogLine{Type: "info", Payload: l})
		b.Write(j)
		b.WriteByte('\n')
	}
	return nikki.NewLogStreamFromReader(io.NopCloser(strings.NewReader(b.String()))), nil
}

func (f *fakeSource) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamN
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	f.snapErr = err
	f.mu.Unlock()
}

// newSession заводит сессию с ручными часами и без горутин: tick и feed
// зовёт сам тест. Иначе TTL, потолок и переподключение проверялись бы
// ожиданием настоящего времени — медленно и капризно под -race.
func newSession(src Source, now *time.Time, mode func(context.Context) (string, error)) *Session {
	cfg := Config{IP: devIP, Src: src, Mode: mode, Now: func() time.Time { return *now }}
	s := &Session{
		cfg: cfg, tb: newTable(),
		since: *now, lastPoll: *now, lastTick: *now,
		engine: EngineRunning,
		done:   make(chan struct{}), reconnect: make(chan struct{}, 1),
	}
	s.cancel = func() {}
	return s
}

// TestTTLStopsSessionWithoutPoll: не смотрят — через минуту погасло.
//
// Это и есть вся цена исключения из ADR-0017: поток к движку существует,
// только пока кто-то смотрит.
func TestTTLStopsSessionWithoutPoll(t *testing.T) {
	now := t0
	s := newSession(&fakeSource{}, &now, nil)

	now = t0.Add(idleTTL - time.Second)
	if !s.tick(context.Background(), now) {
		t.Fatal("сессия погасла раньше TTL")
	}
	now = t0.Add(idleTTL + time.Second)
	if s.tick(context.Background(), now) {
		t.Fatal("сессия пережила TTL без единого запроса панели")
	}
}

// TestPollRefreshesTTLAndStateDoesNot — два метода, и разница между ними
// содержательная.
func TestPollRefreshesTTLAndStateDoesNot(t *testing.T) {
	now := t0
	s := newSession(&fakeSource{}, &now, nil)

	now = t0.Add(idleTTL - time.Second)
	_ = s.Poll()
	now = t0.Add(2 * idleTTL)
	if s.tick(context.Background(), now) {
		t.Fatal("после Poll в середине окна сессия обязана погаснуть через своё TTL от Poll")
	}

	now = t0
	s2 := newSession(&fakeSource{}, &now, nil)
	now = t0.Add(idleTTL - time.Second)
	_ = s2.State()
	now = t0.Add(idleTTL + time.Second)
	if s2.tick(context.Background(), now) {
		t.Fatal("State продлил TTL: сводка на главной держала бы сессию вечно")
	}
}

// TestAbsoluteCap — вкладка, забытая на ночь, не держит поток до утра.
func TestAbsoluteCap(t *testing.T) {
	now := t0
	s := newSession(&fakeSource{}, &now, nil)
	// Панель исправно опрашивает всё это время.
	for at := time.Duration(0); at < sessionMax+time.Minute; at += 30 * time.Second {
		now = t0.Add(at)
		_ = s.Poll()
		if !s.tick(context.Background(), now) {
			if at <= sessionMax {
				t.Fatalf("сессия погасла на %v, потолок %v", at, sessionMax)
			}
			return
		}
	}
	t.Fatal("сессия пережила абсолютный потолок")
}

// TestSingleSnapshotFailureIsNotRestart — один промах не повод пугать
// владельца.
func TestSingleSnapshotFailureIsNotRestart(t *testing.T) {
	now := t0
	src := &fakeSource{snaps: []nikki.Snapshot{snap(conn("a", "x.example.com", "1.2.3.4", 443, 10, 20, t0))}}
	s := newSession(src, &now, nil)
	s.tick(context.Background(), now)

	src.setErr(errors.New("нет связи"))
	now = t0.Add(time.Second)
	s.tick(context.Background(), now)
	if got := s.State().Engine; got != EngineRunning {
		t.Errorf("после одного отказа движок = %q, ожидался %q", got, EngineRunning)
	}

	now = t0.Add(2 * time.Second)
	s.tick(context.Background(), now)
	if got := s.State().Engine; got != EngineRestarting {
		t.Errorf("после двух отказов подряд движок = %q, ожидался %q", got, EngineRestarting)
	}
}

// TestRestartKeepsTargetsZeroesLive — строки на месте, живых нет.
func TestRestartKeepsTargetsZeroesLive(t *testing.T) {
	now := t0
	// Первый снимок пустой: он задаёт точку отсчёта, а байты второго —
	// байты сессии целиком.
	src := &fakeSource{snaps: []nikki.Snapshot{snap(), snap(conn("a", "x.example.com", "1.2.3.4", 443, 500, 900, t0))}}
	s := newSession(src, &now, nil)
	s.tick(context.Background(), now)
	now = t0.Add(500 * time.Millisecond)
	s.tick(context.Background(), now)

	src.setErr(errors.New("нет связи"))
	for i := 1; i <= snapshotFailsForRestart; i++ {
		now = t0.Add(time.Duration(i) * time.Second)
		s.tick(context.Background(), now)
	}

	st := s.State()
	if st.Engine != EngineRestarting {
		t.Fatalf("движок = %q", st.Engine)
	}
	if len(st.Targets) != 1 {
		t.Fatalf("адресатов %d, они обязаны пережить перезапуск", len(st.Targets))
	}
	if st.Targets[0].Up != 500 || st.Targets[0].Down != 900 {
		t.Errorf("счётчики = ↑%d ↓%d, ожидались прежние", st.Targets[0].Up, st.Targets[0].Down)
	}
	if st.Targets[0].Live != 0 {
		t.Errorf("живых = %d", st.Targets[0].Live)
	}
	select {
	case <-s.reconnect:
	default:
		t.Error("поток не попросили переоткрыть: если тело /logs повисло, оно так и повиснет")
	}
}

// TestShrinkingUploadTotalIsRestart — уменьшение счётчика движка
// доказательно и действует сразу, без всякой отсрочки.
func TestShrinkingUploadTotalIsRestart(t *testing.T) {
	now := t0
	src := &fakeSource{snaps: []nikki.Snapshot{
		{UploadTotal: 9866090047, Connections: []nikki.Conn{conn("a", "x.example.com", "1.2.3.4", 443, 10, 20, t0)}},
		{UploadTotal: 4096, Connections: nil},
	}}
	s := newSession(src, &now, nil)
	s.tick(context.Background(), now)
	now = t0.Add(time.Second)
	s.tick(context.Background(), now)

	select {
	case <-s.reconnect:
	default:
		t.Error("поток не переоткрыли после перезапуска движка")
	}
	if len(s.State().Targets) != 1 {
		t.Error("адресаты не пережили перезапуск")
	}
}

// TestEngineOffWhenModeChanged — «перезапускается» и «выключен» это разные
// ответы на «почему пусто», и лечатся они по-разному.
func TestEngineOffWhenModeChanged(t *testing.T) {
	now := t0
	src := &fakeSource{snapErr: errors.New("нет связи")}
	var modeCalls int
	mode := func(context.Context) (string, error) {
		modeCalls++
		return "off", nil
	}
	s := newSession(src, &now, mode)
	for i := 0; i < snapshotFailsForRestart; i++ {
		now = t0.Add(time.Duration(i) * time.Second)
		s.tick(context.Background(), now)
	}
	if got := s.State().Engine; got != EngineOff {
		t.Errorf("движок = %q, ожидался %q", got, EngineOff)
	}
	if modeCalls != 1 {
		t.Errorf("режим спрошен %d раз; спрашивать его каждый тик — семь тысяч запусков uci за сессию", modeCalls)
	}
}

// TestFeedCountsUnparsedButNotForeign — счётчик unparsed загорается только
// от строк ПРО СОЕДИНЕНИЕ.
//
// Собственные строки движка приходят тем же уровнем info после каждого
// перезапуска, который владелец запускает с этого же экрана. Считай мы их,
// «часть событий не разобрана» загоралось бы после первого же «Применить».
func TestFeedCountsUnparsedButNotForeign(t *testing.T) {
	now := t0
	s := newSession(&fakeSource{}, &now, nil)

	s.feed(nikki.LogLine{Type: "info", Payload: "Start initial provider nm-geosite-youtube"}, now)
	s.feed(nikki.LogLine{Type: "info", Payload: "[DNS] www.google.com --> 198.18.0.63"}, now)
	if got := s.State().Unparsed; got != 0 {
		t.Errorf("unparsed = %d от собственных строк движка", got)
	}

	s.feed(nikki.LogLine{Type: "info", Payload: "[TCP] непонятно что"}, now)
	if got := s.State().Unparsed; got != 1 {
		t.Errorf("unparsed = %d, ожидалась 1", got)
	}
}

// TestFeedIgnoresOtherDevices — в журнале лежат все устройства и сам роутер.
func TestFeedIgnoresOtherDevices(t *testing.T) {
	now := t0
	s := newSession(&fakeSource{}, &now, nil)
	s.feed(nikki.LogLine{Type: "info",
		Payload: "[TCP] 192.168.0.234:37984 --> api.anthropic.com:443 match RuleSet(nm-geosite-anthropic) using BYPASS[🇫🇯⚡Фокстрот]"}, now)

	st := s.State()
	if len(st.Targets) != 0 {
		t.Errorf("чужая строка попала в таблицу: %+v", st.Targets)
	}
	if st.Unparsed != 0 {
		t.Errorf("чужая строка посчитана неразобранной: это выглядело бы как смена формата")
	}
}

// TestStateWithoutSessionIsInactive — остановленная сессия честно говорит,
// что её нет.
func TestStateWithoutSessionIsInactive(t *testing.T) {
	now := t0
	s := newSession(&fakeSource{}, &now, nil)
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	if s.State().Active {
		t.Error("остановленная сессия называет себя активной")
	}
}

// TestStartStopRunsBothGoroutines — живой запуск со всеми горутинами.
//
// Проверяет то, чего ручные tick и feed не проверяют: что Stop дожидается
// обеих горутин, а не оставляет их дочитывать поток в фоне.
func TestStartStopRunsBothGoroutines(t *testing.T) {
	src := &fakeSource{
		snaps: []nikki.Snapshot{snap(conn("a", "x.example.com", "1.2.3.4", 443, 10, 20, time.Now()))},
		lines: []string{"[TCP] " + devIP + ":50446 --> www.google.com:443 match Match using DIRECT"},
	}
	s := Start(context.Background(), Config{IP: devIP, Src: src})
	defer s.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Poll().Targets) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := s.Poll()
	if len(st.Targets) == 0 {
		t.Fatal("за три секунды не появилось ни одного адресата")
	}
	if !st.Active || st.IP != devIP {
		t.Errorf("состояние = %+v", st)
	}

	s.Stop()
	select {
	case <-s.Done():
	default:
		t.Error("Stop вернулся, не дождавшись горутин")
	}
	if s.State().Active {
		t.Error("после Stop сессия называет себя активной")
	}
}

// TestReaderReconnectsAfterStreamEnd — поток, кончившийся сам, открывается
// заново.
//
// Как именно ведёт себя тело /logs при перезапуске nikki — чистым EOF или
// повисшим соединением — разведка не сняла (RQ-09), и ответ не нужен: конец
// потока лечится переподключением, а повисший рвётся по признаку из снимка.
func TestReaderReconnectsAfterStreamEnd(t *testing.T) {
	src := &fakeSource{
		streamer: func(int) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(
				`{"type":"info","payload":"[TCP] ` + devIP + `:50446 --> www.google.com:443 match Match using DIRECT"}` + "\n")), nil
		},
	}
	s := Start(context.Background(), Config{IP: devIP, Src: src})
	defer s.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if src.opens() >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("поток открыт %d раз: после конца он обязан открыться заново", src.opens())
}
