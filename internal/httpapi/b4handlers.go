package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"netmoded/internal/b4"
)

// handleB4Sets отдаёт список сетов.
//
// Ответ проецируется до {id, name, enabled}: каждый сет в b4 тащит ~2.5 КБ
// настроек стратегии (raw/51-b4-sets.json), а панель опрашивает статус раз
// в секунду. Проксировать это наружу означало бы гонять мегабайты в час
// ради трёх полей.
func (s *Server) handleB4Sets(w http.ResponseWriter, r *http.Request) {
	if s.b4 == nil {
		writeErr(w, http.StatusServiceUnavailable, "b4_unavailable", "Клиент b4 не настроен")
		return
	}

	sets, err := s.b4.Sets(r.Context())
	if err != nil {
		if errors.Is(err, b4.ErrUnavailable) {
			// b4 перезапускается сам — в разведке он как раз лежал
			// (raw/25-netstat.txt). Это не сбой демона, и панель обязана
			// продолжать работать, погасив чипы сетов.
			writeErr(w, http.StatusServiceUnavailable, "b4_unavailable",
				"Панель b4 не отвечает. Сеты недоступны, режим переключается по-прежнему.")
			return
		}
		writeErr(w, http.StatusBadGateway, "b4_error", err.Error())
		return
	}

	// Версия не обязательна: список сетов уже получен, и терять его из-за
	// неудачной пробы версии было бы обменом важного на украшение.
	ver := ""
	if v, err := s.b4.Version(r.Context()); err == nil {
		ver = v.Version
	}

	// Все ключи пишутся всегда, включая пустые. `selected: ""` и
	// `enabled_count: 0` — это ответ «включённых сетов нет», а не молчание;
	// пропусти мы их, клиент не отличил бы одно от другого (openapi.yaml,
	// SetsResponse: все поля required).
	writeJSON(w, http.StatusOK, map[string]any{
		"available":     true,
		"version":       ver,
		"selected":      b4.Selected(sets),
		"enabled_count": b4.EnabledCount(sets),
		"sets":          sets,
	})
}

// handleB4Set переключает сет.
//
// Быстрая операция (SPEC §5): без джоба, без светодиода, синхронный ответ.
// b4 применяет изменение горячо и сам пишет его на диск
// (src/http/handler/config.go:439), поэтому ни рестарта, ни дублирования
// в b4.json не требуется.
func (s *Server) handleB4Set(w http.ResponseWriter, r *http.Request) {
	if s.b4 == nil {
		writeErr(w, http.StatusServiceUnavailable, "b4_unavailable", "Клиент b4 не настроен")
		return
	}

	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	if in.ID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"Не указан id сета. Имя ключом не является: в b4 оно не уникально.")
		return
	}

	err := s.b4.SelectOnly(r.Context(), in.ID)
	switch {
	case err == nil:
	case errors.Is(err, b4.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "Сет не найден")
		return
	case errors.Is(err, b4.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "b4_unavailable",
			"Панель b4 не отвечает")
		return
	default:
		writeErr(w, http.StatusBadGateway, "b4_error", err.Error())
		return
	}

	// Отдаём обновлённый список: панели он нужен сразу, а второй запрос —
	// лишняя гонка с изменениями из веб-морды b4.
	s.handleB4Sets(w, r)
}
