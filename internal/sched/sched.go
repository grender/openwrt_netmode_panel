// Package sched — расписание обновления подписки внутри демона.
//
// Расписание забрано у cron намеренно (SPEC §9): тогда «последнее
// обновление», результат и журнал живут в одном месте и не расходятся.
// Строку happ2clash из /etc/crontabs/root по-прежнему надо убрать руками —
// чужой crontab демон не правит. Причина теперь другая: не «конвертер
// дёргается дважды», а cron зовёт файл, которого на роутере уже нет, молча
// получает 127 и шлёт письмо root (ADR-0031).
//
// Само обновление сюда не входит: расписание знает, КОГДА дёргать и что
// записать в журнал, а что при этом происходит с файлами — политика
// internal/subs. Отсюда и Updater интерфейсом в одну строку.
package sched

import (
	"context"
	"errors"
	"sync"
	"time"

	"netmoded/internal/happ"
	"netmoded/internal/logs"
	"netmoded/internal/safe"
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

// Updater выполняет одно обновление подписки целиком.
//
// Интерфейс, а не *subs.Updater: расписанию нужен ровно один глагол, и на
// подставной реализации оно проверяется без сети, без файлов и без движка.
type Updater interface {
	Update(ctx context.Context) (happ.Summary, error)
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
	first := time.NewTimer(s.firstDelaySafe())
	defer first.Stop()

	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case <-first.C:
		s.runOnceSafely(ctx)
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
			s.runOnceSafely(ctx)
		}
	}
}

// runOnceSafely выполняет одно срабатывание так, чтобы паника стоила одного
// срабатывания, а не всего расписания.
//
// Цикл живёт в горутине, заведённой в main, и работает месяцами: паника
// здесь может выстрелить через полсуток после старта, без всякой связи с
// действиями владельца — просто в какой-то момент роутер перестаёт
// управляться. Ловим тут, а не в вызывающем: горутину завёл main, а recover
// действует только в той горутине, где случилась паника.
//
// Из цикла после перехвата НЕ выходим. Расписание, молча переставшее
// существовать после одной неудачи, — та же потеря функции, только
// замеченная неделями позже, по протухшей подписке. Записали стек — ждём
// следующего тика.
//
// Ошибка наружу не отдаётся, потому что отдавать её некому: исход
// обновления RunOnce уже положил в журнал подписки, а причину со стеком
// safe.Do — в лог демона.
func (s *Scheduler) runOnceSafely(ctx context.Context) {
	_ = safe.Do(s.logf, "обновление подписки по расписанию", func() error {
		_, err := s.RunOnce(ctx)
		return err
	})
}

// firstDelaySafe — задержка первого срабатывания, посчитанная так, чтобы
// паника не убила демона ещё до первого тика.
//
// firstDelay читает журнал обновлений, то есть файл, который демон не
// единственный правит. Паника здесь пришлась бы на самый старт: procd
// поднял бы демона заново, тот упал бы на том же файле — вместо потерянного
// расписания владелец получил бы перезапуск по кругу и никакого API. Не
// посчиталось — берём обычный интервал, ровно как при нечитаемом журнале.
func (s *Scheduler) firstDelaySafe() time.Duration {
	d := s.interval
	_ = safe.Do(s.logf, "расчёт задержки первого обновления", func() error {
		d = s.firstDelay()
		return nil
	})
	return d
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
// нажал «обновить сейчас». Второй запуск не нужен и вреден: у файла
// провайдера один писатель, и два обновления подряд разошлись бы в нём
// молча — кто победил, было бы видно только по времени файла.
var ErrAlreadyRunning = errors.New("sched: обновление уже идёт")

// RunOnce выполняет одно обновление и пишет результат в журнал.
//
// Возвращает сводку разбора — она уходит в панель и в диагностику целиком,
// а в журнал из неё попадает число узлов. Ошибка обновления не считается
// сбоем демона: она попадает в журнал и в статус, а демон живёт дальше.
func (s *Scheduler) RunOnce(ctx context.Context) (happ.Summary, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return happ.Summary{}, ErrAlreadyRunning
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastRun = s.now()
		s.mu.Unlock()
	}()

	sum, err := s.up.Update(ctx)

	entry := logs.Entry{TS: s.now().UTC(), Nodes: sum.Nodes}
	switch {
	case err != nil:
		entry.Status = logs.StatusFail
		entry.Err = err.Error()
	case sum.Nodes == 0:
		// НАМЕРЕННО ДУБЛИРУЮЩАЯ проверка, и убирать её не надо.
		//
		// Первичный предохранитель теперь наш и стоит в subs.Update —
		// там, где владеют файлами: при нуле узлов ни провайдер, ни
		// манифест не переписываются, и Update возвращает ошибку. Сюда
		// ноль может прийти только от обновления, которое СООБЩИЛО ОБ
		// УСПЕХЕ с пустым списком, — то есть от нашей же будущей ошибки.
		// Цена ловли здесь — одна строка, цена пропуска — «обновлено,
		// 0 узлов» в панели при неизменившемся списке.
		entry.Status = logs.StatusFail
		entry.Err = "обновление сообщило об успехе, но узлов ноль"
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
		return sum, err
	}
	return sum, nil
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
