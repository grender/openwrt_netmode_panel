package watch

import (
	"sort"
	"time"

	"netmoded/internal/nikki"
)

// Агрегат наблюдения — АДРЕСАТ, а не соединение.
//
// Одно приложение открывает десятки соединений к одному имени, и таблица
// соединений читалась бы хуже, чем сегодняшний JSON из ssh. Ключ адресата —
// имя плюс протокол: 443/tcp и 443/udp у одного домена это TLS и QUIC, то
// есть разные вещи и с разной судьбой.

const (
	// maxTargets — потолок адресатов на сессию. Вытеснение по времени
	// последнего события.
	maxTargets = 500
	// newTTL — сколько держится пометка «новый». Считает её демон, а не
	// панель: пометка обязана гаснуть сама, даже если на экран никто не
	// смотрит.
	newTTL = 30 * time.Second
	// silentAge — сколько молчит установленное соединение, прежде чем это
	// станет подсказкой. Порог ложный у долгого запроса без ответа
	// (long-polling), поэтому это подсказка, а не приговор: правило
	// добавляет человек.
	silentAge = 5 * time.Second
	// unreachableFor — окно, в котором неудавшийся дозвон ещё значим.
	unreachableFor = 60 * time.Second
	// maxPorts — сколько портов показывать: до трёх, по частоте.
	maxPorts = 3
)

// Verdict — главное, что есть на экране, и это факт, а не догадка.
type Verdict string

const (
	// VerdictOK — всё остальное.
	VerdictOK Verdict = "ok"
	// VerdictSilent — рукопожатие прошло, ответа нет: типичная блокировка
	// по имени сайта.
	VerdictSilent Verdict = "silent"
	// VerdictUnreachable — движок пытался соединиться и не смог. У прямой
	// цепочки лечится правилом «в туннель»; у цепочки туннеля правило не
	// поможет — там либо мёртв узел, либо адресат закрыт с той стороны.
	VerdictUnreachable Verdict = "unreachable"
)

// Target — строка экрана.
type Target struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// IsIP — имени нет; правило для такого адресата будет cidr.
	IsIP bool   `json:"is_ip"`
	Net  string `json:"net"`
	// Ports — до трёх разных, по частоте.
	Ports []int `json:"ports"`
	// Addr — последний remoteDestination: реальный адрес, куда ушло.
	Addr string `json:"addr"`
	// Geo — метка страны, если движок её поставил. Пусто у большинства
	// строк, и это НЕ «страна неизвестна», а отсутствие поля.
	Geo []string `json:"geo,omitempty"`
	// Rule, Payload, Chain — что сработало и куда ушло.
	Rule    string `json:"rule"`
	Payload string `json:"payload"`
	Chain   string `json:"chain"`
	First   string `json:"first"`
	Last    string `json:"last"`
	// Count, Live — соединений всего и живых сейчас.
	Count int `json:"count"`
	Live  int `json:"live"`
	// Up, Down — байты за сессию; RateUp, RateDown — по последнему тику.
	Up       int64 `json:"up"`
	Down     int64 `json:"down"`
	RateUp   int64 `json:"rate_up"`
	RateDown int64 `json:"rate_down"`
	// DialErrors, LastError — из строк «dial … error».
	DialErrors int    `json:"dial_errors"`
	LastError  string `json:"last_error"`
	// ErrorChain — цепочка, через которую не дозвонились. Отдельным полем,
	// потому что у неудачи через туннель подсказка другая, чем у прямой, а
	// искать скобки в Chain — это программировать на форме имени узла.
	ErrorChain string `json:"error_chain"`
	// Verdict — ok | silent | unreachable.
	Verdict Verdict `json:"verdict"`
	// New — появился после старта наблюдения; гаснет сам через newTTL.
	New bool `json:"new"`
}

// entry — адресат плюс то, что наружу не отдаётся.
type entry struct {
	t Target
	// firstAt, lastAt — те же метки, что First/Last, но временем, а не
	// строкой: по ним считается вытеснение и пометка «новый».
	firstAt, lastAt time.Time
	ports           map[int]int
	// lastDialAt, lastOKAt — два факта, из которых складывается вердикт
	// «не отвечает»: была неудача и после неё не было удачи.
	lastDialAt, lastOKAt time.Time
	// silentTick — на последнем снимке было живое соединение, которое
	// отправило запрос и не получило ответа.
	silentTick bool
	// appeared — адресат родился уже при наблюдении, а не приехал первым
	// снимком. Без этого «новыми» оказались бы все полторы сотни
	// соединений в нулевую секунду, и пометка не значила бы ничего.
	appeared bool
}

// sample — что мы видели у соединения на прошлом тике.
type sample struct {
	key      string
	up, down int64
}

// pendingEvent — строка журнала, ждущая своего соединения в снимке.
type pendingEvent struct {
	key string
	at  time.Time
}

// table — состояние сессии наблюдения.
//
// Не потокобезопасна сама по себе: замок держит сессия, потому что только
// она знает, что разбор строки (дорогой) обязан идти ВНЕ замка.
type table struct {
	m map[string]*entry
	// pend — строки журнала, для которых соединение ещё не появилось в
	// снимке. Ключ — «адрес:порт источника».
	pend map[string]pendingEvent
	// prev — байты соединений на прошлом тике, по id соединения.
	prev map[string]sample
	// primed — первый снимок уже применён.
	primed            bool
	unparsed, dropped int
}

func newTable() *table {
	return &table{
		m:    map[string]*entry{},
		pend: map[string]pendingEvent{},
		prev: map[string]sample{},
	}
}

func targetKey(name, netProto string) string { return name + "|" + netProto }

// get находит или заводит адресата.
func (tb *table) get(key, name, netProto string, isIP bool, now time.Time) *entry {
	if e, ok := tb.m[key]; ok {
		return e
	}
	tb.evict()
	e := &entry{
		t:       Target{Key: key, Name: name, IsIP: isIP, Net: netProto, Verdict: VerdictOK},
		firstAt: now,
		lastAt:  now,
		ports:   map[int]int{},
		// Новым считается только тот, кто появился ПОСЛЕ первого снимка:
		// первый снимок приносит всё, что и так шло.
		appeared: tb.primed,
	}
	tb.m[key] = e
	return e
}

// evict освобождает место под нового адресата.
//
// Линейный поиск по пятистам записям и только когда потолок достигнут:
// куча ради случая, до которого большинство сессий не доживает, стоила бы
// дороже, чем стоит сам случай.
func (tb *table) evict() {
	if len(tb.m) < maxTargets {
		return
	}
	var oldest string
	var at time.Time
	for k, e := range tb.m {
		if oldest == "" || e.lastAt.Before(at) {
			oldest, at = k, e.lastAt
		}
	}
	delete(tb.m, oldest)
	tb.dropped++
}

// bumpPort считает порты по частоте и оставляет три самых частых.
func (e *entry) bumpPort(p int) {
	if p == 0 {
		return
	}
	e.ports[p]++
	ports := make([]int, 0, len(e.ports))
	for k := range e.ports {
		ports = append(ports, k)
	}
	sort.Slice(ports, func(i, j int) bool {
		if e.ports[ports[i]] != e.ports[ports[j]] {
			return e.ports[ports[i]] > e.ports[ports[j]]
		}
		return ports[i] < ports[j]
	})
	if len(ports) > maxPorts {
		ports = ports[:maxPorts]
	}
	e.t.Ports = ports
}

// applyEvent кладёт в таблицу разобранную строку журнала.
func (tb *table) applyEvent(ev Event, now time.Time) {
	key := targetKey(ev.Host, ev.Net)
	e := tb.get(key, ev.Host, ev.Net, ev.IsIP, now)
	e.lastAt = now
	e.bumpPort(ev.Port)

	switch ev.Kind {
	case EventMatch:
		// Соединение установлено, и в снимке оно появится в течение
		// секунды. Считаем его здесь, а снимку оставляем метку, чтобы он не
		// посчитал его второй раз.
		e.t.Count++
		e.lastOKAt = now
		e.t.Rule, e.t.Payload, e.t.Chain = ev.Rule, ev.Payload, ev.Chain
		tb.pend[ev.SourceKey()] = pendingEvent{key: key, at: now}
	case EventDial:
		// Дозвон не удался — соединением он не стал и в снимке его не будет
		// вовсе. Это единственный источник вердикта «не отвечает».
		e.t.DialErrors++
		e.t.LastError = ev.Err
		e.t.ErrorChain = ev.Chain
		e.lastDialAt = now
		if ev.Rule != "" {
			e.t.Rule, e.t.Payload = ev.Rule, ev.Payload
		}
		if e.t.Chain == "" {
			e.t.Chain = ev.Chain
		}
	}
}

// applySnapshot применяет снимок: байты, скорости, живость, правило.
//
// elapsed измеряется, а не берётся равным секунде: тик, отработавший
// дольше, раздул бы скорость ровно на своё опоздание.
func (tb *table) applySnapshot(snap nikki.Snapshot, srcIP string, elapsed time.Duration, now time.Time) {
	for _, e := range tb.m {
		e.t.Live = 0
		e.t.RateUp, e.t.RateDown = 0, 0
		e.silentTick = false
	}

	next := make(map[string]sample, len(snap.Connections))
	tickUp := map[string]int64{}
	tickDown := map[string]int64{}

	for _, c := range snap.Connections {
		if c.SourceIP != srcIP {
			continue
		}
		name := c.Name()
		if name == "" {
			continue
		}
		key := targetKey(name, c.Net)
		isIP := name == c.RemoteDestination && c.Host == "" && c.SniffHost == ""
		e := tb.get(key, name, c.Net, isIP, now)

		prev, seen := tb.prev[c.ID]
		if !seen {
			// Соединение впервые в снимке. Если оно же пришло строкой
			// журнала — метка эта строка и оставила, и второй раз считать
			// его нельзя.
			if p, ok := tb.pend[c.SourceKey()]; ok && p.key == key && absDur(p.at.Sub(c.Start)) <= joinWindow {
				delete(tb.pend, c.SourceKey())
			} else {
				e.t.Count++
			}
		}

		dUp, dDown := c.Upload-prev.up, c.Download-prev.down
		if !seen || dUp < 0 || dDown < 0 {
			// Не видели раньше либо счётчики поехали назад (движок
			// перезапустился, id стали новыми) — берём то, что есть, за
			// прирост целиком.
			dUp, dDown = c.Upload, c.Download
		}
		// Байты копятся приростами, а не пересчётом суммы живых: адресат,
		// чьё соединение закрылось, иначе терял бы всё, что перенёс.
		e.t.Up += dUp
		e.t.Down += dDown
		tickUp[key] += dUp
		tickDown[key] += dDown

		e.t.Live++
		e.t.Addr = c.RemoteDestination
		if len(c.Geo) > 0 {
			e.t.Geo = c.Geo
		}
		if c.Rule != "" {
			e.t.Rule, e.t.Payload = c.Rule, c.RulePayload
		}
		if ch := nikki.ChainString(c.Chains); ch != "" {
			e.t.Chain = ch
		}
		e.bumpPort(c.DestinationPort)
		e.lastAt = now
		if c.Download > 0 {
			e.lastOKAt = now
		}
		// Молчит: соединение живёт дольше порога, запрос ушёл, ответа нет,
		// и ушло оно напрямую. У туннеля тот же признак значит другое, и
		// подсказка там другая.
		if !c.Start.IsZero() && now.Sub(c.Start) > silentAge &&
			c.Upload > 0 && c.Download == 0 && nikki.ChainString(c.Chains) == "DIRECT" {
			e.silentTick = true
		}

		next[c.ID] = sample{key: key, up: c.Upload, down: c.Download}
	}

	if elapsed > 0 {
		for key, v := range tickUp {
			if e, ok := tb.m[key]; ok {
				e.t.RateUp = perSecond(v, elapsed)
				e.t.RateDown = perSecond(tickDown[key], elapsed)
			}
		}
	}

	tb.prev = next
	tb.primed = true
	tb.sweepPending(now)
}

// sweepPending выбрасывает метки, которых снимок так и не забрал: порт
// источника переиспользуется, и старая метка склеила бы чужое соединение.
func (tb *table) sweepPending(now time.Time) {
	for k, p := range tb.pend {
		if now.Sub(p.at) > joinWindow {
			delete(tb.pend, k)
		}
	}
}

func perSecond(v int64, d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(float64(v) * float64(time.Second) / float64(d))
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// zeroLive гасит живость и скорости: движок перезапускается, соединений нет,
// а накопленное остаётся.
func (tb *table) zeroLive() {
	for _, e := range tb.m {
		e.t.Live = 0
		e.t.RateUp, e.t.RateDown = 0, 0
		e.silentTick = false
	}
	tb.prev = map[string]sample{}
}

// snapshotTargets собирает ответ: вердикты, времена, пометка «новый».
func (tb *table) snapshotTargets(now time.Time) []Target {
	out := make([]Target, 0, len(tb.m))
	for _, e := range tb.m {
		t := e.t
		t.Verdict = verdictOf(e, now)
		t.First = e.firstAt.UTC().Format(time.RFC3339)
		t.Last = e.lastAt.UTC().Format(time.RFC3339)
		t.New = e.appeared && now.Sub(e.firstAt) < newTTL
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
