package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"netmoded/internal/job"
	"netmoded/internal/logs"
	"netmoded/internal/sched"
)

// handleSubscriptionUpdate запускает обновление подписки.
//
// Медленная операция (2–10 с по SPEC §5): идёт джобом, но БЕЗ отката —
// откатывать нечего, предохранитель внутри happ2clash сам не даёт
// перезаписать файл провайдера пустым результатом.
func (s *Server) handleSubscriptionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "Планировщик не настроен")
		return
	}

	// Arg пустой: обновление подписки одно, уточнять в нём нечего.
	j, err := s.jobs.Start("subscription", "", "Обновление подписки", 10,
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

var _ = sched.DefaultInterval
