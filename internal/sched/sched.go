// Package sched — расписание обновления подписки внутри демона.
//
// Расписание забрано у cron намеренно (SPEC §9): тогда «последнее
// обновление», результат и журнал живут в одном месте и не расходятся.
// Строку happ2clash из /etc/crontabs/root надо убрать руками — демон
// чужой crontab не правит.
//
// `flock` внутри самого happ2clash сохраняется: скрипт остаётся
// запускаемым из ssh, и пересечения быть не должно.
package sched

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"netmoded/internal/logs"
)

// DefaultInterval — как часто обновлять подписку.
//
// Двенадцать часов: провайдер меняет узлы редко, а каждое обновление —
// это запись во флеш. Чаще смысла нет, реже — список успевает протухнуть.
const DefaultInterval = 12 * time.Hour

// Updater запускает конвертер подписки.
type Updater interface {
	UpdateSubscription(ctx context.Context) ([]byte, error)
}

// Scheduler периодически обновляет подписку.
type Scheduler struct {
	up  Updater
	log *logs.Log

	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	lastRun  time.Time
	running  bool
	stopOnce sync.Once
	stop     chan struct{}
}

func New(up Updater, log *logs.Log, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Scheduler{
		up:       up,
		log:      log,
		interval: interval,
		now:      time.Now,
		stop:     make(chan struct{}),
	}
}

// Run крутит расписание до отмены контекста.
//
// Первое обновление НЕ делается на старте: демон перезапускается при
// каждом обновлении прошивки и при отладке, и обновлять подписку на каждый
// перезапуск значило бы дёргать провайдера почём зря.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			_, _ = s.RunOnce(ctx)
		}
	}
}

// Stop останавливает расписание.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// ErrAlreadyRunning — обновление уже идёт.
//
// Пересечение возможно: расписание сработало ровно тогда, когда владелец
// нажал «обновить сейчас». Второй запуск не нужен — конвертер всё равно
// сериализован своим flock, и мы бы просто ждали впустую.
var ErrAlreadyRunning = errors.New("sched: обновление уже идёт")

// RunOnce выполняет одно обновление и пишет результат в журнал.
//
// Возвращает число разобранных узлов. Ошибка конвертера не считается
// сбоем демона: она попадает в журнал и в статус, а демон живёт дальше.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return 0, ErrAlreadyRunning
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastRun = s.now()
		s.mu.Unlock()
	}()

	out, err := s.up.UpdateSubscription(ctx)
	nodes := ParseNodeCount(out)

	entry := logs.Entry{TS: s.now().UTC(), Nodes: nodes}
	switch {
	case err != nil:
		entry.Status = logs.StatusFail
		entry.Err = err.Error()
	case nodes == 0:
		// Предохранитель happ2clash: при нуле разобранных узлов старый
		// файл провайдера НЕ перезаписывается (SPEC §2). Для нас это
		// неудача, а не успех с нулём — иначе панель показала бы
		// «обновлено, 0 узлов», хотя список остался прежним.
		entry.Status = logs.StatusFail
		entry.Err = "конвертер вернул 0 узлов, файл провайдера не перезаписан"
		err = errors.New(entry.Err)
	default:
		entry.Status = logs.StatusOK
	}

	// Сбой записи журнала не отменяет сделанного: подписка уже обновилась.
	if lerr := s.log.Append(entry); lerr != nil && err == nil {
		err = fmt.Errorf("подписка обновлена, но журнал не записан: %w", lerr)
	}

	if entry.Status == logs.StatusFail {
		return nodes, err
	}
	return nodes, nil
}

// LastRun — когда обновление выполнялось последний раз в этом процессе.
func (s *Scheduler) LastRun() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun
}

// Running сообщает, идёт ли обновление.
func (s *Scheduler) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// nodeCountRe выхватывает число узлов из вывода happ2clash.
//
// Формат вывода скрипта не зафиксирован контрактом, поэтому берём число
// рядом со словом об узлах, а не гадаем по позиции. Не нашли — ноль,
// и это будет трактовано как неудача: лучше лишний раз сказать «не
// получилось», чем показать успех, которого не было.
var nodeCountRe = regexp.MustCompile(`(?i)(\d+)\s*(?:nodes?|узл|proxies|proxy)`)

// ParseNodeCount достаёт число узлов из вывода конвертера.
func ParseNodeCount(out []byte) int {
	m := nodeCountRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return n
}
