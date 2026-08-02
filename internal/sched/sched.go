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

// catchUpDelay — отсрочка догоняющего обновления после старта демона.
//
// Не ноль намеренно: демон поднимается раньше, чем wwan получает адрес, и
// обновление в первую же секунду упало бы в сеть, записав в журнал fail.
// Хуже того, эта запись обнулила бы отсчёт — просроченное обновление так и
// не состоялось бы, только журнал засорился. Минуты хватает, чтобы связь
// встала.
const catchUpDelay = time.Minute

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
	logf     func(string, ...any)

	mu       sync.Mutex
	lastRun  time.Time
	running  bool
	stopOnce sync.Once
	stop     chan struct{}
}

// New собирает планировщик. Пустой logf — тишина.
func New(up Updater, log *logs.Log, interval time.Duration, logf func(string, ...any)) *Scheduler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Scheduler{
		up:       up,
		log:      log,
		interval: interval,
		now:      time.Now,
		logf:     logf,
		stop:     make(chan struct{}),
	}
}

// Run крутит расписание до отмены контекста.
//
// Интервал отсчитывается от последней записи журнала, а не от старта
// процесса. Обновления на самом старте по-прежнему нет: демон
// перезапускается при каждой прошивке и при отладке, и дёргать провайдера
// на каждый перезапуск незачем. Но и тикер «с нуля» не годится — на этом
// роутере перезапуски частые, и обновление откладывалось бы месяцами,
// то есть автоматическим не было бы вовсе.
func (s *Scheduler) Run(ctx context.Context) {
	// Первое срабатывание — одноразовый таймер: его задержка зависит от
	// журнала и почти никогда не равна интервалу. Дальше обычный тикер.
	first := time.NewTimer(s.firstDelay())
	defer first.Stop()

	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case <-first.C:
		_, _ = s.RunOnce(ctx)
	}

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

// firstDelay — сколько ждать до первого обновления после старта.
//
// Пустой или нечитаемый журнал означает полный интервал, а НЕ «догнать
// немедленно»: на свежей установке подписки ещё нет, и дёргать провайдера
// раньше, чем владелец что-либо настроил, бессмысленно.
func (s *Scheduler) firstDelay() time.Duration {
	last, ok, err := s.log.Last()
	if err != nil || !ok {
		return s.interval
	}

	elapsed := s.now().Sub(last.TS)
	switch {
	case elapsed < 0:
		// Запись «из будущего»: у роутера нет RTC, и до синхронизации по
		// NTP часы уходят в прошлое. Ждать разницу значило бы отложить
		// обновление на годы — берём обычный интервал.
		return s.interval
	case elapsed >= s.interval:
		return catchUpDelay
	default:
		return s.interval - elapsed
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

	// Сбой записи журнала не отменяет сделанного: подписка уже обновилась
	// (контракт logs.Append). Поэтому ошибка уходит в лог демона, а не
	// наружу: вернуть её значило бы покрасить успешное обновление в
	// «неудачу» — ровно та ложь, против которой стоит предохранитель нуля
	// узлов выше, только наизнанку.
	if lerr := s.log.Append(entry); lerr != nil {
		s.logf("журнал обновлений не записан: %v", lerr)
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
