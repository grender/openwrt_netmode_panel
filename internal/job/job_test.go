package job

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// Главное требование SPEC §6: второй джоб ОТБИВАЕТСЯ, а не встаёт
// в очередь. Очередь означала бы, что пользователь нажал кнопку, ушёл,
// а операция началась через минуту — когда обстановка уже другая.
func TestSecondJobIsRefusedNotQueued(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})

	first, err := m.Start("mode", "b4", "Переключение на b4", 8, func(context.Context) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatalf("первый джоб: %v", err)
	}

	second, err := m.Start("subscription", "", "Обновление подписки", 4, func(context.Context) error {
		t.Error("второй джоб не должен был запуститься")
		return nil
	})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("второй джоб: ошибка %v, ожидалась ErrBusy", err)
	}
	if second.ID != "" {
		t.Errorf("отбитый джоб не должен иметь идентификатор: %+v", second)
	}

	close(release)
	if !m.Wait(time.Second) {
		t.Fatal("первый джоб не завершился")
	}

	// После завершения новый запускается.
	if _, err := m.Start("mode", "off", "снова", 1, func(context.Context) error { return nil }); err != nil {
		t.Errorf("после завершения: %v", err)
	}
	_ = first
}

func TestConcurrentStartsOnlyOneWins(t *testing.T) {
	// Гонка на старте: ровно один должен пройти, остальные получить ErrBusy.
	m := NewManager()
	release := make(chan struct{})

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, busy := 0, 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Start("mode", "nikki", "гонка", 1, func(context.Context) error {
				<-release
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, ErrBusy) {
				busy++
			}
		}()
	}
	wg.Wait()

	if ok != 1 {
		t.Errorf("запустилось %d джобов, должен был ровно один", ok)
	}
	if busy != n-1 {
		t.Errorf("отбито %d, ожидалось %d", busy, n-1)
	}
	close(release)
	m.Wait(time.Second)
}

func TestFailedJobKeepsError(t *testing.T) {
	m := NewManager()
	boom := errors.New("netmode-apply вернул 1")

	if _, err := m.Start("mode", "b4", "Переключение", 8, func(context.Context) error {
		return boom
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.Wait(time.Second) {
		t.Fatal("джоб не завершился")
	}

	cur := m.Current()
	if cur == nil {
		t.Fatal("завершённый джоб пропал сразу — результат некому прочитать")
	}
	if cur.State != Failed {
		t.Errorf("состояние %q, ожидалось failed", cur.State)
	}
	if cur.Error == nil || *cur.Error != boom.Error() {
		t.Errorf("ошибка не сохранена: %v", cur.Error)
	}
	if cur.FinishedAt == nil {
		t.Error("не проставлено время завершения")
	}
}

// Завершённый джоб показывается ещё несколько секунд: панель опрашивает
// статус раз в секунду, и мгновенное исчезновение означало бы, что
// результат мелькнул и пропал.
func TestFinishedJobLingersThenDisappears(t *testing.T) {
	m := NewManager()
	base := time.Unix(1700000000, 0)
	cur := base
	m.now = func() time.Time { return cur }

	if _, err := m.Start("subscription", "", "Обновление", 4, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !m.Wait(time.Second) {
		t.Fatal("джоб не завершился")
	}

	if j := m.Current(); j == nil || j.State != Done {
		t.Fatalf("сразу после завершения: %+v", j)
	}

	cur = base.Add(keepFinished + time.Second)
	if j := m.Current(); j != nil {
		t.Errorf("через %v джоб всё ещё виден: %+v", keepFinished, j)
	}
}

// Клиент может уйти, а смена режима обязана довестись до конца: брошенная
// на середине, она оставила бы висячие цепочки в nftables.
func TestJobSurvivesCallerGoingAway(t *testing.T) {
	m := NewManager()
	finished := make(chan struct{})

	if _, err := m.Start("mode", "nikki", "долгая", 8, func(ctx context.Context) error {
		select {
		case <-time.After(50 * time.Millisecond):
			close(finished)
			return nil
		case <-ctx.Done():
			t.Error("операция прервана вместе с запросом")
			return ctx.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("операция не довелась до конца")
	}
}

func TestBusyReflectsRunningOnly(t *testing.T) {
	m := NewManager()
	if m.Busy() {
		t.Error("на старте занятости нет")
	}

	release := make(chan struct{})
	_, _ = m.Start("mode", "nikki", "x", 1, func(context.Context) error { <-release; return nil })
	if !m.Busy() {
		t.Error("во время операции Busy должен быть true")
	}

	close(release)
	m.Wait(time.Second)
	if m.Busy() {
		t.Error("после завершения Busy должен быть false")
	}
	// Но джоб ещё виден — это разные вещи.
	if m.Current() == nil {
		t.Error("завершённый джоб исчез сразу")
	}
}

// Панель строит подпись операции из kind и arg на своём языке, поэтому оба
// поля обязаны дожить до JSON. Пропади arg — подпись «Переключаю на b4»
// выродилась бы в безликое «Идёт операция».
func TestKindAndArgSurviveToJSON(t *testing.T) {
	m := NewManager()

	snapshot, err := m.Start("mode", "b4", "Переключение режима на b4", 8,
		func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snapshot.Arg != "b4" {
		t.Errorf("снимок при старте: arg %q, ожидалось b4", snapshot.Arg)
	}
	if !m.Wait(time.Second) {
		t.Fatal("джоб не завершился")
	}

	cur := m.Current()
	if cur == nil {
		t.Fatal("завершённый джоб пропал сразу")
	}
	if cur.Kind != "mode" || cur.Arg != "b4" {
		t.Errorf("kind=%q arg=%q, ожидалось mode/b4", cur.Kind, cur.Arg)
	}

	b, err := json.Marshal(cur)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if wire["arg"] != "b4" {
		t.Errorf("в JSON arg=%v, ожидалось b4 (поле %s)", wire["arg"], b)
	}
	// Label по-прежнему едет: его читают в syslog и в диагностике по ssh.
	if wire["label"] != "Переключение режима на b4" {
		t.Errorf("label потерялся: %v", wire["label"])
	}
}

// Уточнять в обновлении подписки нечего, но ключ arg обязан присутствовать:
// панель разбирает ответ без проверок на отсутствие поля.
func TestSubscriptionJobHasEmptyArg(t *testing.T) {
	m := NewManager()
	if _, err := m.Start("subscription", "", "Обновление подписки", 4,
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.Wait(time.Second) {
		t.Fatal("джоб не завершился")
	}

	b, err := json.Marshal(m.Current())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	v, ok := wire["arg"]
	if !ok {
		t.Fatalf("ключа arg нет вовсе: %s", b)
	}
	if v != "" {
		t.Errorf("arg=%v, ожидалась пустая строка", v)
	}
}

func TestNilWhenNothingHappened(t *testing.T) {
	if j := NewManager().Current(); j != nil {
		t.Errorf("на старте джоба быть не должно: %+v", j)
	}
}

func TestIDsAreUnique(t *testing.T) {
	m := NewManager()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		j, err := m.Start("x", "", "y", 1, func(context.Context) error { return nil })
		if err != nil {
			t.Fatalf("итерация %d: %v", i, err)
		}
		if seen[j.ID] {
			t.Fatalf("идентификатор %q повторился", j.ID)
		}
		seen[j.ID] = true
		m.Wait(time.Second)
	}
}
