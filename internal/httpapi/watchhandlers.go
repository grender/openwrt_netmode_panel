package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"netmoded/internal/uci"
	"netmoded/internal/watch"
	"netmoded/internal/wireless"
)

// Наблюдатель трафика устройства (ADR-0043).
//
// Нового пути ЗАПИСИ здесь нет ни одного. Кнопка в строке экрана кладёт
// правило в тот же черновик своих правил, что и настройки, и применяется он
// тем же PUT /api/nikki/rulesets с отпечатком. Отдельный эндпоинт «добавить
// правило из наблюдателя» был бы вторым путём записи в mixin.yaml мимо
// черновика и мимо отпечатка.

const (
	// leasesPath — аренды dnsmasq. Договорённость с соседом, а не настройка
	// владельца: файл пишет dnsmasq, и путь у него свой. Одноимённое поле
	// Config переопределяет её только из тестов.
	leasesPath = "/tmp/dhcp.leases"
	// leasesLimit — потолок чтения файла аренд (ADR-0022). Семь аренд
	// весили 480 байт; четверть мегабайта — это тысячи устройств.
	leasesLimit = 256 << 10
)

// handleWatchHosts отдаёт список устройств.
//
// Отвечает 200 при любом режиме: список нужен панели ровно затем, чтобы
// объяснить владельцу, почему наблюдать нечего. Недостающий источник просто
// убирает свою часть данных.
func (s *Server) handleWatchHosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leases := watch.ParseLeases(s.readLeases())
	hosts := watch.MergeHosts(leases, s.wifiClients(ctx), s.talkingIPs(ctx))
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

// readLeases читает /tmp/dhcp.leases напрямую.
//
// Как mixin.yaml: это чтение файла, а не команда, и заводить ради него
// глагол Executor значило бы расширить поверхность применения ради того,
// что ничего не применяет. Файл живёт в tmpfs и переписывается на живую,
// поэтому обрезанная строка — обычное дело: разборщик её пропускает, а
// отсутствие файла даёт пустой список, а не отказ.
func (s *Server) readLeases() []byte {
	path := s.cfg.LeasesPath
	if path == "" {
		path = leasesPath
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, leasesLimit))
	if err != nil {
		return nil
	}
	return b
}

// wifiClients — MAC клиентов домашней точки.
//
// Имя интерфейса выводится из живого состояния и конфигурации, как у
// станции в статусе: литерал phy0.1-ap0 запрещён гейтом
// check-no-hardcoded-if и меняется с конфигурацией радио. Не вывелось —
// пустой список, и вид связи у всех станет «неизвестно».
func (s *Server) wifiClients(ctx context.Context) []string {
	raw, err := s.ex.UbusCall(ctx, "network.wireless", "status", nil)
	if err != nil {
		return nil
	}
	st, err := wireless.ParseStatus(raw)
	if err != nil {
		return nil
	}
	rawCfg, err := s.ex.UCIShow(ctx, "wireless")
	if err != nil {
		return nil
	}
	cfg, err := uci.ParseShow("wireless", rawCfg)
	if err != nil {
		return nil
	}
	radios := wireless.ResolveRadios(st, cfg)
	if radios.AP == "" {
		return nil
	}
	dev := wireless.IfnameForMode(st, radios.AP, "ap")
	if dev == "" {
		return nil
	}
	b, err := s.ex.UbusCall(ctx, "iwinfo", "assoclist", map[string]any{"device": dev})
	if err != nil {
		return nil
	}
	return wireless.ParseAssocList(b)
}

// talkingIPs — кто говорит прямо сейчас, по снимку соединений.
func (s *Server) talkingIPs(ctx context.Context) []string {
	snap, err := s.nikki.Connections(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range snap.Connections {
		if c.SourceIP == "" || seen[c.SourceIP] {
			continue
		}
		seen[c.SourceIP] = true
		out = append(out, c.SourceIP)
	}
	return out
}

// handleWatchGet отдаёт состояние сессии и продлевает её жизнь.
func (s *Server) handleWatchGet(w http.ResponseWriter, r *http.Request) {
	sess := s.currentWatch()
	if sess == nil {
		writeJSON(w, http.StatusOK, watch.State{Active: false})
		return
	}
	writeJSON(w, http.StatusOK, sess.Poll())
}

// handleWatchStop гасит наблюдение.
//
// Единственный 204 в этом API. Правило «тело есть всегда» из
// docs/contracts/errors.md — про тела ОШИБОК; отдавать здесь состояние,
// которое запрос только что сделал невозможным, было бы странно.
// Cache-Control ставится руками ровно потому, что путь не идёт через
// writeJSON, который ставит его всем остальным.
func (s *Server) handleWatchStop(w http.ResponseWriter, _ *http.Request) {
	s.stopWatch()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleWatchStart заводит сессию.
//
// Порядок проверок — от бесполезного к невозможному, как у записи наборов:
// ни одна не должна пройти после того, как что-то уже началось.
func (s *Server) handleWatchStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var in struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "тело запроса не разобрано")
		return
	}
	ip := net.ParseIP(strings.TrimSpace(in.IP))
	if ip == nil || ip.To4() == nil {
		// IPv6 назван отдельно: у устройств есть ULA-адреса, и отказ без
		// объяснения выглядел бы как опечатка владельца.
		writeErr(w, http.StatusBadRequest, "bad_ip",
			"нужен адрес IPv4 устройства из локальной сети: наблюдение привязано к одному адресу")
		return
	}

	mode, err := s.ex.UCIGet(ctx, "netmode", "main", "mode")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "uci_unavailable", err.Error())
		return
	}
	if mode != "nikki" {
		writeErr(w, http.StatusConflict, "engine_off",
			"наблюдатель читает решения движка Nikki; сейчас режим другой, и решений нет")
		return
	}

	rawNet, err := s.ex.UCIShow(ctx, "network")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "uci_unavailable", err.Error())
		return
	}
	netCfg, err := uci.ParseShow("network", rawNet)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "parse_failed", err.Error())
		return
	}
	// Подсеть выводится, а не угадывается: демон не сочиняет чужую
	// конфигурацию (ADR-0019), и не вывелась она — это отказ, а не повод
	// пустить любой адрес.
	subnet := lanSubnetFrom(netCfg)
	if subnet == "" {
		writeErr(w, http.StatusServiceUnavailable, "lan_unknown",
			"подсеть локальной сети не вывелась из network.lan")
		return
	}
	_, lan, err := net.ParseCIDR(subnet)
	if err != nil || !lan.Contains(ip) {
		writeErr(w, http.StatusBadRequest, "bad_ip",
			"адрес не из локальной сети "+subnet)
		return
	}

	// Сессия ЗАМЕНЯЕТСЯ, а не встаёт в очередь: два потока к движку сразу
	// подписались бы оба и гонялись бы за одну таблицу.
	s.stopWatch()
	sess := watch.Start(context.Background(), watch.Config{
		IP:   ip.String(),
		Src:  s.nikki,
		Mode: func(c context.Context) (string, error) { return s.ex.UCIGet(c, "netmode", "main", "mode") },
		Logf: s.logf,
	})
	s.watchMu.Lock()
	s.watch = sess
	s.watchMu.Unlock()

	writeJSON(w, http.StatusOK, sess.State())
}

// currentWatch отдаёт живую сессию либо nil.
//
// Догоревшую по TTL сессию здесь же и забываем: иначе указатель на неё жил
// бы до следующего POST, и «закрыл вкладку — память отдана» было бы правдой
// только наполовину.
func (s *Server) currentWatch() *watch.Session {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if s.watch == nil {
		return nil
	}
	select {
	case <-s.watch.Done():
		s.watch = nil
		return nil
	default:
	}
	return s.watch
}

// stopWatch гасит сессию и дожидается её горутин.
func (s *Server) stopWatch() {
	s.watchMu.Lock()
	sess := s.watch
	s.watch = nil
	s.watchMu.Unlock()
	if sess != nil {
		sess.Stop()
	}
}

// watchSummary — строка полки главной. nil, когда сессии нет.
//
// Зовёт State, а не Poll: это чтение ради показа, а не признак того, что
// за наблюдением следят. Продлевай оно TTL, поток к движку жил бы, пока
// открыта любая вкладка панели.
func (s *Server) watchSummary() *WatchSummary {
	sess := s.currentWatch()
	if sess == nil {
		return nil
	}
	st := sess.State()
	if !st.Active {
		return nil
	}
	bad := 0
	for _, t := range st.Targets {
		if t.Verdict != watch.VerdictOK {
			bad++
		}
	}
	return &WatchSummary{IP: st.IP, Since: st.Since, Engine: st.Engine, Problems: bad}
}
