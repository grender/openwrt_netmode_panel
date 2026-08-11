package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"netmoded/internal/executor"
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
	// Label остаётся русским: он читается в syslog и в диагностике по ssh.
	// Панель его не показывает — она строит подпись из kind и arg сама.
	label := "Переключение режима на " + mode
	if mode == "off" {
		label = "Выключение обхода"
	}

	j, err := s.jobs.Start("mode", mode, label, modeETASec, func(ctx context.Context) error {
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
		return s.modeApplyFailed(mode, err)
	}

	s.setLED(led.StateForMode(mode))
	return nil
}

// modeApplyFailed различает исходы netmode-apply.
//
// Скрипт возвращает разные коды для разных причин (см. его шапку и
// docs/contracts/executor.md), и реакция на них разная: «занято» ничего не
// сломало, «firewall не перезапустился» требует внимания владельца прямо
// сейчас. Раньше все причины сводились к одной ошибке, и отличить их можно
// было только чтением текста.
//
// Повторов здесь нет и не будет: демон не приводит систему в порядок сам
// (ADR-0010). Единственный повтор — вторая попытка firewall restart ВНУТРИ
// скрипта. Это не откат (ADR-0006, check-no-rollback): ничего не
// возвращается в прежнее состояние, повторяется та же самая операция, и
// живёт она там, где видна в логе одной строкой.
//
// Текст ошибки уходит в job.error и показывается в панели, поэтому он
// говорит не «код 4», а что именно с системой сейчас происходит.
func (s *Server) modeApplyFailed(mode string, err error) error {
	switch {
	case errors.Is(err, executor.ErrApplyBusy):
		// Скрипт держит flock: тем же занят кто-то другой — человек из ssh
		// или применение при загрузке. Ничего не сломано и ничего не
		// изменено, поэтому индикация ошибки была бы прямой ложью: она
		// означает «переключение провалилось», а оно не начиналось.
		// Светодиод всё равно уводится с мигающего «применяю» к
		// ЗАПИСАННОМУ режиму — иначе он мигал бы целью, которой никто не
		// добивается.
		s.setLED(led.StateForMode(mode))
		return fmt.Errorf("переключение не начиналось: оно уже идёт в другом процессе, система не тронута: %w", err)

	case errors.Is(err, executor.ErrApplyFirewall):
		// Худший из исходов, требующих владельца: цепочки в nftables могли
		// остаться от прошлого режима. Скрипт намеренно НЕ стал запускать
		// туннель поверх них — иначе панель показывала бы работающий режим,
		// а трафик шёл мимо. Сейчас обход выключен целиком.
		s.flashLED(mode)
		return fmt.Errorf("режим не включён, обход выключен — трафик идёт напрямую: %w", err)

	case errors.Is(err, executor.ErrApplyVerify):
		// Скрипт отработал, но проверка показала не то, что просили.
		// Автопочинки нет (ADR-0010): состояние докладывается как есть.
		s.flashLED(mode)
		return fmt.Errorf("система оказалась не в запрошенном состоянии: %w", err)

	case errors.Is(err, executor.ErrApplyStart):
		s.flashLED(mode)
		return fmt.Errorf("сервис режима не поднялся: %w", err)

	case errors.Is(err, executor.ErrApplyPrereq):
		// Чинится доставкой пакета на роутер, а не повтором запроса.
		s.flashLED(mode)
		return fmt.Errorf("переключение невозможно без предусловия на роутере: %w", err)

	case errors.Is(err, executor.ErrNoExecutor):
		// Самого netmode-apply нет на роутере: половинчатая установка —
		// демон приехал, скрипт слоя 2 нет. Отдельно от ErrApplyPrereq,
		// потому что о нехватке flock докладывает сам скрипт кодом 7, а
		// скрипт, которого нет, доложить о себе не может.
		//
		// Таксономии кодов у режимов нет (last_fail — только у переключения
		// сети), поэтому здесь разница выражается формулировкой и ничем
		// больше. Голая ошибка exec на её месте выглядела бы как «внутренний
		// сбой» и уводила бы искать поломку в демоне.
		//
		// Владелец узнаёт об этом и до нажатия — по missing_executors в
		// /api/status, — но узнать после тоже обязан: баннер он мог не
		// заметить, а сюда пришёл целенаправленно.
		s.flashLED(mode)
		return fmt.Errorf("режим не переключён: на роутере нет скрипта применения, "+
			"переустановите пакет (deploy.sh --install): %w", err)

	default:
		// Скрипт отвалился по таймауту или вернул код, которого нет в
		// контракте. Намерение записано, система к нему не пришла.
		s.flashLED(mode)
		return err
	}
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
