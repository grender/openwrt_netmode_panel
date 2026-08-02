package logs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLog(t *testing.T) *Log {
	t.Helper()
	l := New(filepath.Join(t.TempDir(), "updates.log"))
	n := time.Date(2026, 8, 2, 6, 0, 0, 0, time.UTC)
	l.now = func() time.Time { n = n.Add(time.Minute); return n }
	return l
}

func TestAppendAndTail(t *testing.T) {
	l := newTestLog(t)

	for _, e := range []Entry{
		{Nodes: 41, Status: StatusOK},
		{Nodes: 0, Status: StatusFail, Err: "конвертер вернул 0 узлов"},
		{Nodes: 42, Status: StatusOK},
	} {
		if err := l.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := l.Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("записей %d, ожидалось 3", len(got))
	}
	// Новые первыми: панель показывает историю сверху вниз.
	if got[0].Nodes != 42 || got[0].Status != StatusOK {
		t.Errorf("первая запись: %+v", got[0])
	}
	if got[1].Status != StatusFail || got[1].Err == "" {
		t.Errorf("вторая запись: %+v", got[1])
	}
	// Время проставляется само, если не задано.
	if got[0].TS.IsZero() {
		t.Error("ts не проставлен")
	}
}

func TestFormatIsJSONLines(t *testing.T) {
	// Построчный JSON, а не массив: дописывание не требует читать и
	// переписывать файл целиком.
	l := newTestLog(t)
	_ = l.Append(Entry{Nodes: 1, Status: StatusOK})
	_ = l.Append(Entry{Nodes: 2, Status: StatusOK})

	b, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("строк %d, ожидалось 2:\n%s", len(lines), b)
	}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("строка %d не разбирается: %v", i, err)
		}
	}
	// Поля названы по SPEC §9. Все четыре — в каждой записи, включая err:
	// пропущенный ключ читатель не отличит от «писатель про него не знал»,
	// а пустая строка однозначно означает «ошибки не было».
	var m map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &m)
	for _, k := range []string{"ts", "nodes", "status", "err"} {
		if _, ok := m[k]; !ok {
			t.Errorf("нет поля %q: %v", k, m)
		}
	}
	if m["err"] != "" {
		t.Errorf("err успешной записи = %v, ожидалась пустая строка", m["err"])
	}
}

// Обрезка нужна не ради места, а ради флеша: файл лежит в overlay,
// и бесконечный рост изнашивал бы его.
func TestTrimsToMaxLines(t *testing.T) {
	l := newTestLog(t)
	for i := 0; i < MaxLines+50; i++ {
		if err := l.Append(Entry{Nodes: i, Status: StatusOK}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	b, _ := os.ReadFile(l.Path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != MaxLines {
		t.Fatalf("строк %d, ожидалось %d", len(lines), MaxLines)
	}

	// Оставаться должны ПОСЛЕДНИЕ записи, а не первые.
	got, _ := l.Tail(1)
	if got[0].Nodes != MaxLines+49 {
		t.Errorf("последняя запись nodes=%d, ожидалось %d", got[0].Nodes, MaxLines+49)
	}
}

func TestTailOnMissingFileIsEmptyNotError(t *testing.T) {
	// Свежая установка: обновлений ещё не было. Это не сбой.
	l := New(filepath.Join(t.TempDir(), "нет-такого", "updates.log"))
	got, err := l.Tail(10)
	if err != nil {
		t.Fatalf("отсутствующий файл — не ошибка: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("записей %d, ожидалось 0", len(got))
	}

	if _, ok, err := l.Last(); err != nil || ok {
		t.Errorf("Last по пустому журналу: ok=%v err=%v", ok, err)
	}
}

// Журнал пишет и демон, и — исторически — сам happ2clash из ssh.
// Одна битая строка не повод показать пустую историю вместо остальных.
func TestBrokenLineIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "updates.log")
	content := `{"ts":"2026-08-01T06:00:00Z","nodes":41,"status":"ok"}
это не json вовсе
{"ts":"2026-08-02T06:00:00Z","nodes":42,"status":"ok"}
{обрезано на середине
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := New(path).Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("разобрано %d записей, ожидалось 2: %+v", len(got), got)
	}
	if got[0].Nodes != 42 {
		t.Errorf("новая запись: %+v", got[0])
	}
}

func TestTailLimit(t *testing.T) {
	l := newTestLog(t)
	for i := 0; i < 10; i++ {
		_ = l.Append(Entry{Nodes: i, Status: StatusOK})
	}
	got, _ := l.Tail(3)
	if len(got) != 3 {
		t.Fatalf("записей %d, ожидалось 3", len(got))
	}
	if got[0].Nodes != 9 || got[2].Nodes != 7 {
		t.Errorf("окно не то: %d..%d", got[0].Nodes, got[2].Nodes)
	}

	// Нулевой и отрицательный запрос — разумное значение по умолчанию,
	// а не пустой ответ и не весь файл.
	if got, _ := l.Tail(0); len(got) != 10 {
		t.Errorf("Tail(0) вернул %d", len(got))
	}
}

func TestLast(t *testing.T) {
	l := newTestLog(t)
	_ = l.Append(Entry{Nodes: 41, Status: StatusOK})
	_ = l.Append(Entry{Nodes: 0, Status: StatusFail, Err: "таймаут"})

	e, ok, err := l.Last()
	if err != nil || !ok {
		t.Fatalf("Last: ok=%v err=%v", ok, err)
	}
	if e.Status != StatusFail || e.Err != "таймаут" {
		t.Errorf("последняя запись: %+v", e)
	}
}

// Обрезка идёт через временный файл и rename: обрыв питания посреди
// переписывания не должен оставить журнал наполовину.
func TestTrimLeavesNoTempFile(t *testing.T) {
	l := newTestLog(t)
	for i := 0; i < MaxLines+5; i++ {
		_ = l.Append(Entry{Nodes: i, Status: StatusOK})
	}
	if _, err := os.Stat(l.Path + ".tmp"); !os.IsNotExist(err) {
		t.Error("временный файл остался после обрезки")
	}
}

func TestPathDefaultsToSpecLocation(t *testing.T) {
	// /var не годится: там tmpfs, и история исчезла бы при перезагрузке —
	// то есть именно тогда, когда она нужна (SPEC §9).
	if got := New("").Path; got != DefaultPath {
		t.Errorf("путь по умолчанию %q, ожидался %q", got, DefaultPath)
	}
	if strings.HasPrefix(DefaultPath, "/var/") {
		t.Errorf("журнал в /var (%s) — tmpfs, история не переживёт перезагрузку", DefaultPath)
	}
}
