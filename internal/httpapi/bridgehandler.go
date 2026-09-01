package httpapi

// Проброс LAN в uplink (ADR-0030): GET состояния с диагностикой и три
// операции-джоба — enable, disable, access. Дисциплина — дословно по
// upstreamhandler.go: все отказы ДО первой записи, партия → revert до
// коммита → по одному коммиту на пакет → применение скриптом слоя 2 →
// вердикт ПО НАБЛЮДЕНИЮ, никогда по коду возврата.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"netmoded/internal/executor"
	"netmoded/internal/job"
	"netmoded/internal/netif"
	"netmoded/internal/uci"
	"netmoded/internal/wireless"
)

// ─────────── таксономия неудач моста ───────────

// Свой закрытый набор, НЕ пересекающийся с upstream-таксономией по смыслу
// использования: те же правила, что у allReasons, — новая причина обязана
// одновременно попасть в openapi (enum BridgeLastFail.reason), в обе локали
// i18n.js (bridge.fail.*), в BRIDGE_REASONS панели и в мок; сверяет
// scripts/check-fail-reasons.sh вторым прогоном.
//
// Границы между близкими:
//   - apply_failed — операция не состоялась по нашей стороне (запись,
//     отпечаток, коммит, отказ reload'ов). Система осталась прежней либо
//     закоммиченное ждёт чужого применения — текст уточняет;
//   - no_iface — применение прошло, а интерфейс homelan не пришёл в
//     ожидаемое состояние за окно наблюдения (для enable — не поднялся с
//     нужным адресом, для disable — не исчез);
//   - relay_down — интерфейс в порядке, а процесс relayd не в ожидаемом
//     состоянии (для enable — не запустился, для disable — не остановился,
//     код 5 скрипта приходит сюда же);
//   - unverifiable — прочитать исход не удалось ни разу за окно: мы не
//     знаем, что вышло, и говорим именно это;
//   - install_failed — apk add relayd отказал; конфигурация не тронута
//     вовсе (установка идёт ДО первой записи uci).
const (
	breasonApplyFailed     FailReason = "apply_failed"
	breasonBusy            FailReason = "busy"
	breasonPrereqMissing   FailReason = "prereq_missing"
	breasonExecutorMissing FailReason = "executor_missing"
	breasonInstallFailed   FailReason = "install_failed"
	breasonNoIface         FailReason = "no_iface"
	breasonRelayDown       FailReason = "relay_down"
	breasonUnverifiable    FailReason = "unverifiable"
	breasonStaleDraft      FailReason = "stale_draft"
)

// allBridgeReasons — единственное перечисление таксономии моста (то же
// правило, что allReasons: причина не отсюда для демона не существует).
var allBridgeReasons = []FailReason{
	breasonApplyFailed,
	breasonBusy,
	breasonPrereqMissing,
	breasonExecutorMissing,
	breasonInstallFailed,
	breasonNoIface,
	breasonRelayDown,
	breasonUnverifiable,
	breasonStaleDraft,
}

func knownBridgeReason(r FailReason) bool {
	for _, known := range allBridgeReasons {
		if r == known {
			return true
		}
	}
	return false
}

// ─────────── слот последней неудачи ───────────

// BridgeFail — последняя неудачная операция моста. Свой тип, а не Fail:
// у моста нет ssid, есть действие (enable/disable/access-on/access-off).
type BridgeFail struct {
	Action string     `json:"action"`
	Reason FailReason `json:"reason"`
	// Detail — человекочитаемая подробность; тот же текст уходит в
	// job.error и в syslog.
	Detail string `json:"detail,omitempty"`
	At     string `json:"at"`
}

// bridgeFailStore — второй одинслотный слот по образцу failStore и ровно
// по той же причине (ADR-0025): job живёт в статусе пять секунд, а браузер,
// потерявший связь на время network reload, вернётся позже.
type bridgeFailStore struct {
	mu   sync.Mutex
	fail *BridgeFail
}

func (s *bridgeFailStore) Set(action string, reason FailReason, detail string, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = &BridgeFail{Action: action, Reason: reason, Detail: detail, At: at.UTC().Format(time.RFC3339)}
}

func (s *bridgeFailStore) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = nil
}

func (s *bridgeFailStore) Get() *BridgeFail {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == nil {
		return nil
	}
	cp := *s.fail
	return &cp
}

// ─────────── пороги и оценки ───────────

// Переменные, не константы, — тесты сжимают окна (прецедент
// upstreamAssocTimeout, там же и довод).
var (
	// Окно подъёма/исчезновения homelan после network reload. Живой замер:
	// netifd поднимает статический интерфейс за доли секунды после reload
	// (raw/82: uptime начинает тикать сразу); 15 с — запас на медленную
	// флешку и занятый netifd.
	bridgeIfaceTimeout = 15 * time.Second
	// Окно старта/останова relayd: его запускает netifd протоколом relay
	// после подъёма интерфейса (raw/84).
	bridgeRelaydTimeout = 10 * time.Second
	bridgePollInterval  = 500 * time.Millisecond

	bridgeEnableETASec  = 30
	bridgeInstallETASec = 60 // enable с установкой relayd: apk качает из сети
	bridgeDisableETASec = 20
	bridgeAccessETASec  = 10
)

// Кэш проб: ping — секунды, а GET c ?probe=1 панель может слать при каждом
// заходе на вкладку. Десять секунд — дольше одного захода, короче смысла.
var bridgeProbeTTL = 10 * time.Second

// ─────────── формы запросов и ответов ───────────

type BridgeEnable struct {
	LegIP    string `json:"leg_ip"`
	PCIP     string `json:"pc_ip"`
	APAccess bool   `json:"ap_access"`
	// Install — явное согласие владельца на apk add relayd (ADR-0030).
	// Панель шлёт true только после confirm; без него отсутствие пакета —
	// отказ relayd_missing, и не сделано НИЧЕГО.
	Install bool `json:"install"`
}

type BridgeAccess struct {
	Enabled bool `json:"enabled"`
}

type BridgeState struct {
	// Fingerprint — оптимистичная блокировка над ПАРОЙ пакетов
	// network+firewall: операции моста пишут в оба, и правка любого из них
	// в LuCI обязана уронить If-Match.
	Fingerprint string `json:"fingerprint"`
	Enabled     bool   `json:"enabled"`
	LegIP       string `json:"leg_ip,omitempty"`
	PCIP        string `json:"pc_ip,omitempty"`
	APAccess    bool   `json:"ap_access"`

	// nil = «спросили, не ответило» — панель рисует отказ, а не выдумку
	// (та же трёхзначная договорённость, что у списков).
	Relayd *BridgeRelayd `json:"relayd"`
	Port   *BridgePort   `json:"port"`
	Uplink *BridgeUplink `json:"uplink"`

	Probes   *BridgeProbes `json:"probes,omitempty"`
	LastFail *BridgeFail   `json:"last_fail"`
}

type BridgeRelayd struct {
	Installed bool `json:"installed"`
	Running   bool `json:"running"`
}

type BridgePort struct {
	Name    string `json:"name"`
	Carrier bool   `json:"carrier"`
	// Указатели: при лежащем линке скорость честно неизвестна (sysfs отдаёт
	// -1, raw/83), и ноль был бы выдуманным числом.
	SpeedMbps      *int `json:"speed_mbps"`
	CarrierChanges *int `json:"carrier_changes"`
}

type BridgeUplink struct {
	Up      bool   `json:"up"`
	Address string `json:"address,omitempty"`
	Mask    int    `json:"mask,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	Device  string `json:"device,omitempty"`
}

type BridgeProbes struct {
	PC      *BridgeProbeResult `json:"pc"`
	Gateway *BridgeProbeResult `json:"gateway"`
	At      string             `json:"at"`
}

type BridgeProbeResult struct {
	Answered bool `json:"answered"`
}

// bridgeScriptStatus — JSON глагола `netmode-bridge status` как есть.
type bridgeScriptStatus struct {
	RelaydInstalled bool `json:"relayd_installed"`
	RelaydRunning   bool `json:"relayd_running"`
	Carrier         int  `json:"carrier"`
	SpeedMbps       *int `json:"speed_mbps"`
	CarrierChanges  *int `json:"carrier_changes"`
}

// ─────────── разбор конфигурации моста ───────────

// bridgeConfig — всё, что операции моста выводят из живой конфигурации.
// Ничего не запоминается между запросами (uplink-подсеть менялась прямо во
// время разведки — raw/86): каждый запрос читает заново.
type bridgeConfig struct {
	rawNet, rawFw []byte
	netCfg, fwCfg *uci.Config
	fp            string

	// baseDev — имя бриджа LAN, ВЫВЕДЕННОЕ из network.lan.device (сам
	// device либо он же с VLAN-суффиксом). Литерала br-lan в коде нет.
	baseDev string
	// port — единственный порт бриджа; пуст, если портов не ровно один.
	port      string
	portCount int

	enabled bool
	legIP   string

	// Демонтаж ищет секции ПО ТИПУ И СОДЕРЖИМОМУ (ADR-0030): живой роутер
	// несёт ручную сборку с анонимными секциями, и панель обязана уметь
	// разобрать её, а не только своё.
	vlanSections  []string // bridge-vlan на baseDev, в порядке файла
	homelanRoutes []string // route через homelan, в порядке файла
	relayRules    []string // rule с lookup в таблицы relayd, в порядке файла
	devSection    string   // секция device самого бриджа
	homelanZones  []string // зоны с name='homelan', в порядке файла
	homelanFwds   []string // форвардинги, касающиеся homelan
	lanFwd        string   // форвардинг lan→homelan (признак ap_access)
	wanZone       string   // секция зоны wan
	wanMasqNeg    []string // элементы masq_src зоны wan вида '!a.b.c.d/nn'
}

var negCIDR = regexp.MustCompile(`^!(\d{1,3}\.){3}\d{1,3}/\d{1,2}$`)

// База таблиц маршрутизации relayd.
//
// Не выдумка и не подсмотренная случайность: значение объявлено собственной
// validate-схемой relayd — `/etc/init.d/relayd`, строка
// `'table:range(0, 65535):16800'` (raw/88). relayd заводит по таблице на
// каждый мостуемый интерфейс: база и база+1.
//
// Пин через `option table` рассмотрен и отвергнут: relayd — отдельный
// procd-сервис, и `ubus call network reload` его НЕ перезапускает
// (измерено, raw/88 §6-7) — то есть пин требовал бы расширить глагол
// скрипта до перезапуска сервиса. Умолчание же объявлено самим relayd и
// цитируется в evidence.
const relaydTableBase = 16800

// vlanBase отрезает VLAN-суффикс: 'br-lan.1' → 'br-lan', 'br-lan' → сам.
func vlanBase(dev string) string {
	i := strings.LastIndexByte(dev, '.')
	if i <= 0 {
		return dev
	}
	for _, r := range dev[i+1:] {
		if r < '0' || r > '9' {
			return dev
		}
	}
	return dev[:i]
}

// readBridgeConfig читает и разбирает оба пакета. Единственный источник
// правды операций моста (ADR-0002).
func (s *Server) readBridgeConfig(ctx context.Context) (*bridgeConfig, *httpErr) {
	rawNet, err := s.ex.UCIShow(ctx, "network")
	if err != nil {
		return nil, &httpErr{http.StatusServiceUnavailable, "uci_unavailable", err.Error()}
	}
	rawFw, err := s.ex.UCIShow(ctx, "firewall")
	if err != nil {
		return nil, &httpErr{http.StatusServiceUnavailable, "uci_unavailable", err.Error()}
	}
	netCfg, err := uci.ParseShow("network", rawNet)
	if err != nil {
		return nil, &httpErr{http.StatusInternalServerError, "parse_failed", err.Error()}
	}
	fwCfg, err := uci.ParseShow("firewall", rawFw)
	if err != nil {
		return nil, &httpErr{http.StatusInternalServerError, "parse_failed", err.Error()}
	}

	c := &bridgeConfig{
		rawNet: rawNet, rawFw: rawFw,
		netCfg: netCfg, fwCfg: fwCfg,
		// Отпечаток — над конкатенацией обоих пакетов: правка любого из
		// них обязана его сменить. Алгоритм тот же, что у wireless.
		fp: wireless.Fingerprint(append(append([]byte{}, rawNet...), rawFw...)),
	}

	if lan, ok := netCfg.Section("lan"); ok {
		c.baseDev = vlanBase(strings.TrimSpace(lan.Options["device"]))
	}
	if c.baseDev != "" {
		for _, sec := range netCfg.ByType("device") {
			if sec.Options["name"] != c.baseDev {
				continue
			}
			c.devSection = sec.Name
			// Один токен разбирается в Options, несколько — в Lists.
			if p := sec.Options["ports"]; p != "" {
				c.port, c.portCount = p, 1
			} else if ps := sec.Lists["ports"]; len(ps) > 0 {
				c.portCount = len(ps)
				if len(ps) == 1 {
					c.port = ps[0]
				}
			}
			break
		}
		for _, sec := range netCfg.ByType("bridge-vlan") {
			if sec.Options["device"] == c.baseDev {
				c.vlanSections = append(c.vlanSections, sec.Name)
			}
		}
	}
	for _, sec := range netCfg.ByType("route") {
		if sec.Options["interface"] == "homelan" {
			c.homelanRoutes = append(c.homelanRoutes, sec.Name)
		}
	}
	// Правила ищутся ПО СОДЕРЖИМОМУ — по таблице, в которую смотрят, а не по
	// имени секции: ручная сборка своих имён не знает (ADR-0030).
	for _, sec := range netCfg.ByType("rule") {
		switch sec.Options["lookup"] {
		case strconv.Itoa(relaydTableBase), strconv.Itoa(relaydTableBase + 1):
			c.relayRules = append(c.relayRules, sec.Name)
		}
	}

	homelan, hasHomelan := netCfg.Section("homelan")
	_, hasRelay := netCfg.Section("relay")
	c.enabled = hasHomelan && hasRelay
	if hasHomelan {
		c.legIP = strings.TrimSpace(homelan.Options["ipaddr"])
	}

	for _, sec := range fwCfg.ByType("zone") {
		switch sec.Options["name"] {
		case "homelan":
			c.homelanZones = append(c.homelanZones, sec.Name)
		case "wan":
			if c.wanZone == "" {
				c.wanZone = sec.Name
			}
			vals := sec.Lists["masq_src"]
			if v := sec.Options["masq_src"]; v != "" {
				vals = append(vals, v)
			}
			for _, v := range vals {
				if negCIDR.MatchString(v) {
					c.wanMasqNeg = append(c.wanMasqNeg, v)
				}
			}
		}
	}
	for _, sec := range fwCfg.ByType("forwarding") {
		src, dst := sec.Options["src"], sec.Options["dest"]
		if src == "homelan" || dst == "homelan" {
			c.homelanFwds = append(c.homelanFwds, sec.Name)
			if src == "lan" && dst == "homelan" {
				c.lanFwd = sec.Name
			}
		}
	}
	return c, nil
}

// ─────────── GET /api/bridge ───────────

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, errResp := s.readBridgeConfig(ctx)
	if errResp != nil {
		errResp.send(w)
		return
	}

	st := &BridgeState{
		Fingerprint: c.fp,
		Enabled:     c.enabled,
		LegIP:       c.legIP,
		APAccess:    c.lanFwd != "",
	}
	if v, err := s.ex.UCIGet(ctx, "netmode", "main", "bridge_pc_ip"); err == nil {
		st.PCIP = v
	} else if !errors.Is(err, executor.ErrNotFound) {
		s.logf("bridge: pc_ip не прочитан: %v", err)
	}

	// Диагностика порта и relayd — глазами скрипта. Отказ не валит ответ:
	// nil в этих полях — честное «не прочитано» для панели.
	if c.port != "" {
		if raw, err := s.ex.BridgeStatus(ctx, c.port); err == nil {
			var sc bridgeScriptStatus
			if jerr := json.Unmarshal(raw, &sc); jerr == nil {
				st.Relayd = &BridgeRelayd{Installed: sc.RelaydInstalled, Running: sc.RelaydRunning}
				st.Port = &BridgePort{Name: c.port, Carrier: sc.Carrier == 1,
					SpeedMbps: sc.SpeedMbps, CarrierChanges: sc.CarrierChanges}
			} else {
				s.logf("bridge: JSON статуса скрипта не разбирается: %v", jerr)
			}
		} else {
			s.logf("bridge: статус скрипта не прочитан: %v", err)
		}
	}

	if up, ok := s.uplinkStatus(ctx); ok {
		st.Uplink = up
	}

	if r.URL.Query().Get("probe") == "1" {
		st.Probes = s.bridgeProbes(ctx, st)
	}
	st.LastFail = s.bridgeFails.Get()
	writeJSON(w, http.StatusOK, st)
}

// uplinkStatus — живое L3-состояние wwan. Подсеть выводится отсюда на
// КАЖДЫЙ запрос: во время разведки роутер сменил uplink при живой ноге
// моста (raw/86), и запомненная подсеть врала бы.
func (s *Server) uplinkStatus(ctx context.Context) (*BridgeUplink, bool) {
	raw, err := s.ex.UbusCall(ctx, "network.interface.wwan", "status", nil)
	if err != nil {
		s.logf("bridge: статус wwan не прочитан: %v", err)
		return nil, false
	}
	st, err := netif.ParseStatus(raw)
	if err != nil {
		s.logf("bridge: статус wwan не разбирается: %v", err)
		return nil, false
	}
	up := &BridgeUplink{Up: st.Up, Device: st.Device, Gateway: st.Gateway()}
	if len(st.Addresses) > 0 {
		up.Address = st.Addresses[0].Address
		up.Mask = st.Addresses[0].Mask
	}
	return up, true
}

// bridgeProbes — пинги ПК и uplink-шлюза, с кэшем: ping держит HTTP-запрос
// секунды, а вкладку панель перезапрашивает часто.
func (s *Server) bridgeProbes(ctx context.Context, st *BridgeState) *BridgeProbes {
	s.bridgeProbeMu.Lock()
	if s.bridgeProbeVal != nil && time.Since(s.bridgeProbeAt) < bridgeProbeTTL {
		v := s.bridgeProbeVal
		s.bridgeProbeMu.Unlock()
		return v
	}
	s.bridgeProbeMu.Unlock()

	out := &BridgeProbes{At: time.Now().UTC().Format(time.RFC3339)}
	// ПК пингуется через ногу моста: до него ходит L2 через VLAN, и проба
	// через wwan проверяла бы путь relayd, а не сам ПК.
	if st.PCIP != "" && st.Enabled {
		if raw, err := s.ex.UbusCall(ctx, "network.interface.homelan", "status", nil); err == nil {
			if hst, perr := netif.ParseStatus(raw); perr == nil && hst.Device != "" {
				if ans, aerr := s.ex.BridgeProbe(ctx, st.PCIP, hst.Device); aerr == nil {
					out.PC = &BridgeProbeResult{Answered: ans}
				} else {
					s.logf("bridge: проба ПК не удалась: %v", aerr)
				}
			}
		}
	}
	if st.Uplink != nil && st.Uplink.Device != "" && st.Uplink.Gateway != "" {
		if ans, err := s.ex.BridgeProbe(ctx, st.Uplink.Gateway, st.Uplink.Device); err == nil {
			out.Gateway = &BridgeProbeResult{Answered: ans}
		} else {
			s.logf("bridge: проба шлюза не удалась: %v", err)
		}
	}

	s.bridgeProbeMu.Lock()
	s.bridgeProbeVal, s.bridgeProbeAt = out, time.Now()
	s.bridgeProbeMu.Unlock()
	return out
}

// ─────────── общий гвард записи моста ───────────

// openBridgeWrite — отказы, общие всем трём операциям, в неизменном
// порядке: чужой стейджинг (двух пакетов!) → чтение → If-Match.
func (s *Server) openBridgeWrite(r *http.Request) (*bridgeConfig, *httpErr) {
	ctx := r.Context()
	for _, pkg := range []string{"network", "firewall"} {
		changes, err := s.ex.UCIChanges(ctx, pkg)
		if err != nil {
			return nil, &httpErr{http.StatusServiceUnavailable, "uci_unavailable", err.Error()}
		}
		if uci.HasStagedChanges(changes) {
			return nil, conflict("foreign_staged_changes",
				"В /etc/config/"+pkg+" есть незакоммиченные правки — вероятно, открыт LuCI. "+
					"Примените или отмените их, затем повторите.")
		}
	}
	c, errResp := s.readBridgeConfig(ctx)
	if errResp != nil {
		return nil, errResp
	}
	want := strings.TrimSpace(r.Header.Get("If-Match"))
	if want == "" {
		return nil, conflict("fingerprint_required",
			"Нужен заголовок If-Match с отпечатком из GET /api/bridge")
	}
	if c.fp != want {
		return nil, conflict("fingerprint_mismatch",
			"Конфигурация изменилась с момента чтения. Обновите состояние и повторите.")
	}
	return c, nil
}

// ─────────── POST /api/bridge/enable ───────────

// bridgePlan — всё, что джобу нужно знать; собирается ДО старта джоба.
type bridgePlan struct {
	action  string
	fp      string
	port    string
	baseDev string
	devSec  string

	legIP      string
	pcIP       string
	maskBits   int
	subnetCIDR string
	wanZone    string
	apAccess   bool
	lanSubnet  string
	install    bool
}

func (s *Server) handleBridgeEnable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in BridgeEnable
	if errResp := decodeStrict(r, &in); errResp != nil {
		errResp.send(w)
		return
	}

	// 1. Синтаксис — до любого обращения к системе.
	legIP := net.ParseIP(in.LegIP)
	if legIP == nil || legIP.To4() == nil {
		writeErr(w, http.StatusBadRequest, "leg_ip_invalid", "Адрес роутера в uplink не похож на IPv4")
		return
	}
	pcIP := net.ParseIP(in.PCIP)
	if pcIP == nil || pcIP.To4() == nil {
		writeErr(w, http.StatusBadRequest, "pc_ip_invalid", "Адрес ПК не похож на IPv4")
		return
	}

	// 2. Живой uplink обязателен: подсеть выводится только из него, и без
	// него enable не имеет смысла — relayd мостит в никуда.
	up, ok := s.uplinkStatus(ctx)
	if !ok || !up.Up || up.Address == "" {
		writeErr(w, http.StatusConflict, "uplink_down",
			"Внешняя сеть не подключена — проброс мостит LAN именно в неё. Подключите uplink и повторите.")
		return
	}
	subnet := subnetOf(up.Address, up.Mask)
	if subnet == nil {
		writeErr(w, http.StatusServiceUnavailable, "uplink_down", "Подсеть uplink не вычислена")
		return
	}
	// 3. Оба адреса в uplink-подсети и не конфликтуют ни между собой, ни
	// с занятыми: IP-конфликт — ровно то, что сломало ручную сборку.
	for name, ip := range map[string]net.IP{"leg": legIP, "pc": pcIP} {
		if !subnet.Contains(ip) {
			writeErr(w, http.StatusConflict, "ip_outside_subnet",
				fmt.Sprintf("Адрес %s (%s) вне uplink-подсети %s", name, ip, subnet))
			return
		}
	}
	if in.LegIP == in.PCIP || in.LegIP == up.Address || in.LegIP == up.Gateway || in.PCIP == up.Address || in.PCIP == up.Gateway {
		writeErr(w, http.StatusConflict, "ip_conflict",
			"Адреса роутера, ПК, uplink-шлюза и wwan обязаны быть попарно разными")
		return
	}

	// 4–5. Чужой стейджинг и отпечаток.
	c, errResp := s.openBridgeWrite(r)
	if errResp != nil {
		errResp.send(w)
		return
	}
	if c.enabled {
		writeErr(w, http.StatusConflict, "already_enabled",
			"Проброс уже включён. Чтобы поменять адреса — выключите и включите заново.")
		return
	}
	// 6. Порт — ровно один, из конфигурации (ADR-0019: литералов нет).
	if c.port == "" {
		writeErr(w, http.StatusConflict, "port_ambiguous",
			fmt.Sprintf("У бриджа %s не ровно один порт (%d) — какой из них ведёт к ПК, демон не угадывает", c.baseDev, c.portCount))
		return
	}

	// 7. relayd: без пакета и без согласия — отказ, и не сделано ничего.
	raw, err := s.ex.BridgeStatus(ctx, c.port)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "bridge_unavailable",
			"Состояние проброса не прочитано: "+err.Error())
		return
	}
	var sc bridgeScriptStatus
	if err := json.Unmarshal(raw, &sc); err != nil {
		writeErr(w, http.StatusInternalServerError, "parse_failed", "JSON netmode-bridge status не разбирается")
		return
	}
	if !sc.RelaydInstalled && !in.Install {
		writeErr(w, http.StatusConflict, "relayd_missing",
			"Пакет relayd не установлен. Панель спросит согласие на установку и повторит с install:true.")
		return
	}

	// 8. Проба занятости выбранного адреса — блокирующая (решение
	// владельца): живой IP-конфликт молча ломал схему.
	answered, err := s.ex.BridgeProbe(ctx, in.LegIP, up.Device)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "probe_failed",
			"Занятость адреса не проверена: "+err.Error())
		return
	}
	if answered {
		writeErr(w, http.StatusConflict, "leg_ip_taken",
			fmt.Sprintf("Адрес %s уже отвечает в uplink-сети — займите другой", in.LegIP))
		return
	}

	p := bridgePlan{
		action: "enable", fp: c.fp, port: c.port, baseDev: c.baseDev, devSec: c.devSection,
		legIP: in.LegIP, pcIP: in.PCIP, maskBits: up.Mask, subnetCIDR: subnet.String(),
		wanZone: c.wanZone, apAccess: in.APAccess, install: !sc.RelaydInstalled && in.Install,
	}
	if in.APAccess {
		p.lanSubnet = s.lanSubnet(c)
		if p.lanSubnet == "" {
			writeErr(w, http.StatusConflict, "lan_subnet_unknown",
				"Подсеть LAN не вычислена из network.lan — доступ из точки доступа настроить не по чему")
			return
		}
	}
	eta := bridgeEnableETASec
	if p.install {
		eta = bridgeInstallETASec
	}
	// 9. Джоб — ПОСЛЕДНИМ: все отказы выше не заняли слот.
	s.startBridgeJob(w, p, eta, "Включение проброса LAN в uplink")
}

// lanSubnet — CIDR подсети LAN из network.lan (ipaddr бывает и с маской
// внутри — '192.168.9.1/24', живой конфиг, — и с отдельным netmask).
func (s *Server) lanSubnet(c *bridgeConfig) string {
	lan, ok := c.netCfg.Section("lan")
	if !ok {
		return ""
	}
	ipaddr := strings.TrimSpace(lan.Options["ipaddr"])
	if strings.Contains(ipaddr, "/") {
		if _, n, err := net.ParseCIDR(ipaddr); err == nil {
			return n.String()
		}
		return ""
	}
	ip := net.ParseIP(ipaddr)
	mask := net.ParseIP(strings.TrimSpace(lan.Options["netmask"]))
	if ip == nil || mask == nil || ip.To4() == nil || mask.To4() == nil {
		return ""
	}
	m := net.IPMask(mask.To4())
	return (&net.IPNet{IP: ip.Mask(m), Mask: m}).String()
}

func subnetOf(addr string, mask int) *net.IPNet {
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() == nil || mask < 0 || mask > 32 {
		return nil
	}
	m := net.CIDRMask(mask, 32)
	return &net.IPNet{IP: ip.To4().Mask(m), Mask: m}
}

func dottedMask(bits int) string {
	return net.IP(net.CIDRMask(bits, 32)).String()
}

// startBridgeJob — общий хвост трёх обработчиков: старт джоба и 202.
func (s *Server) startBridgeJob(w http.ResponseWriter, p bridgePlan, eta int, label string) {
	arg := p.action
	j, err := s.jobs.Start("bridge", arg, label, eta, func(ctx context.Context) error {
		return s.runBridge(ctx, p)
	})
	if errors.Is(err, job.ErrBusy) {
		writeErr(w, http.StatusConflict, "job_busy", "Уже идёт другая операция. Дождитесь её завершения.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "job_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": j})
}

// ─────────── POST /api/bridge/disable ───────────

func (s *Server) handleBridgeDisable(w http.ResponseWriter, r *http.Request) {
	// Тело не читается намеренно: у демонтажа нет параметров, а пустое
	// тело у fetch без body — норма.
	c, errResp := s.openBridgeWrite(r)
	if errResp != nil {
		errResp.send(w)
		return
	}
	// «Нечего демонтировать» — по СОДЕРЖИМОМУ, не по флагу enabled: после
	// сорванного демонтажа могла остаться половина секций, и повторный
	// disable обязан уметь дочистить её.
	if !c.enabled && len(c.vlanSections) == 0 && len(c.homelanZones) == 0 &&
		len(c.homelanFwds) == 0 && len(c.wanMasqNeg) == 0 {
		writeErr(w, http.StatusConflict, "already_disabled", "Проброс не включён — демонтировать нечего.")
		return
	}
	p := bridgePlan{action: "disable", fp: c.fp, port: c.port, baseDev: c.baseDev, devSec: c.devSection}
	s.startBridgeJob(w, p, bridgeDisableETASec, "Выключение проброса LAN в uplink")
}

// ─────────── POST /api/bridge/access ───────────

func (s *Server) handleBridgeAccess(w http.ResponseWriter, r *http.Request) {
	var in BridgeAccess
	if errResp := decodeStrict(r, &in); errResp != nil {
		errResp.send(w)
		return
	}
	c, errResp := s.openBridgeWrite(r)
	if errResp != nil {
		errResp.send(w)
		return
	}
	if !c.enabled {
		writeErr(w, http.StatusConflict, "bridge_not_enabled",
			"Проброс выключен — доступ из точки доступа настраивается поверх включённого.")
		return
	}
	if (c.lanFwd != "") == in.Enabled {
		writeErr(w, http.StatusConflict, "already_set", "Доступ уже в запрошенном состоянии.")
		return
	}
	p := bridgePlan{action: "access-off", fp: c.fp, port: c.port, baseDev: c.baseDev}
	label := "Закрытие доступа к uplink из точки доступа"
	if in.Enabled {
		p.action = "access-on"
		label = "Открытие доступа к uplink из точки доступа"
		p.lanSubnet = s.lanSubnet(c)
		if p.lanSubnet == "" {
			writeErr(w, http.StatusConflict, "lan_subnet_unknown",
				"Подсеть LAN не вычислена из network.lan")
			return
		}
	}
	s.startBridgeJob(w, p, bridgeAccessETASec, label)
}

// ─────────── тело джоба ───────────

// touched — адрес секции, которой партия успела коснуться; их и только их
// отменяет revert (ADR-0028).
type touched struct{ pkg, section string }

// bridgeBatch — партия записей: первый отказ останавливает всё, а список
// затронутых секций остаётся для revert.
type bridgeBatch struct {
	s   *Server
	ctx context.Context
	ops []touched
	err error
}

func (b *bridgeBatch) note(pkg, section string) {
	for _, t := range b.ops {
		if t.pkg == pkg && t.section == section {
			return
		}
	}
	b.ops = append(b.ops, touched{pkg, section})
}

func (b *bridgeBatch) do(pkg, section string, fn func() error) {
	if b.err != nil {
		return
	}
	// Секция записывается ДО попытки: отказавшая команда могла успеть
	// изменить стейджинг, и не отменить её хуже, чем отменить лишний раз.
	b.note(pkg, section)
	b.err = fn()
}

func (b *bridgeBatch) addNamed(pkg, section, typ string) {
	b.do(pkg, section, func() error { return b.s.ex.UCIAddNamed(b.ctx, pkg, section, typ) })
}
func (b *bridgeBatch) set(pkg, section, opt, val string) {
	b.do(pkg, section, func() error { return b.s.ex.UCISet(b.ctx, pkg, section, opt, val) })
}
func (b *bridgeBatch) addList(pkg, section, opt, val string) {
	b.do(pkg, section, func() error { return b.s.ex.UCIAddList(b.ctx, pkg, section, opt, val) })
}
func (b *bridgeBatch) delList(pkg, section, opt, val string) {
	b.do(pkg, section, func() error { return b.s.ex.UCIDelList(b.ctx, pkg, section, opt, val) })
}
func (b *bridgeBatch) del(pkg, section, opt string) {
	b.do(pkg, section, func() error { return b.s.ex.UCIDelete(b.ctx, pkg, section, opt) })
}

// delIfPresent — удаление, для которого ОТСУТСТВИЕ цели не отказ.
//
// Демонтаж обязан быть идемпотентным: он снимает конфигурацию, собранную
// не обязательно нами. Живой прогон 2026-09-01 упёрся ровно в это —
// `uci delete netmode.main.bridge_pc_ip` вернул «Entry not found», потому
// что ручная сборка адрес ПК никогда не записывала, и вся партия
// демонтажа рухнула на последнем шаге. Требовать наличия того, что мы
// собираемся УДАЛИТЬ, — это требовать, чтобы конфигурацию собрали ровно
// мы, а она бывает и чужой.
//
// Прочие отказы uci при этом остаются отказами: «нет записи» —
// единственное, что здесь безвредно.
func (b *bridgeBatch) delIfPresent(pkg, section, opt string) {
	b.do(pkg, section, func() error {
		err := b.s.ex.UCIDelete(b.ctx, pkg, section, opt)
		if err != nil && (errors.Is(err, executor.ErrNotFound) || isUCIEntryNotFound(err)) {
			return nil
		}
		return err
	})
}

// isUCIEntryNotFound — «нет такой записи» по тексту uci.
//
// UCIDelete не заворачивает этот случай в ErrNotFound (в отличие от
// UCIGet): удаление в остальном коде адресуется тому, что мы только что
// прочитали. Строка «Entry not found» — часть пользовательского
// интерфейса uci, не приватная деталь (тот же приём, что isUCINotFound
// в executor).
func isUCIEntryNotFound(err error) bool {
	return strings.Contains(err.Error(), "Entry not found")
}

// revert отменяет свой черновик, адресуя секции ИМЕНАМИ ИЗ uci changes.
//
// Не по b.ops, и это не оптимизация: адрес анонимной секции — индекс, а
// удалённая анонимная секция живёт в стейджинге под внутренним именем
// (cfg08a1b0). `uci revert network.@bridge-vlan[0]` на неё отвечает кодом
// 0 и не отменяет ничего — измерено на живом роутере, см.
// uci.ChangedSections. Список из самого uci — единственный способ назвать
// то, что мы наделали, теми же словами, что и он.
//
// Границы ADR-0028 соблюдены: чужой стейджинг отвергнут на входе
// (foreign_staged_changes), значит всё, что uci сейчас перечисляет, —
// наше; зовётся это только на пути отказа и только до коммита.
//
// b.ops остаётся источником СПИСКА ПАКЕТОВ: спрашивать changes у пакета,
// которого партия не касалась, незачем.
func (b *bridgeBatch) revert() error {
	var firstErr error
	var pkgs []string
	for _, t := range b.ops {
		known := false
		for _, p := range pkgs {
			if p == t.pkg {
				known = true
				break
			}
		}
		if !known {
			pkgs = append(pkgs, t.pkg)
		}
	}
	for _, pkg := range pkgs {
		raw, err := b.s.ex.UCIChanges(b.ctx, pkg)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("черновик %s не прочитан: %w", pkg, err)
			}
			continue
		}
		// В обратном порядке: у зависимых секций отмена в прямом порядке
		// повторила бы ту же беду со сдвигом адресов.
		secs := uci.ChangedSections(pkg, raw)
		for i := len(secs) - 1; i >= 0; i-- {
			if err := b.s.ex.UCIRevert(b.ctx, pkg, secs[i]); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// runBridge — тело джоба всех трёх действий. Дисциплина switchUpstream.
func (s *Server) runBridge(ctx context.Context, p bridgePlan) error {
	// Паника обязана доехать причиной, а не только состоянием failed —
	// см. switchUpstream, шаг 0.
	defer func() {
		if r := recover(); r != nil {
			s.bridgeFailed(p.action, breasonUnverifiable,
				fmt.Errorf("операция рухнула на середине (%v) — состояние роутера неизвестно, сверьтесь со статусом", r))
			panic(r)
		}
	}()
	// Прошлая неудача стирается первой строкой: новая попытка не имеет
	// права показывать вердикт старой.
	s.bridgeFails.Clear()

	// Повторная сверка отпечатка: окно между обработчиком и джобом —
	// миллисекунды, но именно в нём чужой Save & Apply публикует своё.
	c, errResp := s.readBridgeConfig(ctx)
	if errResp != nil {
		return s.bridgeFailed(p.action, breasonApplyFailed,
			fmt.Errorf("конфигурация не прочитана, ничего не изменено: %s", errResp.message))
	}
	if c.fp != p.fp {
		return s.bridgeFailed(p.action, breasonApplyFailed, errors.New(
			"конфигурация network/firewall изменилась между проверкой и записью — ничего не изменено, обновите состояние и повторите"))
	}

	// Установка relayd — ДО первой записи uci: мутация системы, но не
	// конфигурации; её отказ оставляет конфигурацию нетронутой.
	if p.action == "enable" && p.install {
		if err := s.ex.InstallRelayd(ctx); err != nil {
			return s.bridgeFailed(p.action, breasonInstallFailed, err)
		}
	}

	b := &bridgeBatch{s: s, ctx: ctx}
	pkgs := s.writeBridge(b, c, p)
	if b.err != nil {
		if rerr := b.revert(); rerr != nil {
			// Неотменённый черновик — ДРУГАЯ новость: следующий запрос
			// вечно получал бы foreign_staged_changes про LuCI, которого
			// нет (см. switchUpstream, шаг 5).
			return s.bridgeFailed(p.action, breasonStaleDraft,
				fmt.Errorf("%w; отменить черновик не удалось: %w", b.err, rerr))
		}
		return s.bridgeFailed(p.action, breasonApplyFailed, b.err)
	}

	// По одному коммиту на пакет. Revert после попытки коммита не
	// дописывается: неизвестно, что успело опубликоваться (ADR-0006).
	for _, pkg := range pkgs {
		if err := s.ex.UCICommit(ctx, pkg); err != nil {
			return s.bridgeFailed(p.action, breasonApplyFailed,
				fmt.Errorf("не удалось применить изменения в /etc/config/%s", pkg))
		}
	}

	// Применение скриптом слоя 2.
	scriptAction := p.action
	if strings.HasPrefix(p.action, "access") {
		scriptAction = "access"
	}
	if err := s.ex.ApplyBridge(ctx, scriptAction); err != nil {
		return s.bridgeFailed(p.action, bridgeApplyReason(err), err)
	}

	// Вердикт — по наблюдению (ADR-0030). У access наблюдаемого следствия
	// в живой системе нет (правила зон не читаются обратно из nft), а fw4
	// проверяет набор правил при reload — код 4 пришёл бы выше.
	switch p.action {
	case "enable":
		if reason, ok := s.awaitHomelan(ctx, true, p.legIP); !ok {
			return s.bridgeFailed(p.action, reason,
				errors.New("network reload прошёл, но интерфейс проброса не поднялся с адресом "+p.legIP+" за отведённое окно"))
		}
		if reason, ok := s.awaitRelayd(ctx, p.port, true); !ok {
			return s.bridgeFailed(p.action, reason,
				errors.New("интерфейс проброса поднят, но процесс relayd не запустился — ПК не будет виден uplink-сети"))
		}
	case "disable":
		if reason, ok := s.awaitHomelan(ctx, false, ""); !ok {
			return s.bridgeFailed(p.action, reason,
				errors.New("демонтаж закоммичен, но интерфейс проброса не исчез за отведённое окно"))
		}
		if p.port != "" {
			if reason, ok := s.awaitRelayd(ctx, p.port, false); !ok {
				return s.bridgeFailed(p.action, reason,
					errors.New("relayd не остановился после демонтажа — мост может работать поверх удалённой конфигурации"))
			}
		}
	}

	s.bridgeFails.Clear()
	return nil
}

// writeBridge собирает партию по действию; возвращает пакеты для коммита
// в порядке записи.
func (s *Server) writeBridge(b *bridgeBatch, c *bridgeConfig, p bridgePlan) []string {
	switch p.action {
	case "enable":
		// network: VLAN-фильтрация на бридже, две VLAN-секции, LAN на
		// untagged, нога в uplink, интерфейс relayd. Формы — с живого
		// роутера (raw/80).
		b.set("network", p.devSec, "vlan_filtering", "1")
		b.addNamed("network", "netmode_vlan1", "bridge-vlan")
		b.set("network", "netmode_vlan1", "device", p.baseDev)
		b.set("network", "netmode_vlan1", "vlan", "1")
		b.addList("network", "netmode_vlan1", "ports", p.port+":u*")
		b.addNamed("network", "netmode_vlan2", "bridge-vlan")
		b.set("network", "netmode_vlan2", "device", p.baseDev)
		b.set("network", "netmode_vlan2", "vlan", "2")
		b.addList("network", "netmode_vlan2", "ports", p.port+":t")
		b.set("network", "lan", "device", p.baseDev+".1")
		// Имя homelan — контракт: на нём держится ubus-объект вердикта
		// network.interface.homelan (allowlist executor'а).
		b.addNamed("network", "homelan", "interface")
		b.set("network", "homelan", "proto", "static")
		b.set("network", "homelan", "device", p.baseDev+".2")
		b.set("network", "homelan", "ipaddr", p.legIP)
		// Маска — из живой маски uplink, не литерал /24 (raw/86).
		b.set("network", "homelan", "netmask", dottedMask(p.maskBits))
		b.addNamed("network", "relay", "interface")
		b.set("network", "relay", "proto", "relay")
		b.addList("network", "relay", "network", "homelan")
		b.addList("network", "relay", "network", "wwan")

		// firewall: relayd форвардит юникаст через маршрутизацию ядра —
		// netfilter ПРИМЕНЯЕТСЯ (живой урок 2026-08-31: без зоны транзит
		// резался дефолтным REJECT).
		b.addNamed("firewall", "netmode_homelan", "zone")
		b.set("firewall", "netmode_homelan", "name", "homelan")
		b.set("firewall", "netmode_homelan", "input", "ACCEPT")
		b.set("firewall", "netmode_homelan", "output", "ACCEPT")
		b.set("firewall", "netmode_homelan", "forward", "ACCEPT")
		b.addList("firewall", "netmode_homelan", "network", "homelan")
		b.addList("firewall", "netmode_homelan", "network", "relay")
		b.addNamed("firewall", "netmode_fwd_h2w", "forwarding")
		b.set("firewall", "netmode_fwd_h2w", "src", "homelan")
		b.set("firewall", "netmode_fwd_h2w", "dest", "wan")
		b.addNamed("firewall", "netmode_fwd_w2h", "forwarding")
		b.set("firewall", "netmode_fwd_w2h", "src", "wan")
		b.set("firewall", "netmode_fwd_w2h", "dest", "homelan")
		// ПК ходит в uplink со своим адресом, LAN-клиенты NAT-ятся как
		// раньше: отрицание в masq_src подтверждено fw4 (raw/85).
		if p.wanZone != "" {
			b.addList("firewall", p.wanZone, "masq_src", "!"+p.subnetCIDR)
		}
		if p.apAccess {
			s.writeAccessOn(b, p)
		}
		// Хост-маршрут к ПК — обязателен, и это урок живого роутера
		// 2026-09-01, а не перестраховка.
		//
		// В главной таблице маршрутов после включения лежат ДВА маршрута
		// к uplink-подсети: через phy0.0-sta0 (наш адрес в ней) и через
		// br-lan.2 (нога моста). Для транзитного пакета из LAN ядро
		// выбирает первый, то есть шлёт пакет к ПК в эфир uplink, где ПК
		// нет. Проверено прямым вопросом ядру:
		//
		//	ip route get 192.168.0.90 from 192.168.9.238 iif br-lan.1
		//	→ dev phy0.0-sta0        (до маршрута)
		//	→ dev br-lan.2           (после)
		//
		// Собственные таблицы relayd (16800/16801) тут не помогают: его
		// правила смотрят только на iif phy0.0-sta0 и iif br-lan.2, а
		// пакет из LAN приходит на br-lan.1 и до них не доходит вовсе.
		//
		// Маршрут ставится ВСЕГДА при включении, а не вместе с доступом из
		// точки доступа: он лишь делает таблицу маршрутов честной насчёт
		// того, где живёт ПК. Доступ открывает firewall — одно правило,
		// один смысл.
		b.addNamed("network", "netmode_pcroute", "route")
		b.set("network", "netmode_pcroute", "interface", "homelan")
		b.set("network", "netmode_pcroute", "target", p.pcIP)
		b.set("network", "netmode_pcroute", "netmask", "255.255.255.255")

		// Правила: трафик из LAN смотрит в таблицы relayd прежде main.
		//
		// Маршрут выше гарантирует ПК; правила покрывают ВСЁ, что relayd
		// выучил, — то есть и виртуалки на ПК, чьих адресов панель не
		// знает и знать не может (измерено: 192.168.0.5 и .250 с MAC
		// VirtualBox, raw/88 §4-5).
		//
		// Правил ДВА, и это не перестраховка. relayd кладёт хосты одной
		// стороны моста в базу, другой — в базу+1, а какая сторона куда
		// попадёт, зависит от порядка, в котором сервис получил
		// интерфейсы. Просмотрев обе, мы снимаем зависимость от этого
		// порядка: в «чужой» таблице лежат маршруты в uplink через
		// станционный интерфейс — ровно то же, что дала бы main.
		//
		// Приоритеты 3 и 4: после собственных правил relayd (2) и задолго
		// до main (32766). in='lan' — ЛОГИЧЕСКОЕ имя, netifd разворачивает
		// его в устройство сам (raw/88 §3), и захардкоженного br-lan.1
		// здесь нет (ADR-0019).
		for i, name := range []string{"netmode_rule_a", "netmode_rule_b"} {
			b.addNamed("network", name, "rule")
			b.set("network", name, "in", "lan")
			b.set("network", name, "lookup", strconv.Itoa(relaydTableBase+i))
			b.set("network", name, "priority", strconv.Itoa(3+i))
		}

		// Метаданные: адрес ПК хранится только здесь — в живой сети его
		// не прочитать, пока ПК молчит (docs/contracts/uci-netmode.md).
		b.set("netmode", "main", "bridge_pc_ip", p.pcIP)
		return []string{"network", "firewall", "netmode"}

	case "disable":
		// Полный демонтаж по ТИПУ И СОДЕРЖИМОМУ (ADR-0030): ручная сборка
		// анонимна, netmode_-имена — только у нашей. Анонимные секции
		// удаляются в обратном порядке файла: индексы @type[N] сдвигаются
		// на каждом удалении.
		for i := len(c.vlanSections) - 1; i >= 0; i-- {
			b.delIfPresent("network", c.vlanSections[i], "")
		}
		if c.devSection != "" {
			b.delIfPresent("network", c.devSection, "vlan_filtering")
		}
		if _, ok := c.netCfg.Section("lan"); ok && c.baseDev != "" {
			b.set("network", "lan", "device", c.baseDev)
		}
		if _, ok := c.netCfg.Section("relay"); ok {
			b.delIfPresent("network", "relay", "")
		}
		if _, ok := c.netCfg.Section("homelan"); ok {
			b.delIfPresent("network", "homelan", "")
		}
		// Маршруты к ноге моста и правила в таблицы relayd — по
		// СОДЕРЖИМОМУ, как и всё остальное: ручная сборка своих имён не
		// знает. В обратном порядке: индексы анонимных секций сдвигаются
		// на каждом удалении.
		for i := len(c.homelanRoutes) - 1; i >= 0; i-- {
			b.delIfPresent("network", c.homelanRoutes[i], "")
		}
		for i := len(c.relayRules) - 1; i >= 0; i-- {
			b.delIfPresent("network", c.relayRules[i], "")
		}
		for i := len(c.homelanFwds) - 1; i >= 0; i-- {
			b.delIfPresent("firewall", c.homelanFwds[i], "")
		}
		for i := len(c.homelanZones) - 1; i >= 0; i-- {
			b.delIfPresent("firewall", c.homelanZones[i], "")
		}
		for _, v := range c.wanMasqNeg {
			b.delList("firewall", c.wanZone, "masq_src", v)
		}
		b.delIfPresent("netmode", "main", "bridge_pc_ip")
		return []string{"network", "firewall", "netmode"}

	case "access-on":
		s.writeAccessOn(b, p)
		return []string{"firewall"}

	case "access-off":
		if c.lanFwd != "" {
			b.delIfPresent("firewall", c.lanFwd, "")
		}
		for _, zone := range c.homelanZones {
			b.delIfPresent("firewall", zone, "masq")
			if sec, ok := c.fwCfg.Section(zone); ok {
				vals := sec.Lists["masq_src"]
				if v := sec.Options["masq_src"]; v != "" {
					vals = append(vals, v)
				}
				for _, v := range vals {
					b.delList("firewall", zone, "masq_src", v)
				}
			}
		}
		return []string{"firewall"}
	}
	b.err = fmt.Errorf("неизвестное действие %q", p.action)
	return nil
}

// writeAccessOn — форвардинг LAN→homelan c NAT в адрес ноги: uplink-сеть
// не знает LAN-подсети, и без masquerade обратного пути нет (живой урок).
// NAT сужен до lan_subnet, чтобы транзит uplink↔ПК через ту же зону
// оставался без подмены адресов.
func (s *Server) writeAccessOn(b *bridgeBatch, p bridgePlan) {
	b.addNamed("firewall", "netmode_fwd_l2h", "forwarding")
	b.set("firewall", "netmode_fwd_l2h", "src", "lan")
	b.set("firewall", "netmode_fwd_l2h", "dest", "homelan")
	b.set("firewall", "netmode_homelan", "masq", "1")
	b.addList("firewall", "netmode_homelan", "masq_src", p.lanSubnet)
}

// ─────────── вердикт по наблюдению ───────────

// awaitHomelan ждёт нужного состояния интерфейса homelan: для enable —
// поднят с нужным адресом, для disable — исчез (ubus-объект пропадает
// вместе с интерфейсом).
func (s *Server) awaitHomelan(ctx context.Context, wantUp bool, legIP string) (FailReason, bool) {
	deadline := time.Now().Add(bridgeIfaceTimeout)
	everRead := false
	for {
		raw, err := s.ex.UbusCall(ctx, "network.interface.homelan", "status", nil)
		if err == nil {
			everRead = true
			if st, perr := netif.ParseStatus(raw); perr == nil {
				if wantUp && st.Up && len(st.Addresses) > 0 && st.Addresses[0].Address == legIP {
					return "", true
				}
			}
		} else if !wantUp {
			// Объект исчез вместе с интерфейсом — ровно то, чего ждём.
			return "", true
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, bridgePollInterval) {
			if wantUp && !everRead {
				return breasonUnverifiable, false
			}
			return breasonNoIface, false
		}
	}
}

// awaitRelayd ждёт нужного состояния процесса relayd глазами скрипта.
func (s *Server) awaitRelayd(ctx context.Context, port string, wantRunning bool) (FailReason, bool) {
	deadline := time.Now().Add(bridgeRelaydTimeout)
	everRead := false
	for {
		if raw, err := s.ex.BridgeStatus(ctx, port); err == nil {
			var sc bridgeScriptStatus
			if jerr := json.Unmarshal(raw, &sc); jerr == nil {
				everRead = true
				if sc.RelaydRunning == wantRunning {
					return "", true
				}
			}
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, bridgePollInterval) {
			if !everRead {
				return breasonUnverifiable, false
			}
			return breasonRelayDown, false
		}
	}
}

// bridgeApplyReason — класс отказа ApplyBridge → причина таксономии.
func bridgeApplyReason(err error) FailReason {
	switch {
	case errors.Is(err, executor.ErrNoExecutor):
		return breasonExecutorMissing
	case errors.Is(err, executor.ErrBridgeBusy):
		return breasonBusy
	case errors.Is(err, executor.ErrBridgePrereq):
		return breasonPrereqMissing
	case errors.Is(err, executor.ErrRelaydInstall):
		return breasonInstallFailed
	case errors.Is(err, executor.ErrBridgeService):
		return breasonRelayDown
	default:
		return breasonApplyFailed
	}
}

// bridgeFailed — единственная воронка неудач: слот + журнал + ошибка джоба.
func (s *Server) bridgeFailed(action string, reason FailReason, err error) error {
	if !knownBridgeReason(reason) {
		// Кричим, но не молчим: панель покажет запасной ключ.
		s.logf("bridge: причина %q не из таксономии — поправьте allBridgeReasons", reason)
	}
	s.bridgeFails.Set(action, reason, err.Error(), time.Now())
	s.logf("проброс (%s): %s: %v", action, reason, err)
	return err
}
