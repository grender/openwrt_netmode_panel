package watch

import (
	"context"
	"sync"
	"time"

	"netmoded/internal/nikki"
	"netmoded/internal/safe"
)

// Сессия наблюдения: одна на процесс, как job.Manager держит одну операцию.
//
// Панель ↔ демон — по-прежнему только опрос раз в секунду (ADR-0017).
// Исключение здесь одно и осознанное: демон держит поток к mihomo по петле,
// переподключает его и помнит состояние между запросами. Цену ограничивают
// одна сессия, петля и TTL: не смотрят — через минуту всё отдано.

const (
	// pollInterval — как часто берётся снимок.
	pollInterval = time.Second
	// idleTTL — сколько сессия живёт без запроса панели. Закрыл вкладку —
	// через минуту поток закрыт, память отдана.
	idleTTL = 60 * time.Second
	// sessionMax — абсолютный потолок. Вкладка, забытая открытой на ночь,
	// не должна держать поток к движку до утра.
	sessionMax = 2 * time.Hour
	// snapshotFailsForRestart — сколько отказов снимка подряд считать
	// перезапуском.
	//
	// Двойка, а не единица: один потерянный запрос переводил бы экран в
	// «движок перезагружает конфигурацию» на ровном месте. Уменьшение
	// uploadTotal — другое дело, оно доказательно и действует сразу.
	snapshotFailsForRestart = 2
)

// backoff — паузы переподключения потока.
var backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}

// Состояние движка глазами сессии.
const (
	EngineRunning    = "running"
	EngineRestarting = "restarting"
	EngineOff        = "off"
)

// State — то, что уезжает в панель.
type State struct {
	Active bool   `json:"active"`
	IP     string `json:"ip,omitempty"`
	Since  string `json:"since,omitempty"`
	Engine string `json:"engine,omitempty"`
	// Unparsed — строки журнала, которые разборщик не узнал. Больше нуля
	// значит, что mihomo сменил формат строки; снимки при этом работают.
	Unparsed int `json:"unparsed"`
	// Dropped — адресаты, вытесненные сверх потолка.
	Dropped int      `json:"dropped"`
	Targets []Target `json:"targets"`
}

// Source — то, что сессии нужно от движка.
//
// Свой узкий интерфейс, а не nikki.Client целиком: замеры задержек и
// перезагрузка провайдеров наблюдателю не нужны, и зависеть от них значило
// бы подделывать их в каждом тесте сессии.
type Source interface {
	Connections(ctx context.Context) (nikki.Snapshot, error)
	LogStream(ctx context.Context, level string) (*nikki.LogStream, error)
}

// Config — что нужно завести сессию.
type Config struct {
	// IP — устройство. Одно на сессию: IPv6-источников за обе пробы
	// разведки не встретилось ни одного.
	IP  string
	Src Source
	// Mode — текущий режим обхода. Спрашивается ТОЛЬКО когда снимок не
	// ответил: именно тогда «перезапускается» и «выключен» надо различить.
	// Спрашивать его каждый тик значило бы за двухчасовую сессию запустить
	// uci семь тысяч раз.
	Mode func(context.Context) (string, error)
	// Now — часы. Подменяются в тестах.
	Now func() time.Time
	// Logf — журнал демона. Пустой означает тишину.
	Logf func(string, ...any)
}

// Session — живая сессия наблюдения.
type Session struct {
	cfg Config

	mu       sync.Mutex
	tb       *table
	since    time.Time
	lastPoll time.Time
	lastTick time.Time
	engine   string
	fails    int
	lastUp   int64
	primed   bool
	stopped  bool

	cancel context.CancelFunc
	done   chan struct{}

	// reconnect просит читателя переоткрыть поток. Ёмкость 1: два подряд
	// стоящих запроса означают ровно то же, что один.
	reconnect chan struct{}
	// streamOpens считает открытия потока: «поток переоткрыли» и «поток не
	// рвали» по одному только состоянию неразличимы.
	streamOpens int
}

// Start заводит сессию и обе её горутины.
func Start(ctx context.Context, cfg Config) *Session {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	now := cfg.Now()
	ctx, cancel := context.WithCancel(ctx)
	s := &Session{
		cfg:       cfg,
		tb:        newTable(),
		since:     now,
		lastPoll:  now,
		lastTick:  now,
		engine:    EngineRunning,
		cancel:    cancel,
		done:      make(chan struct{}),
		reconnect: make(chan struct{}, 1),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// safe.Do зовётся ВНУТРИ горутины: recover действует только в той,
		// где случилась паника (ADR-0021).
		_ = safe.Do(cfg.Logf, "снимки наблюдения за устройством", func() error {
			s.poll(ctx)
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_ = safe.Do(cfg.Logf, "чтение журнала движка для наблюдателя", func() error {
			s.read(ctx)
			return nil
		})
	}()
	go func() {
		wg.Wait()
		close(s.done)
	}()
	return s
}

// Stop гасит сессию и ждёт, пока обе горутины уйдут.
func (s *Session) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	<-s.done
}

// Done закрывается, когда сессия догорела — сама по TTL или по Stop.
func (s *Session) Done() <-chan struct{} { return s.done }

// Poll отдаёт состояние и ПРОДЛЕВАЕТ жизнь сессии.
//
// Зовётся только из GET /api/watch. Разделение с State не косметика: сводка
// наблюдения когда-нибудь поедет на главный экран, который опрашивается раз
// в секунду, и если бы она тоже продлевала TTL, свойство «закрыл вкладку —
// погасло через минуту» отменилось бы для всякого, кто просто ушёл с экрана.
func (s *Session) Poll() State {
	s.mu.Lock()
	s.lastPoll = s.cfg.Now()
	s.mu.Unlock()
	return s.State()
}

// State отдаёт состояние, ничего не продлевая.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return State{Active: false}
	}
	now := s.cfg.Now()
	return State{
		Active:   true,
		IP:       s.cfg.IP,
		Since:    s.since.UTC().Format(time.RFC3339),
		Engine:   s.engine,
		Unparsed: s.tb.unparsed,
		Dropped:  s.tb.dropped,
		Targets:  s.tb.snapshotTargets(now),
	}
}

// poll — горутина снимков.
func (s *Session) poll(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.tick(ctx, s.cfg.Now()) {
				s.cancel()
				return
			}
		}
	}
}

// tick — один шаг снимка. false означает «сессия догорела».
//
// Вынесен из горутины ради тестов: иначе TTL, потолок и переподключение
// проверялись бы ожиданием реального времени, то есть капризно и медленно.
func (s *Session) tick(ctx context.Context, now time.Time) bool {
	s.mu.Lock()
	expired := now.Sub(s.lastPoll) > idleTTL || now.Sub(s.since) > sessionMax
	elapsed := now.Sub(s.lastTick)
	s.lastTick = now
	s.mu.Unlock()
	if expired {
		return false
	}

	snap, err := s.cfg.Src.Connections(ctx)
	if err != nil {
		s.snapshotFailed(ctx, now)
		return true
	}
	// Уменьшение счётчика движка — доказательство перезапуска: он считает
	// с собственного старта и назад не идёт.
	s.mu.Lock()
	shrank := s.primed && snap.UploadTotal < s.lastUp
	s.mu.Unlock()
	if shrank {
		s.markRestart()
	}

	s.mu.Lock()
	s.fails = 0
	s.lastUp = snap.UploadTotal
	s.primed = true
	s.engine = EngineRunning
	s.tb.applySnapshot(snap, s.cfg.IP, elapsed, now)
	s.mu.Unlock()
	return true
}

// snapshotFailed разбирает отказ снимка: движок перезапускается или выключен.
func (s *Session) snapshotFailed(ctx context.Context, now time.Time) {
	s.mu.Lock()
	s.fails++
	enough := s.fails >= snapshotFailsForRestart
	s.mu.Unlock()
	if !enough {
		return
	}

	engine := EngineRestarting
	if s.cfg.Mode != nil {
		// Режим спрашивается ровно здесь: «перезапускается» и «выключен» —
		// разные ответы на «почему пусто», и лечатся они по-разному.
		if m, err := s.cfg.Mode(ctx); err == nil && m != "nikki" {
			engine = EngineOff
		}
	}
	s.mu.Lock()
	s.engine = engine
	s.tb.zeroLive()
	s.mu.Unlock()
	s.askReconnect()
}

// markRestart — движок точно перезапустился.
func (s *Session) markRestart() {
	s.mu.Lock()
	s.engine = EngineRestarting
	s.tb.zeroLive()
	s.mu.Unlock()
	s.askReconnect()
}

// askReconnect просит читателя переоткрыть поток.
//
// Это и есть ответ на незакрытый вопрос разведки: как ведёт себя тело /logs
// при перезапуске nikki — закрывается чистым EOF или повисает — не снято, и
// ждать ответа не нужно. Признак перезапуска даёт СНИМОК, а не поток, и по
// нему поток рвётся сам. Чистый EOF читатель переживёт тем же
// переподключением.
func (s *Session) askReconnect() {
	select {
	case s.reconnect <- struct{}{}:
	default:
	}
}

// read — горутина потока журнала.
func (s *Session) read(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		ok := s.readOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if ok {
			// Поток жил и что-то отдал: следующая попытка начинается с
			// самой короткой паузы.
			attempt = 0
		}
		d := backoff[min(attempt, len(backoff)-1)]
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// readOnce открывает поток и читает его до конца. true — что-то пришло.
func (s *Session) readOnce(ctx context.Context) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	s.streamOpens++
	s.mu.Unlock()

	st, err := s.cfg.Src.LogStream(ctx, "info")
	if err != nil {
		return false
	}
	defer st.Close()

	// Поток рвётся не только своим концом: снимок мог увидеть перезапуск
	// движка, и тогда читать дальше нечего.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		_ = safe.Do(s.cfg.Logf, "сторож переподключения к журналу", func() error {
			select {
			case <-stop:
			case <-ctx.Done():
			case <-s.reconnect:
				cancel()
			}
			return nil
		})
	}()

	var got bool
	for st.Next() {
		got = true
		s.feed(st.Line(), s.cfg.Now())
	}
	// Слишком длинные строки — тоже смена формата: разбирать их не в чем,
	// и молчать о них нельзя.
	if n := st.Oversize(); n > 0 {
		s.mu.Lock()
		s.tb.unparsed += n
		s.mu.Unlock()
	}
	return got
}

// feed кладёт строку в таблицу.
//
// Разбор идёт ВНЕ замка намеренно: под замком он стоял бы в очереди за
// сериализацией пятисот адресатов для панели, а mihomo молча выбрасывает
// события, когда канал подписчика на 1024 переполняется. Застрявший
// читатель теряет события без единого сигнала.
func (s *Session) feed(l nikki.LogLine, now time.Time) {
	ev, kind := ParseLine(l.Payload)
	switch kind {
	case LineForeign:
		return
	case LineUnknown:
		s.mu.Lock()
		s.tb.unparsed++
		s.mu.Unlock()
		return
	}
	if ev.SrcIP != s.cfg.IP {
		// Чужое устройство и трафик самого роутера: строка разобрана, просто
		// не наша. В неразобранные её писать нельзя — это выглядело бы как
		// смена формата.
		return
	}
	s.mu.Lock()
	s.tb.applyEvent(ev, now)
	s.mu.Unlock()
}
