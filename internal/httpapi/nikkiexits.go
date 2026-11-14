package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"netmoded/internal/exits"
	"netmoded/internal/mixin"
	"netmoded/internal/nikki"
)

// Свои выходы в карточке Nikki: какие группы EXIT-* есть у движка, через
// что каждая работает сейчас, и ручной выбор «прямо / через хоп / закрыть»
// (ADR-0050, docs/exit-nodes.md). Выбор — Select в Clash API, как у PROXY:
// живёт до перезапуска движка, профиль не трогается.

// exitMemberKind — вид участника группы выхода для панели.
func exitMemberKind(name string, p nikki.Proxy, known bool) string {
	switch {
	case name == "REJECT":
		return "reject"
	case known && p.IsGroup():
		return "chains"
	case strings.Contains(name, exits.ChainSep):
		return "chain"
	default:
		return "direct"
	}
}

// exitGroups — группы EXIT-*, которые сами участниками других EXIT-* не
// являются: EXIT-DC-HOP — часть выхода EXIT-DC, а не отдельный выход.
func exitGroups(all map[string]nikki.Proxy) []map[string]any {
	inner := map[string]bool{}
	for name, p := range all {
		if !strings.HasPrefix(name, mixin.ExitGroupPrefix) || !p.IsGroup() {
			continue
		}
		for _, m := range p.Members {
			if strings.HasPrefix(m, mixin.ExitGroupPrefix) {
				inner[m] = true
			}
		}
	}
	var names []string
	for name, p := range all {
		if mixin.ValidExitGroup(name) && p.IsGroup() && !inner[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		g := all[name]
		members := make([]map[string]any, 0, len(g.Members))
		for _, m := range g.Members {
			p, known := all[m]
			row := map[string]any{"name": m, "kind": exitMemberKind(m, p, known),
				"delay_ms": p.DelayMS, "alive": p.Alive}
			if known && p.IsGroup() {
				row["now"] = p.Now
				// У url-test-группы своей истории часто нет — берём замер
				// цепочки, через которую она работает.
				if p.DelayMS == nil {
					if cur, ok := all[p.Now]; ok {
						row["delay_ms"], row["alive"] = cur.DelayMS, cur.Alive
					}
				}
			}
			members = append(members, row)
		}
		out = append(out, map[string]any{"name": name, "type": g.Type, "now": g.Now, "members": members})
	}
	return out
}

// handleNikkiExits — GET /api/nikki/exits.
func (s *Server) handleNikkiExits(w http.ResponseWriter, r *http.Request) {
	s.writeExits(w, r)
}

func (s *Server) writeExits(w http.ResponseWriter, r *http.Request) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}
	all, err := s.nikki.Proxies(r.Context())
	if err != nil {
		if errors.Is(err, nikki.ErrUnavailable) {
			writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Clash API не отвечает.")
			return
		}
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": exitGroups(all)})
}

// handleNikkiExitSelect — POST /api/nikki/exits/select.
//
// Только группы EXIT-* и только их участники: эндпоинт не должен стать
// вторым путём перещёлкнуть PROXY или GLOBAL мимо их правил.
func (s *Server) handleNikkiExitSelect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Group  string `json:"group"`
		Member string `json:"member"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Group == "" || in.Member == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Нужны group и member")
		return
	}
	if !mixin.ValidExitGroup(in.Group) {
		writeErr(w, http.StatusBadRequest, "bad_group", "Переключать здесь можно только группы своих выходов (EXIT-…)")
		return
	}
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}
	all, err := s.nikki.Proxies(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Clash API не отвечает.")
		return
	}
	g, ok := all[in.Group]
	if !ok || !g.IsGroup() {
		writeErr(w, http.StatusNotFound, "unknown_group", "У движка нет группы «"+in.Group+"»")
		return
	}
	member := false
	for _, m := range g.Members {
		if m == in.Member {
			member = true
		}
	}
	if !member {
		writeErr(w, http.StatusBadRequest, "unknown_member", "«"+in.Member+"» не участник группы «"+in.Group+"»")
		return
	}
	if err := s.nikki.Select(r.Context(), in.Group, in.Member); err != nil {
		if errors.Is(err, nikki.ErrNotSelectable) {
			writeErr(w, http.StatusConflict, "group_not_selectable", err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}
	s.writeExits(w, r)
}
