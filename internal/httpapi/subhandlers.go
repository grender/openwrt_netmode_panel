package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"netmoded/internal/job"
	"netmoded/internal/logs"
)

// subscriptionETASec — оценка длительности обновления (SPEC §5: 2–10 с).
const subscriptionETASec = 10

// handleSubscriptionUpdate запускает обновление подписки.
//
// Медленная операция (2–10 с по SPEC §5): демон скачивает подписку у
// провайдера, разбирает её, пишет файл узлов для mihomo и свой манифест
// порядка, после чего просит движок перечитать провайдера. Идёт джобом, но
// БЕЗ отката — откатывать нечего: предохранитель subs.Update не даёт
// перезаписать файл провайдера пустым результатом, и при нуле разобранных
// узлов оба файла остаются нетронутыми (ADR-0031).
func (s *Server) handleSubscriptionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "Планировщик не настроен")
		return
	}

	// Пустой адрес отбивается ЗДЕСЬ, а не внутри джоба.
	//
	// Внутри он тоже проверяется (subs.ErrNotConfigured), но джоб доложил бы
	// об этом состоянии строкой «fail» в журнале обновлений — то есть
	// нормальная свежая установка копила бы историю неудач при каждом
	// нажатии, и журнал перестали бы читать. Ответ 409 говорит владельцу
	// то же самое, ничего не записав.
	//
	// Читается живое значение, а не UCI и не cfg. UCI — потому что демон
	// всё равно работает со своим значением, и живое чтение обещало бы
	// панели работающую кнопку там, где обновление откажет. cfg — потому
	// что с появлением PUT /api/subscription (ADR-0034) там лежит снимок,
	// сделанный при старте: владелец задал бы адрес и продолжал получать
	// «не задан» до перезапуска демона.
	if !s.subURL.configured() {
		writeErr(w, http.StatusConflict, "subscription_not_configured",
			"Адрес подписки не задан. Задайте его в панели — раздел «Подписка», "+
				"или на роутере: uci set netmode.main.subscription_url='…' && uci commit netmode")
		return
	}

	// Arg пустой: обновление подписки одно, уточнять в нём нечего.
	j, err := s.jobs.Start("subscription", "", "Обновление подписки", subscriptionETASec,
		func(ctx context.Context) error {
			_, err := s.sched.RunOnce(ctx)
			return err
		})
	if errors.Is(err, job.ErrBusy) {
		// 409, а не очередь: пользователь нажал кнопку сейчас, и начать
		// операцию через минуту — не то, о чём он просил (SPEC §6).
		writeErr(w, http.StatusConflict, "job_busy",
			"Уже идёт другая операция. Дождитесь её завершения.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"job": j})
}

// handleLogs отдаёт журнал обновлений.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			writeErr(w, http.StatusBadRequest, "bad_request",
				"Параметр n должен быть положительным числом")
			return
		}
		if parsed > logs.MaxLines {
			parsed = logs.MaxLines
		}
		n = parsed
	}

	lines, err := s.logs.Tail(n)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	if lines == nil {
		// Пустой массив, а не null: панель перебирает список, и null
		// заставил бы её отдельно это проверять.
		lines = []logs.Entry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":  s.logs.Path,
		"n":     len(lines),
		"lines": lines,
	})
}
