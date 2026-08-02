package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"netmoded/internal/nikki"
)

// ProxyGroup — группа, из которой панель выбирает узел.
//
// Имя фиксировано профилем mihomo (SPEC §2), а не настраивается: сделать
// его опцией значило бы дать способ выбрать чужую группу и сломать
// fail-closed, не заметив этого.
const ProxyGroup = "PROXY"

func (s *Server) handleNikkiProxies(w http.ResponseWriter, r *http.Request) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}

	all, err := s.nikki.Proxies(r.Context())
	if err != nil {
		if errors.Is(err, nikki.ErrUnavailable) {
			writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable",
				"Clash API не отвечает. Узлы недоступны, режим переключается по-прежнему.")
			return
		}
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}

	g, ok := all[ProxyGroup]
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "group_missing",
			"В профиле mihomo нет группы "+ProxyGroup)
		return
	}

	ver := ""
	if v, err := s.nikki.Version(r.Context()); err == nil {
		ver = v
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"version":   ver,
		"group":     ProxyGroup,
		"type":      g.Type,
		"selected":  g.Now,
		// Закреплён вручную или выбран движком — по одному selected это
		// неразличимо, а для панели разница принципиальна: закрепление
		// временное, и возврат к AUTO надо держать на виду.
		"fixed":      g.Fixed,
		"pinned":     g.Pinned,
		"selectable": g.Selectable,
		"members":    nikki.Members(all, ProxyGroup),
	})
}

// AutoSentinel — значение, возвращающее группу к автовыбору.
//
// Имя из SPEC §7: спека предполагала, что в профиле придётся завести
// вложенную группу AUTO. Разбор исходников mihomo показал, что этого не
// нужно — движок умеет снимать закрепление сам
// (DELETE /proxies/{группа} → ForceSet("")). Внешний контракт спеки
// сохранён, реализация оказалась проще.
const AutoSentinel = "AUTO"

// handleNikkiProxy — быстрая операция (SPEC §5): без джоба и светодиода.
func (s *Server) handleNikkiProxy(w http.ResponseWriter, r *http.Request) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}

	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Не указано имя узла")
		return
	}

	var err error
	if in.Name == AutoSentinel {
		err = s.nikki.Unfix(r.Context(), ProxyGroup)
	} else {
		err = s.nikki.Select(r.Context(), ProxyGroup, in.Name)
	}
	switch {
	case err == nil:
	case errors.Is(err, nikki.ErrNotSelectable):
		// 409, а не 501: конфликт с текущей конфигурацией, а не
		// нереализованная возможность. Чинится правкой профиля.
		writeErr(w, http.StatusConflict, "group_not_selectable", err.Error())
		return
	case errors.Is(err, nikki.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	case errors.Is(err, nikki.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Clash API не отвечает")
		return
	default:
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}

	s.handleNikkiProxies(w, r)
}
