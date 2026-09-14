package watch

import (
	"fmt"
	"testing"
	"time"

	"netmoded/internal/nikki"
)

const devIP = "192.168.9.219"

var t0 = time.Date(2026, 9, 14, 2, 10, 0, 0, time.UTC)

func conn(id, name, addr string, port int, up, down int64, start time.Time, chains ...string) nikki.Conn {
	if len(chains) == 0 {
		chains = []string{"DIRECT"}
	}
	return nikki.Conn{
		ID: id, Net: "tcp", SourceIP: devIP, SourcePort: 50000 + len(id),
		Host: name, RemoteDestination: addr, DestinationPort: port,
		Upload: up, Download: down, Start: start, Chains: chains,
		Rule: "Match",
	}
}

func snap(cs ...nikki.Conn) nikki.Snapshot { return nikki.Snapshot{Connections: cs} }

func find(t *testing.T, tb *table, name string, now time.Time) Target {
	t.Helper()
	for _, x := range tb.snapshotTargets(now) {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("адресата %q нет в таблице", name)
	return Target{}
}

// TestVanishedConnectionKeepsItsBytes — байты копятся приростами.
//
// Пересчёт суммы по живым соединениям каждый тик выглядел бы проще и терял
// бы ВСЁ, что перенесло закрывшееся соединение: у адресата, который только
// что скачал сто мегабайт и закрылся, в таблице стоял бы ноль.
func TestVanishedConnectionKeepsItsBytes(t *testing.T) {
	tb := newTable()
	c := conn("a", "cdn.example.com", "1.2.3.4", 443, 100, 1000, t0.Add(-time.Minute))
	tb.applySnapshot(snap(c), devIP, time.Second, t0)

	c.Upload, c.Download = 300, 5000
	tb.applySnapshot(snap(c), devIP, time.Second, t0.Add(time.Second))

	// Соединение исчезло из снимка.
	tb.applySnapshot(snap(), devIP, time.Second, t0.Add(2*time.Second))

	got := find(t, tb, "cdn.example.com", t0.Add(2*time.Second))
	if got.Up != 300 || got.Down != 5000 {
		t.Errorf("байты = ↑%d ↓%d, ожидалось ↑300 ↓5000", got.Up, got.Down)
	}
	if got.Live != 0 {
		t.Errorf("живых = %d, соединение закрылось", got.Live)
	}
	if got.RateUp != 0 || got.RateDown != 0 {
		t.Errorf("скорость = ↑%d ↓%d, ожидались нули", got.RateUp, got.RateDown)
	}
}

// TestRatesUseMeasuredElapsed — скорость делится на ИЗМЕРЕННОЕ время.
//
// Тик, отработавший вдвое дольше, при делении на константу «секунда» раздул
// бы скорость ровно на своё опоздание — и именно тогда, когда роутер занят.
func TestRatesUseMeasuredElapsed(t *testing.T) {
	tb := newTable()
	c := conn("a", "x.example.com", "1.2.3.4", 443, 0, 0, t0)
	tb.applySnapshot(snap(c), devIP, time.Second, t0)
	c.Download = 2000
	tb.applySnapshot(snap(c), devIP, 2*time.Second, t0.Add(2*time.Second))

	got := find(t, tb, "x.example.com", t0.Add(2*time.Second))
	if got.RateDown != 1000 {
		t.Errorf("скорость = %d Б/с, ожидалось 1000 (2000 байт за две секунды)", got.RateDown)
	}
}

// TestJoinCountsConnectionOnce — соединение, увиденное обоими источниками,
// считается один раз.
//
// Строка журнала пишется в момент удачного дозвона, то есть РАНЬШЕ, чем
// соединение попадёт хоть в один снимок. Ради этого склейка и существует:
// без неё каждое обычное соединение считалось бы дважды.
func TestJoinCountsConnectionOnce(t *testing.T) {
	tb := newTable()
	ev, kind := ParseLine("[TCP] " + devIP + ":50446 --> www.google.com:443 match Match using DIRECT")
	if kind != LineParsed {
		t.Fatalf("строка не разобрана")
	}
	tb.applyEvent(ev, t0)

	c := conn("a", "www.google.com", "142.250.74.100", 443, 100, 200, t0)
	c.SourcePort = 50446
	tb.applySnapshot(snap(c), devIP, time.Second, t0.Add(300*time.Millisecond))

	got := find(t, tb, "www.google.com", t0.Add(300*time.Millisecond))
	if got.Count != 1 {
		t.Errorf("соединений = %d, ожидалось 1: журнал и снимок увидели одно и то же", got.Count)
	}
}

// TestReusedSourcePortOutsideWindow — за окном склейки метка не действует.
//
// Порт источника переиспользуется; без окна метка от давно закрытого
// соединения однажды приклеила бы чужое и спрятала бы его из счёта.
func TestReusedSourcePortOutsideWindow(t *testing.T) {
	tb := newTable()
	ev, _ := ParseLine("[TCP] " + devIP + ":50446 --> www.google.com:443 match Match using DIRECT")
	tb.applyEvent(ev, t0)

	late := t0.Add(joinWindow + time.Second)
	c := conn("a", "www.google.com", "142.250.74.100", 443, 1, 1, late)
	c.SourcePort = 50446
	tb.applySnapshot(snap(c), devIP, time.Second, late)

	got := find(t, tb, "www.google.com", late)
	if got.Count != 2 {
		t.Errorf("соединений = %d, ожидалось 2: порт тот же, а соединение другое", got.Count)
	}
}

// TestPendingSweptAfterWindow — просроченные метки не копятся.
func TestPendingSweptAfterWindow(t *testing.T) {
	tb := newTable()
	ev, _ := ParseLine("[TCP] " + devIP + ":50446 --> www.google.com:443 match Match using DIRECT")
	tb.applyEvent(ev, t0)
	if len(tb.pend) != 1 {
		t.Fatalf("меток %d", len(tb.pend))
	}
	tb.applySnapshot(snap(), devIP, time.Second, t0.Add(joinWindow+time.Second))
	if len(tb.pend) != 0 {
		t.Errorf("метка пережила окно: %+v", tb.pend)
	}
}

// TestForeignSourceIgnoredInSnapshot — в снимке лежат все устройства сразу,
// фильтра на стороне mihomo нет.
func TestForeignSourceIgnoredInSnapshot(t *testing.T) {
	tb := newTable()
	other := conn("b", "other.example.com", "5.6.7.8", 443, 10, 10, t0)
	other.SourceIP = "192.168.9.163"
	tb.applySnapshot(snap(conn("a", "mine.example.com", "1.2.3.4", 443, 1, 1, t0), other), devIP, time.Second, t0)

	if len(tb.m) != 1 {
		t.Fatalf("адресатов %d, ожидался один: чужое устройство не наше дело", len(tb.m))
	}
}

// TestTopThreePortsByFrequency — до трёх портов, по частоте.
func TestTopThreePortsByFrequency(t *testing.T) {
	tb := newTable()
	e := tb.get(targetKey("x", "tcp"), "x", "tcp", false, t0)
	for _, p := range []int{443, 443, 443, 80, 80, 5222, 8080} {
		e.bumpPort(p)
	}
	if len(e.t.Ports) != maxPorts || e.t.Ports[0] != 443 || e.t.Ports[1] != 80 {
		t.Errorf("порты = %v, ожидались три самых частых во главе с 443", e.t.Ports)
	}
}

// TestNewOnlyAfterFirstSnapshot — «новый» у того, кто появился при
// наблюдении.
//
// Сессия стартует пустой, и первый снимок приносит всё, что и так шло.
// Считай мы новыми и их, пометка загоралась бы на всей таблице в нулевую
// секунду и не значила бы ничего.
func TestNewOnlyAfterFirstSnapshot(t *testing.T) {
	tb := newTable()
	tb.applySnapshot(snap(conn("a", "old.example.com", "1.2.3.4", 443, 1, 1, t0)), devIP, time.Second, t0)
	tb.applySnapshot(snap(
		conn("a", "old.example.com", "1.2.3.4", 443, 2, 2, t0),
		conn("bb", "new.example.com", "5.6.7.8", 443, 1, 1, t0),
	), devIP, time.Second, t0.Add(time.Second))

	now := t0.Add(time.Second)
	if find(t, tb, "old.example.com", now).New {
		t.Error("адресат из первого снимка помечен новым")
	}
	if !find(t, tb, "new.example.com", now).New {
		t.Error("появившийся при наблюдении не помечен новым")
	}
	// Пометка гаснет сама, даже если на экран никто не смотрит.
	if find(t, tb, "new.example.com", now.Add(newTTL+time.Second)).New {
		t.Error("пометка «новый» не погасла через newTTL")
	}
}

// TestEvictsOldestBeyondCap — потолок в 500 адресатов, вытеснение по
// времени последнего события.
func TestEvictsOldestBeyondCap(t *testing.T) {
	tb := newTable()
	for i := 0; i < maxTargets; i++ {
		name := fmt.Sprintf("h%04d.example.com", i)
		tb.get(targetKey(name, "tcp"), name, "tcp", false, t0.Add(time.Duration(i)*time.Second))
	}
	if len(tb.m) != maxTargets {
		t.Fatalf("адресатов %d", len(tb.m))
	}
	tb.get(targetKey("fresh", "tcp"), "fresh", "tcp", false, t0.Add(time.Hour))

	if len(tb.m) != maxTargets {
		t.Errorf("после вытеснения адресатов %d, потолок %d", len(tb.m), maxTargets)
	}
	if tb.dropped != 1 {
		t.Errorf("dropped = %d, ожидалась 1", tb.dropped)
	}
	if _, ok := tb.m[targetKey("h0000.example.com", "tcp")]; ok {
		t.Error("вытеснен не самый старый")
	}
	if _, ok := tb.m[targetKey("fresh", "tcp")]; !ok {
		t.Error("новый адресат не попал в таблицу")
	}
}

// TestZeroLiveKeepsCounters — при перезапуске движка накопленное остаётся.
//
// Обнулить вместе с живыми и счётчики значило бы стереть у владельца ту
// самую картину, ради которой он и нажал «Применить».
func TestZeroLiveKeepsCounters(t *testing.T) {
	tb := newTable()
	tb.applySnapshot(snap(conn("a", "x.example.com", "1.2.3.4", 443, 500, 900, t0)), devIP, time.Second, t0)
	tb.zeroLive()

	got := find(t, tb, "x.example.com", t0)
	if got.Up != 500 || got.Down != 900 {
		t.Errorf("байты = ↑%d ↓%d, ожидались прежние", got.Up, got.Down)
	}
	if got.Live != 0 {
		t.Errorf("живых = %d, движок перезапускается", got.Live)
	}
	if len(tb.prev) != 0 {
		t.Error("прошлый снимок не забыт: после перезапуска id соединений другие")
	}
}

// TestSnapshotNamesTargetByFallback — имя берётся sniffHost → host → адрес.
func TestSnapshotNamesTargetByFallback(t *testing.T) {
	tb := newTable()
	c := conn("a", "", "13.222.111.224", 554, 10, 10, t0)
	c.SniffHost = ""
	c2 := conn("bb", "", "5.6.7.8", 443, 1, 1, t0)
	c2.SniffHost = "sniffed.example.com"
	tb.applySnapshot(snap(c, c2), devIP, time.Second, t0)

	bare := find(t, tb, "13.222.111.224", t0)
	if !bare.IsIP {
		t.Error("адресат без имени не помечен адресом: правило для него будет cidr")
	}
	sniffed := find(t, tb, "sniffed.example.com", t0)
	if sniffed.IsIP {
		t.Error("адресат с именем от sniffer помечен адресом")
	}
}
