package safe

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recorder — журнал демона в тесте.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// Главное требование: паника не убивает процесс, а становится ошибкой.
// Если тест дойдёт до конца — значит горутина выжила.
func TestPanicBecomesError(t *testing.T) {
	var r recorder

	err := Do(r.logf, "смена режима", func() error {
		panic("нечего применять")
	})

	if err == nil {
		t.Fatal("паника проглочена: ошибки нет, вызывающий считает операцию успешной")
	}
	if !errors.Is(err, ErrPanic) {
		t.Errorf("ошибка %v не опознаётся как паника (errors.Is с ErrPanic)", err)
	}
	// Вызывающий кладёт этот текст в поле error операции — по нему владелец
	// должен понять, что и на чём рухнуло.
	if !strings.Contains(err.Error(), "смена режима") {
		t.Errorf("в тексте ошибки нет описания операции: %q", err)
	}
	if !strings.Contains(err.Error(), "нечего применять") {
		t.Errorf("в тексте ошибки нет причины паники: %q", err)
	}
}

// Стек — единственное, по чему паника чинится. Без него в журнале остаётся
// «что-то упало», и следующий такой отказ разбирать не с чего.
func TestStackReachesLog(t *testing.T) {
	var r recorder

	_ = Do(r.logf, "обновление подписки", func() error {
		return boom()
	})

	log := r.all()
	if log == "" {
		t.Fatal("в журнале пусто: паника прошла бесследно")
	}
	if !strings.Contains(log, "goroutine") {
		t.Errorf("в журнале нет стека:\n%s", log)
	}
	// Кадр функции, которая паникнула, обязан быть в стеке: именно он
	// отвечает на вопрос «где».
	if !strings.Contains(log, "safe.boom") {
		t.Errorf("в стеке нет кадра паникующей функции:\n%s", log)
	}
	if !strings.Contains(log, "safe_test.go") {
		t.Errorf("в стеке нет файла и строки:\n%s", log)
	}
	if !strings.Contains(log, "обновление подписки") {
		t.Errorf("в журнале нет описания операции:\n%s", log)
	}
}

// nil-логгер — не экзотика, а штатный случай: так собираются все узлы демона
// в тестах и в раннем запуске. Перехватчик паники, падающий на nil, хуже,
// чем его отсутствие.
func TestNilLoggerIsSafe(t *testing.T) {
	err := Do(nil, "операция без журнала", func() error {
		var m map[string]int
		m["ключ"] = 1 // паника: запись в nil-карту
		return nil
	})
	if err == nil || !errors.Is(err, ErrPanic) {
		t.Fatalf("с nil-логгером паника не превратилась в ошибку: %v", err)
	}
}

// Штатный отказ обязан пройти насквозь: Do — не место, где ошибка операции
// подменяется на свою.
func TestNormalErrorPassesThrough(t *testing.T) {
	want := errors.New("netmode-apply вернул 1")
	var r recorder

	got := Do(r.logf, "смена режима", func() error { return want })

	if !errors.Is(got, want) {
		t.Errorf("ошибка операции подменена: %v", got)
	}
	if errors.Is(got, ErrPanic) {
		t.Error("обычный отказ помечен как паника")
	}
	if r.all() != "" {
		t.Errorf("успешный вызов написал в журнал: %s", r.all())
	}
}

func TestSuccessReturnsNil(t *testing.T) {
	if err := Do(nil, "ничего", func() error { return nil }); err != nil {
		t.Errorf("успешный вызов вернул ошибку: %v", err)
	}
}

// Паника среды выполнения (не своя строка) должна доехать до ошибки так же:
// именно такие панические значения дают будущие правки с индексами и nil.
func TestRuntimePanicIsCaught(t *testing.T) {
	var r recorder

	err := Do(r.logf, "разбор ответа", func() error {
		xs := []int{1, 2}
		i := len(xs) + 1
		_ = xs[i]
		return nil
	})

	if !errors.Is(err, ErrPanic) {
		t.Fatalf("паника среды выполнения не поймана: %v", err)
	}
	if !strings.Contains(err.Error(), "range") {
		t.Errorf("причина паники потерялась: %q", err)
	}
	if !strings.Contains(r.all(), "goroutine") {
		t.Errorf("стека для паники среды выполнения нет:\n%s", r.all())
	}
}

// boom вынесена из теста, чтобы её кадр было видно в стеке под своим именем.
func boom() error {
	panic("провод оборван")
}
