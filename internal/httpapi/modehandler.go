package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"netmoded/internal/job"
	"netmoded/internal/led"
)

// modeETASec — сколько примерно занимает переключение (SPEC §5: 5–15 с).
const modeETASec = 15

// handleMode переключает режим обработки трафика.
//
// Медленная операция: джоб плюс светодиод, отката нет — связь при смене
// режима не рвётся, рвётся только туннель.
func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	switch in.Mode {
	case "nikki", "b4", "off":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request",
			"Режим должен быть nikki, b4 или off")
		return
	}

	mode := in.Mode
	label := "Переключение режима на " + mode
	if mode == "off" {
		label = "Выключение обхода"
	}

	j, err := s.jobs.Start("mode", label, modeETASec, func(ctx context.Context) error {
		return s.applyMode(ctx, mode)
	})
	if errors.Is(err, job.ErrBusy) {
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

// applyMode выполняет переключение.
//
// Порядок именно такой: сначала намерение в UCI, потом скрипт. UCI —
// источник намерения, скрипт приводит систему к намерению. Упал скрипт —
// намерение сохранено, джоб в состоянии failed, повтор возможен, и
// netmode-apply при следующей загрузке доведёт дело сам.
//
// Обратный порядок означал бы, что после успешного применения и упавшей
// записи система работает не в том режиме, который записан, — и после
// перезагрузки молча вернулась бы к прежнему.
func (s *Server) applyMode(ctx context.Context, mode string) error {
	// Индикация показывает ЦЕЛЬ, а не факт: пользователь должен видеть,
	// что нажатие принято, ещё до того как переключение завершится.
	s.setLED(led.ApplyingForMode(mode))

	if err := s.ex.UCISet(ctx, "netmode", "main", "mode", mode); err != nil {
		s.flashLED(mode)
		return err
	}
	if err := s.ex.UCICommit(ctx, "netmode"); err != nil {
		s.flashLED(mode)
		return err
	}

	if err := s.ex.ApplyMode(ctx, mode); err != nil {
		// Намерение записано, система к нему не пришла. Светодиод мигает
		// ошибкой три секунды и возвращается к ЗАПИСАННОМУ режиму —
		// не к фактическому: панель и индикация должны говорить одно.
		s.flashLED(mode)
		return err
	}

	s.setLED(led.StateForMode(mode))
	return nil
}

// setLED и flashLED глотают ошибки индикации.
//
// Погасший светодиод — неудобство; сорванное переключение режима из-за
// него — потеря связи (ADR-0013). И журнал, и признак деградации для
// /api/status — целиком забота контроллера: он говорит об отказе ОДИН раз,
// а здесь мы на каждом переключении писали бы одну и ту же строку, в
// которой утонет что-то важное.
func (s *Server) setLED(st led.State) {
	if s.led == nil {
		return
	}
	_ = s.led.Set(st)
}

func (s *Server) flashLED(mode string) {
	if s.led == nil {
		return
	}
	s.led.Flash(led.StateForMode(mode))
}
