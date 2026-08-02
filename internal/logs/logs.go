// Package logs — построчный журнал обновлений подписки.
//
// Формат — JSON Lines: одна запись в строке. Дописывание не требует читать
// и переписывать файл целиком, а обрезка сводится к «оставить хвост».
//
// Файл живёт в /etc/nikki/updates.log, а НЕ в /var: там tmpfs, и история
// исчезла бы при первой же перезагрузке — то есть именно тогда, когда она
// нужна, чтобы понять, что произошло до неё (SPEC §9).
package logs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPath — путь журнала (SPEC §9).
const DefaultPath = "/etc/nikki/updates.log"

// MaxLines — сколько записей хранится.
//
// Обрезка нужна не ради места, а ради флеша: файл на этом роутере лежит
// в overlay, и бесконечный рост изнашивал бы его.
const MaxLines = 200

// Entry — одна запись. Поля названы по SPEC §9 (ts, nodes, status, err).
//
// У err нет omitempty: строка журнала обязана выглядеть одинаково независимо
// от исхода. Пропущенный ключ означал бы «версия писателя не знала про err»,
// а не «ошибки не было», и читателю пришлось бы гадать, какое из двух.
type Entry struct {
	TS     time.Time `json:"ts"`
	Nodes  int       `json:"nodes"`
	Status string    `json:"status"` // ok | fail
	Err    string    `json:"err"`
}

const (
	StatusOK   = "ok"
	StatusFail = "fail"
)

// Log — журнал на диске.
type Log struct {
	Path string
	now  func() time.Time
}

func New(path string) *Log {
	if path == "" {
		path = DefaultPath
	}
	return &Log{Path: path, now: time.Now}
}

// Append дописывает запись и обрезает файл до MaxLines.
//
// Сбой записи журнала НЕ должен валить саму операцию обновления: подписка
// уже обновилась, и потерять её результат из-за неудачной записи в лог
// было бы обменом важного на второстепенное. Ошибка возвращается, чтобы
// вызывающий её залогировал, но не отменяет сделанного.
func (l *Log) Append(e Entry) error {
	if e.TS.IsZero() {
		e.TS = l.now().UTC()
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("сборка записи: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return fmt.Errorf("каталог журнала: %w", err)
	}

	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("открытие журнала: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("запись в журнал: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("закрытие журнала: %w", err)
	}

	return l.trim()
}

// trim оставляет последние MaxLines записей.
//
// Переписывание идёт через временный файл и rename: обрыв питания посреди
// обрезки не должен оставить журнал наполовину переписанным. Файл лежит
// во флеше, и это не теоретический риск.
func (l *Log) trim() error {
	entries, err := l.readRaw()
	if err != nil {
		return err
	}
	if len(entries) <= MaxLines {
		return nil
	}

	keep := entries[len(entries)-MaxLines:]
	var buf bytes.Buffer
	for _, line := range keep {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}

	tmp := l.Path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("временный файл журнала: %w", err)
	}
	if err := os.Rename(tmp, l.Path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("замена журнала: %w", err)
	}
	return nil
}

// readRaw возвращает непустые строки файла.
func (l *Log) readRaw() ([]string, error) {
	f, err := os.Open(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("чтение журнала: %w", err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	// Записи короткие, но битая строка не должна ронять чтение: буфер
	// с запасом, а неразбираемые строки пропускаются в Tail.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("чтение журнала: %w", err)
	}
	return out, nil
}

// Tail возвращает последние n записей, новые первыми.
//
// Неразбираемые строки пропускаются молча: журнал пишет и наш демон, и —
// исторически — сам happ2clash из ssh, и одна битая строка не повод
// показать пользователю пустую историю вместо остальных.
func (l *Log) Tail(n int) ([]Entry, error) {
	if n <= 0 {
		n = 50
	}
	raw, err := l.readRaw()
	if err != nil {
		return nil, err
	}

	out := make([]Entry, 0, n)
	for i := len(raw) - 1; i >= 0 && len(out) < n; i-- {
		var e Entry
		if json.Unmarshal([]byte(raw[i]), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Last возвращает последнюю разобранную запись.
func (l *Log) Last() (Entry, bool, error) {
	es, err := l.Tail(1)
	if err != nil || len(es) == 0 {
		return Entry{}, false, err
	}
	return es[0], true, nil
}
