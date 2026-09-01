package httpapi

// Тесты проброса LAN в uplink (ADR-0030). Проверяется дисциплина
// upstreamhandler, перенесённая на две конфигурации: порядок гвардов, состав
// партии, отмена черновика до коммита, вердикт по наблюдению — и главное,
// демонтаж СОБРАННОЙ ВРУЧНУЮ конфигурации (фикстура firewall снята с живого
// роутера, там анонимные секции).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"netmoded/internal/executor"
	"netmoded/internal/job"
)

// ─────────── оснастка ───────────

// Литералы из записанных фикстур живого роутера — то же исключение, что у
// upstream-тестов: это не догадка об устройстве роутера, а его снимок.
const (
	bPort    = "eth1"          // network.@device[0].ports в raw/11
	bBase    = "br-lan"        // network.@device[0].name
	bDevSec  = "@device[0]"    // анонимная секция бриджа
	bWanZone = "@zone[1]"      // firewall.@zone[1].name='wan'
	bLegIP   = "192.168.0.85"  // свободный адрес в uplink-подсети фикстуры
	bPCIP    = "192.168.0.90"  // адрес ПК
	bWwanIP  = "192.168.0.234" // адрес роутера в uplink (фикстура wwan)
	bWwanDev = "phy0.0-sta0"   //
	bGateway = "192.168.0.1"   //
)

// fastBridge ужимает окна наблюдения — иначе каждая причина таксономии
// стоила бы 15–25 секунд (довод дословно тот же, что у fastUpstream).
func fastBridge(t *testing.T) {
	t.Helper()
	iface, relayd, poll := bridgeIfaceTimeout, bridgeRelaydTimeout, bridgePollInterval
	bridgeIfaceTimeout = 60 * time.Millisecond
	bridgeRelaydTimeout = 60 * time.Millisecond
	bridgePollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		bridgeIfaceTimeout, bridgeRelaydTimeout, bridgePollInterval = iface, relayd, poll
	})
}

// scriptStatus — ответ `netmode-bridge status`.
func scriptStatus(installed, running bool, carrier int) []byte {
	return []byte(fmt.Sprintf(
		`{"relayd_installed":%t,"relayd_running":%t,"carrier":%d,"speed_mbps":1000,"carrier_changes":7}`,
		installed, running, carrier))
}

// homelanStatus — ответ ubus для интерфейса ноги.
func homelanStatus(up bool, addr string) []byte {
	if !up {
		return []byte(`{"up":false,"ipv4-address":[]}`)
	}
	return []byte(`{"up":true,"l3_device":"br-lan.2","proto":"static","ipv4-address":[{"address":"` +
		addr + `","mask":24}]}`)
}

// bridgeOff приводит фикстуры к состоянию «проброс выключен»: с живого
// роутера они сняты СОБРАННЫМИ, и тесты включения обязаны начинать с чистого.
func bridgeOff(t *testing.T, f *executor.Fake) {
	t.Helper()
	keep := func(key string, drop func(string) bool) {
		var out []string
		for _, line := range strings.Split(string(f.Fixtures[key]), "\n") {
			if !drop(line) {
				out = append(out, line)
			}
		}
		f.Fixtures[key] = []byte(strings.Join(out, "\n"))
	}
	keep("uci show network", func(l string) bool {
		return strings.HasPrefix(l, "network.homelan") || strings.HasPrefix(l, "network.relay") ||
			strings.HasPrefix(l, "network.@bridge-vlan[") || strings.Contains(l, "vlan_filtering")
	})
	keep("uci show firewall", func(l string) bool {
		return strings.HasPrefix(l, "firewall.@zone[2]") ||
			strings.HasPrefix(l, "firewall.@forwarding[1]") ||
			strings.HasPrefix(l, "firewall.@forwarding[2]") ||
			strings.Contains(l, "masq_src")
	})
	// lan.device в фикстуре network уже 'br-lan' (VLAN не включён).
	f.Fixtures["ubus network.interface.wwan status"] = []byte(
		`{"up":true,"l3_device":"` + bWwanDev + `","proto":"dhcp","ipv4-address":[{"address":"` +
			bWwanIP + `","mask":24}],"route":[{"target":"0.0.0.0","mask":0,"nexthop":"` + bGateway + `"}]}`)
	f.Fixtures["bridge status"] = scriptStatus(true, false, 1)
}

// bridgeOn изображает СОБРАННЫЙ ВРУЧНУЮ проброс.
//
// Фикстура firewall уже такова (снята с живого роутера 2026-09-01), а
// снимок network старше моста (2026-08-02) — его секции дописываются здесь
// дословно по raw/80-bridge-uci-network.txt, включая анонимные имена
// @bridge-vlan[0..1]: именно их обязан находить демонтаж, и подменять их на
// удобные netmode_* значило бы проверять не то, что стоит на роутере.
func bridgeOn(t *testing.T, f *executor.Fake) {
	t.Helper()
	cur := string(f.Fixtures["uci show network"])
	cur = strings.Replace(cur, "network.lan.device='br-lan'", "network.lan.device='br-lan.1'", 1)
	cur += `network.@device[0].vlan_filtering='1'
network.@bridge-vlan[0]=bridge-vlan
network.@bridge-vlan[0].device='br-lan'
network.@bridge-vlan[0].vlan='1'
network.@bridge-vlan[0].ports='eth1:u*'
network.@bridge-vlan[1]=bridge-vlan
network.@bridge-vlan[1].device='br-lan'
network.@bridge-vlan[1].vlan='2'
network.@bridge-vlan[1].ports='eth1:t'
network.homelan=interface
network.homelan.proto='static'
network.homelan.device='br-lan.2'
network.homelan.ipaddr='` + bLegIP + `'
network.homelan.netmask='255.255.255.0'
network.relay=interface
network.relay.proto='relay'
network.relay.network='homelan' 'wwan'
`
	f.Fixtures["uci show network"] = []byte(cur)
	f.Fixtures["ubus network.interface.wwan status"] = []byte(
		`{"up":true,"l3_device":"` + bWwanDev + `","proto":"dhcp","ipv4-address":[{"address":"` +
			bWwanIP + `","mask":24}],"route":[{"target":"0.0.0.0","mask":0,"nexthop":"` + bGateway + `"}]}`)
	f.Fixtures["bridge status"] = scriptStatus(true, true, 1)
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
}

func bridgeState(t *testing.T, s *Server, query string) BridgeState {
	t.Helper()
	rec := do(t, s, "GET", "/api/bridge"+query, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/bridge: код %d: %s", rec.Code, rec.Body.String())
	}
	var st BridgeState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("тело не разбирается: %v (%s)", err, rec.Body.String())
	}
	return st
}

func bridgeFP(t *testing.T, s *Server) string {
	t.Helper()
	return bridgeState(t, s, "").Fingerprint
}

// runBridgeOp шлёт запрос и дожидается джоба.
func runBridgeOp(t *testing.T, s *Server, path, body string) *job.Job {
	t.Helper()
	rec := post(t, s, path, body, bridgeFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s: код %d, ожидался 202: %s", path, rec.Code, rec.Body.String())
	}
	if !s.jobs.Wait(5 * time.Second) {
		t.Fatal("джоб не завершился")
	}
	j := s.jobs.Current()
	if j == nil {
		t.Fatal("джоб исчез до проверки")
	}
	return j
}

func wantBridgeFailure(t *testing.T, s *Server, j *job.Job, reason FailReason) {
	t.Helper()
	if j.State != job.Failed {
		t.Fatalf("состояние джоба %q, ожидалось failed", j.State)
	}
	lf := bridgeState(t, s, "").LastFail
	if lf == nil {
		t.Fatal("last_fail пуст: вернувшийся через минуту владелец не узнает, что не вышло")
	}
	if lf.Reason != reason {
		t.Errorf("причина %q, ожидалась %q (detail: %s)", lf.Reason, reason, lf.Detail)
	}
}

// ─────────── GET ───────────

// Состояние ВЫВОДИТСЯ из живой конфигурации, а не из флага. Фикстуры сняты
// с роутера с собранным вручную пробросом — панель обязана увидеть его как
// включённый, хотя ни одной секции с именем netmode_* там нет.
func TestBridgeStateDerivedFromManualConfig(t *testing.T) {
	s, f := newServer(t)
	bridgeOn(t, f)

	st := bridgeState(t, s, "")
	if !st.Enabled {
		t.Error("ручная сборка не опознана как включённый проброс")
	}
	if st.LegIP != bLegIP {
		t.Errorf("leg_ip %q, ожидался %q", st.LegIP, bLegIP)
	}
	if st.APAccess {
		t.Error("ap_access выведен как включённый, хотя форвардинга lan→homelan в фикстуре нет")
	}
	if st.Relayd == nil || !st.Relayd.Installed || !st.Relayd.Running {
		t.Errorf("relayd прочитан неверно: %+v", st.Relayd)
	}
	if st.Port == nil || st.Port.Name != bPort || !st.Port.Carrier {
		t.Errorf("порт прочитан неверно: %+v", st.Port)
	}
	if st.Uplink == nil || st.Uplink.Address != bWwanIP || st.Uplink.Gateway != bGateway {
		t.Errorf("uplink прочитан неверно: %+v", st.Uplink)
	}
	if st.Fingerprint == "" {
		t.Error("нет отпечатка — записывать будет нечем")
	}
}

// Выключенное состояние тоже выводится, а не выдумывается.
func TestBridgeStateOff(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	if st := bridgeState(t, s, ""); st.Enabled {
		t.Error("проброс объявлен включённым на чистой конфигурации")
	}
}

// Отказ чтения статуса скрипта не валит весь ответ: nil — честное «не
// прочитано», по которому панель нарисует отказ, а не выдуманные нули.
func TestBridgeStateNullsInsteadOfInvention(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Errors["bridge status "+bPort] = errors.New("скрипта нет")

	st := bridgeState(t, s, "")
	if st.Relayd != nil || st.Port != nil {
		t.Errorf("вместо null выдуманы значения: relayd=%+v port=%+v", st.Relayd, st.Port)
	}
	if st.Uplink == nil {
		t.Error("uplink должен читаться независимо от скрипта")
	}
}

// Пробы — только по запросу: ping держит запрос секунды.
func TestBridgeProbesOnlyOnDemand(t *testing.T) {
	s, f := newServer(t)
	bridgeOn(t, f)
	f.UCIValues["netmode.main.bridge_pc_ip"] = bPCIP
	f.BridgeProbeAnswers[bPCIP] = true
	f.BridgeProbeAnswers[bGateway] = true

	if st := bridgeState(t, s, ""); st.Probes != nil {
		t.Error("пробы выполнены без ?probe=1 — GET стал бы медленным на каждом заходе")
	}
	st := bridgeState(t, s, "?probe=1")
	if st.Probes == nil || st.Probes.PC == nil || !st.Probes.PC.Answered {
		t.Errorf("проба ПК: %+v", st.Probes)
	}
	if st.Probes.Gateway == nil || !st.Probes.Gateway.Answered {
		t.Errorf("проба шлюза: %+v", st.Probes)
	}
}

// ─────────── enable: гварды до первой записи ───────────

// Каждый отказ обязан оставить конфигурацию нетронутой. Проверяется не
// «код 409», а отсутствие записей — именно это делает повтор безопасным.
func TestBridgeEnableRefusalsWriteNothing(t *testing.T) {
	body := func(leg, pc string, extra string) string {
		return `{"leg_ip":"` + leg + `","pc_ip":"` + pc + `"` + extra + `}`
	}
	tests := []struct {
		name   string
		setup  func(t *testing.T, s *Server, f *executor.Fake)
		body   string
		status int
		code   string
	}{
		{
			"адрес ноги не IPv4", nil,
			body("не адрес", bPCIP, ""), http.StatusBadRequest, "leg_ip_invalid",
		},
		{
			"адрес ПК не IPv4", nil,
			body(bLegIP, "1.2.3", ""), http.StatusBadRequest, "pc_ip_invalid",
		},
		{
			"нога вне uplink-подсети", nil,
			body("10.9.9.9", bPCIP, ""), http.StatusConflict, "ip_outside_subnet",
		},
		{
			"нога совпала с адресом wwan", nil,
			body(bWwanIP, bPCIP, ""), http.StatusConflict, "ip_conflict",
		},
		{
			"нога совпала с адресом ПК", nil,
			body(bPCIP, bPCIP, ""), http.StatusConflict, "ip_conflict",
		},
		{
			"адрес ноги занят",
			func(t *testing.T, s *Server, f *executor.Fake) { f.BridgeProbeAnswers[bLegIP] = true },
			body(bLegIP, bPCIP, ""), http.StatusConflict, "leg_ip_taken",
		},
		{
			"relayd не установлен, согласия нет",
			func(t *testing.T, s *Server, f *executor.Fake) {
				f.Fixtures["bridge status"] = scriptStatus(false, false, 1)
			},
			body(bLegIP, bPCIP, ""), http.StatusConflict, "relayd_missing",
		},
		{
			"uplink лежит",
			func(t *testing.T, s *Server, f *executor.Fake) {
				f.Fixtures["ubus network.interface.wwan status"] = []byte(`{"up":false,"ipv4-address":[]}`)
			},
			body(bLegIP, bPCIP, ""), http.StatusConflict, "uplink_down",
		},
		{
			"чужой черновик в network",
			func(t *testing.T, s *Server, f *executor.Fake) { f.Staged["network"] = "set network.lan.foo=1\n" },
			body(bLegIP, bPCIP, ""), http.StatusConflict, "foreign_staged_changes",
		},
		{
			"чужой черновик в firewall",
			func(t *testing.T, s *Server, f *executor.Fake) { f.Staged["firewall"] = "set firewall.x.y=1\n" },
			body(bLegIP, bPCIP, ""), http.StatusConflict, "foreign_staged_changes",
		},
		{
			"лишнее поле в теле", nil,
			body(bLegIP, bPCIP, `,"колхоз":1`), http.StatusBadRequest, "unsupported_field",
		},
		{
			"проброс уже включён",
			func(t *testing.T, s *Server, f *executor.Fake) { bridgeOn(t, f) },
			body(bLegIP, bPCIP, ""), http.StatusConflict, "already_enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, f := newServer(t)
			bridgeOff(t, f)
			fp := bridgeFP(t, s)
			if tt.setup != nil {
				tt.setup(t, s, f)
				// Отпечаток берётся ПОСЛЕ setup: иначе тест падал бы на
				// fingerprint_mismatch вместо проверяемой причины.
				if tt.code != "foreign_staged_changes" {
					fp = bridgeFP(t, s)
				}
			}
			before := len(f.Calls)

			rec := post(t, s, "/api/bridge/enable", tt.body, fp)
			if rec.Code != tt.status {
				t.Fatalf("код %d, ожидался %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			if got := errCode(t, rec); got != tt.code {
				t.Errorf("код ошибки %q, ожидался %q", got, tt.code)
			}
			for _, c := range f.Calls[before:] {
				if isUCICall(c) {
					t.Errorf("отказ оставил запись в конфигурации: %s", c)
				}
			}
		})
	}
}

// Отпечаток обязателен и обязан ловить чужую правку — в ЛЮБОМ из двух
// пакетов: операции моста пишут и в network, и в firewall.
func TestBridgeEnableFingerprintCoversBothPackages(t *testing.T) {
	for _, pkg := range []string{"network", "firewall"} {
		t.Run(pkg, func(t *testing.T) {
			s, f := newServer(t)
			bridgeOff(t, f)
			fp := bridgeFP(t, s)

			// Чужая правка «в LuCI» — прямо в фикстуру, мимо стейджинга.
			f.Fixtures["uci show "+pkg] = append(f.Fixtures["uci show "+pkg],
				[]byte("\n"+pkg+".foreign=interface\n")...)

			rec := post(t, s, "/api/bridge/enable",
				`{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`, fp)
			if got := errCode(t, rec); got != "fingerprint_mismatch" {
				t.Errorf("правка в %s не уронила If-Match: код %q", pkg, got)
			}
		})
	}
	// И без заголовка вовсе.
	s, f := newServer(t)
	bridgeOff(t, f)
	rec := post(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`, "")
	if got := errCode(t, rec); got != "fingerprint_required" {
		t.Errorf("запрос без If-Match принят: код %q", got)
	}
}

// Порт выводится из конфигурации; двусмысленность — отказ, а не догадка.
func TestBridgeEnableRefusesAmbiguousPort(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	cur := strings.Replace(string(f.Fixtures["uci show network"]),
		"network.@device[0].ports='eth1'",
		"network.@device[0].ports='eth1' 'eth2'", 1)
	f.Fixtures["uci show network"] = []byte(cur)

	rec := post(t, s, "/api/bridge/enable",
		`{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`, bridgeFP(t, s))
	if got := errCode(t, rec); got != "port_ambiguous" {
		t.Errorf("два порта приняты: код %q (%s)", got, rec.Body.String())
	}
}

// ─────────── enable: партия записей ───────────

func TestBridgeEnableWritesExpectedBatch(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
	// После применения relayd поднимается — это и есть вердикт.
	f.QueueFixture("bridge status", scriptStatus(true, false, 1), scriptStatus(true, true, 1))

	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}

	calls := strings.Join(f.Calls, "\n")
	must := []string{
		"set network." + bDevSec + ".vlan_filtering=1",
		"add-named network.netmode_vlan1=bridge-vlan",
		"add_list network.netmode_vlan1.ports=" + bPort + ":u*",
		"add-named network.netmode_vlan2=bridge-vlan",
		"add_list network.netmode_vlan2.ports=" + bPort + ":t",
		"set network.lan.device=" + bBase + ".1",
		"add-named network.homelan=interface",
		"set network.homelan.ipaddr=" + bLegIP,
		"set network.homelan.netmask=255.255.255.0",
		"add-named network.relay=interface",
		"set network.relay.proto=relay",
		"add_list network.relay.network=homelan",
		"add_list network.relay.network=wwan",
		"add-named firewall.netmode_homelan=zone",
		"add_list firewall.netmode_homelan.network=relay",
		"add-named firewall.netmode_fwd_h2w=forwarding",
		"add-named firewall.netmode_fwd_w2h=forwarding",
		"add_list firewall." + bWanZone + ".masq_src=!192.168.0.0/24",
		"set netmode.main.bridge_pc_ip=" + bPCIP,
		"apply-bridge enable",
	}
	for _, want := range must {
		if !strings.Contains(calls, want) {
			t.Errorf("в партии нет %q", want)
		}
	}
	// Без ap_access форвардинга lan→homelan быть не должно.
	if strings.Contains(calls, "netmode_fwd_l2h") {
		t.Error("доступ из точки доступа настроен, хотя его не просили")
	}
	// Ровно по одному коммиту на пакет, и все — ДО применения.
	for _, pkg := range []string{"network", "firewall", "netmode"} {
		if n := strings.Count(calls, "commit "+pkg+"\n") +
			strings.Count(calls, "commit "+pkg+"$"); n > 1 {
			t.Errorf("коммитов пакета %s: %d", pkg, n)
		}
	}
	if ci, ai := callIndex(f.Calls, "commit firewall"), callIndex(f.Calls, "apply-bridge enable"); ci < 0 || ai < 0 || ci > ai {
		t.Errorf("применение вызвано раньше коммита firewall (commit=%d apply=%d)", ci, ai)
	}
}

// Маска берётся из ЖИВОЙ маски uplink, а не из литерала /24.
func TestBridgeEnableTakesMaskFromUplink(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Fixtures["ubus network.interface.wwan status"] = []byte(
		`{"up":true,"l3_device":"` + bWwanDev + `","ipv4-address":[{"address":"10.0.0.234","mask":16}],` +
			`"route":[{"target":"0.0.0.0","mask":0,"nexthop":"10.0.0.1"}]}`)
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, "10.0.5.5")
	f.QueueFixture("bridge status", scriptStatus(true, false, 1), scriptStatus(true, true, 1))

	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"10.0.5.5","pc_ip":"10.0.5.6"}`)
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, "set network.homelan.netmask=255.255.0.0") {
		t.Error("маска ноги не из живой маски uplink — литерал /24 вернулся")
	}
	if !strings.Contains(calls, "masq_src=!10.0.0.0/16") {
		t.Error("подсеть в masq_src не из живого uplink")
	}
}

// ap_access при включении добавляет форвардинг и NAT, суженный до LAN.
func TestBridgeEnableWithAPAccess(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
	f.QueueFixture("bridge status", scriptStatus(true, false, 1), scriptStatus(true, true, 1))

	j := runBridgeOp(t, s, "/api/bridge/enable",
		`{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`","ap_access":true}`)
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	for _, want := range []string{
		"add-named firewall.netmode_fwd_l2h=forwarding",
		"set firewall.netmode_fwd_l2h.src=lan",
		"set firewall.netmode_fwd_l2h.dest=homelan",
		"set firewall.netmode_homelan.masq=1",
		"add_list firewall.netmode_homelan.masq_src=192.168.9.0/24",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("нет %q", want)
		}
	}
}

// Установка relayd идёт ДО первой записи uci: её отказ оставляет
// конфигурацию нетронутой.
func TestBridgeEnableInstallsBeforeAnyWrite(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Fixtures["bridge status"] = scriptStatus(false, false, 1)
	f.BridgeExitCodes["install"] = 9

	j := runBridgeOp(t, s, "/api/bridge/enable",
		`{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`","install":true}`)
	wantBridgeFailure(t, s, j, breasonInstallFailed)
	for _, c := range f.Calls {
		if isUCICall(c) || strings.HasPrefix(c, "add_list") || strings.HasPrefix(c, "del_list") {
			t.Errorf("после отказа установки осталась запись: %s", c)
		}
	}
}

// Отказ в середине партии обязан отменить СВОЙ черновик и не оставить
// ничего в стейджинге.
func TestBridgeEnableRevertsDraftOnMidBatchFailure(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	f.ErrorsBySuffix["network.relay.proto=relay"] = errors.New("uci упал")

	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
	wantBridgeFailure(t, s, j, breasonApplyFailed)

	if n := len(f.CallsContaining("commit ")); n != 0 {
		t.Errorf("после отказа записи выполнено %d коммитов — половина партии опубликована", n)
	}
	if n := len(f.CallsContaining("revert ")); n == 0 {
		t.Error("черновик не отменён: следующий запрос вечно получит foreign_staged_changes")
	}
	if len(f.Staged) != 0 {
		t.Errorf("в стейджинге остался наш черновик: %v", f.Staged)
	}
	if n := len(f.CallsContaining("apply-bridge")); n != 0 {
		t.Error("применение вызвано после отказа записи")
	}
}

// Неудавшаяся отмена — ДРУГАЯ новость: владельцу идти в ssh.
func TestBridgeEnableStaleDraftWhenRevertFails(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	f.ErrorsBySuffix["network.relay.proto=relay"] = errors.New("uci упал")
	f.ErrorsBySuffix["revert network.relay"] = errors.New("revert тоже упал")

	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
	wantBridgeFailure(t, s, j, breasonStaleDraft)
}

// ─────────── enable: вердикт по наблюдению ───────────

func TestBridgeEnableVerdictFromObservation(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(f *executor.Fake)
		reason FailReason
	}{
		{
			"интерфейс не поднялся",
			func(f *executor.Fake) {
				f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(false, "")
				f.Fixtures["bridge status"] = scriptStatus(true, true, 1)
			},
			breasonNoIface,
		},
		{
			"интерфейс поднялся с ЧУЖИМ адресом",
			func(f *executor.Fake) {
				f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, "192.168.0.77")
				f.Fixtures["bridge status"] = scriptStatus(true, true, 1)
			},
			breasonNoIface,
		},
		{
			"интерфейс поднят, relayd не запустился",
			func(f *executor.Fake) {
				f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
				f.Fixtures["bridge status"] = scriptStatus(true, false, 1)
			},
			breasonRelayDown,
		},
		{
			"состояние не прочитано ни разу",
			func(f *executor.Fake) {
				f.Errors["ubus network.interface.homelan status"] = errors.New("ubus молчит")
				f.Fixtures["bridge status"] = scriptStatus(true, true, 1)
			},
			breasonUnverifiable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fastBridge(t)
			s, f := newServer(t)
			bridgeOff(t, f)
			tt.setup(f)

			j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
			wantBridgeFailure(t, s, j, tt.reason)
			// Применение состоялось — значит конфигурация закоммичена:
			// «не проверилось» не то же самое, что «не сделано».
			if n := len(f.CallsContaining("commit network")); n != 1 {
				t.Errorf("коммитов network: %d, ожидался 1", n)
			}
		})
	}
}

// Код возврата скрипта → причина таксономии.
func TestBridgeApplyExitCodeBecomesReason(t *testing.T) {
	tests := []struct {
		code   int
		reason FailReason
	}{
		{3, breasonBusy},
		{4, breasonApplyFailed},
		{5, breasonRelayDown},
		{7, breasonPrereqMissing},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			fastBridge(t)
			s, f := newServer(t)
			bridgeOff(t, f)
			f.BridgeExitCodes["apply enable"] = tt.code

			j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
			wantBridgeFailure(t, s, j, tt.reason)
		})
	}

	// Нет скрипта вовсе — executor_missing, а не «глаголы отказали».
	fastBridge(t)
	s, f := newServer(t)
	bridgeOff(t, f)
	f.MissingBins = []string{executor.BridgeBinPath}
	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
	wantBridgeFailure(t, s, j, breasonExecutorMissing)
}

// ─────────── disable: демонтаж ручной сборки ───────────

// Ключевой тест фазы: конфигурация снята с ЖИВОГО роутера, секции анонимны,
// имён netmode_* в ней нет ни одного. Демонтаж обязан разобрать её.
func TestBridgeDisableDismantlesManualConfig(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOn(t, f)
	// После демонтажа объект ubus исчезает вместе с интерфейсом — именно
	// это и наблюдает вердикт; relayd гаснет следом.
	f.Errors["ubus network.interface.homelan status"] = errors.New("Not found")
	f.Fixtures["bridge status"] = scriptStatus(true, false, 1)

	j := runBridgeOp(t, s, "/api/bridge/disable", "")
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	must := []string{
		// Анонимные VLAN-секции ручной сборки.
		"delete network.@bridge-vlan[1]",
		"delete network.@bridge-vlan[0]",
		"delete network." + bDevSec + ".vlan_filtering",
		"set network.lan.device=" + bBase,
		"delete network.relay",
		"delete network.homelan",
		// Анонимные зона и форвардинги.
		"delete firewall.@forwarding[2]",
		"delete firewall.@forwarding[1]",
		"delete firewall.@zone[2]",
		"del_list firewall." + bWanZone + ".masq_src=!192.168.0.0/24",
		"delete netmode.main.bridge_pc_ip",
		"apply-bridge disable",
	}
	for _, want := range must {
		if !strings.Contains(calls, want) {
			t.Errorf("демонтаж не снял %q", want)
		}
	}
	// Анонимные секции — в обратном порядке: индексы сдвигаются на каждом
	// удалении, и прямой порядок снёс бы не то.
	if a, b := callIndex(f.Calls, "delete network.@bridge-vlan[1]"), callIndex(f.Calls, "delete network.@bridge-vlan[0]"); a > b {
		t.Errorf("анонимные секции удалены в прямом порядке (%d, %d) — индексы сдвинутся", a, b)
	}
	if a, b := callIndex(f.Calls, "delete firewall.@forwarding[2]"), callIndex(f.Calls, "delete firewall.@forwarding[1]"); a > b {
		t.Error("форвардинги удалены в прямом порядке")
	}
}

// Демонтировать нечего — отказ по СОДЕРЖИМОМУ.
func TestBridgeDisableRefusesWhenNothingToRemove(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	rec := post(t, s, "/api/bridge/disable", "", bridgeFP(t, s))
	if got := errCode(t, rec); got != "already_disabled" {
		t.Errorf("код %q, ожидался already_disabled", got)
	}
}

// Демонтаж дочищает ОСТАТОК после сорванной попытки: интерфейсов уже нет,
// а зона и отрицание в masq_src остались.
func TestBridgeDisableCleansLeftovers(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOn(t, f)
	cur := string(f.Fixtures["uci show network"])
	for _, prefix := range []string{"network.homelan", "network.relay"} {
		var out []string
		for _, l := range strings.Split(cur, "\n") {
			if !strings.HasPrefix(l, prefix) {
				out = append(out, l)
			}
		}
		cur = strings.Join(out, "\n")
	}
	f.Fixtures["uci show network"] = []byte(cur)
	f.Errors["ubus network.interface.homelan status"] = errors.New("интерфейса нет")
	f.Fixtures["bridge status"] = scriptStatus(true, false, 1)

	j := runBridgeOp(t, s, "/api/bridge/disable", "")
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, "delete firewall.@zone[2]") {
		t.Error("остаток зоны не дочищен — повторный демонтаж бесполезен")
	}
}

// ─────────── access ───────────

func TestBridgeAccessTogglesFirewallOnly(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOn(t, f)

	j := runBridgeOp(t, s, "/api/bridge/access", `{"enabled":true}`)
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	if strings.Contains(calls, "commit network") {
		t.Error("доступ тронул сеть — правится только firewall")
	}
	if !strings.Contains(calls, "apply-bridge access") {
		t.Error("не вызвано применение access")
	}
	if !strings.Contains(calls, "add_list firewall.netmode_homelan.masq_src=192.168.9.0/24") {
		t.Error("NAT не сужен до LAN-подсети — транзит uplink↔ПК пошёл бы через подмену адреса")
	}
}

func TestBridgeAccessRefusals(t *testing.T) {
	// Проброс выключен — настраивать поверх нечего.
	s, f := newServer(t)
	bridgeOff(t, f)
	rec := post(t, s, "/api/bridge/access", `{"enabled":true}`, bridgeFP(t, s))
	if got := errCode(t, rec); got != "bridge_not_enabled" {
		t.Errorf("код %q, ожидался bridge_not_enabled", got)
	}

	// Уже в запрошенном состоянии.
	s2, f2 := newServer(t)
	bridgeOn(t, f2)
	rec = post(t, s2, "/api/bridge/access", `{"enabled":false}`, bridgeFP(t, s2))
	if got := errCode(t, rec); got != "already_set" {
		t.Errorf("код %q, ожидался already_set", got)
	}
}

// ─────────── общие правила ───────────

// Джоб один на демона: второй получает 409, а не встаёт в очередь.
func TestBridgeJobBusy(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	fp := bridgeFP(t, s)
	// Долгий вызов внутри джоба держит слот занятым.
	release := make(chan struct{})
	f.Errors["apply-bridge enable"] = nil
	f.Panics = map[string]string{}
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
	f.QueueFixture("bridge status", scriptStatus(true, false, 1), scriptStatus(true, true, 1))

	started := make(chan struct{})
	if _, err := s.jobs.Start("bridge", "enable", "тест", 1, func(ctx context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	rec := post(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`, fp)
	close(release)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "job_busy" {
		t.Errorf("второй джоб принят: код %d %s", rec.Code, rec.Body.String())
	}
}

// Таксономия закрыта: каждая причина известна knownBridgeReason, и ни одна
// не совпадает с фолбэком панели.
func TestBridgeTaxonomyClosed(t *testing.T) {
	if len(allBridgeReasons) < 9 {
		t.Fatalf("в таксономии %d причин — перечисление урезали?", len(allBridgeReasons))
	}
	seen := map[FailReason]bool{}
	for _, r := range allBridgeReasons {
		if seen[r] {
			t.Errorf("причина %q перечислена дважды", r)
		}
		seen[r] = true
		if !knownBridgeReason(r) {
			t.Errorf("причина %q не опознаётся собственной проверкой", r)
		}
		if r == "unknown" {
			t.Error("'unknown' — фолбэк панели, а не код демона")
		}
	}
}

// Токен обязателен на всех маршрутах моста.
func TestBridgeRoutesRequireToken(t *testing.T) {
	s, f := newServer(t)
	bridgeOff(t, f)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/bridge"},
		{"POST", "/api/bridge/enable"},
		{"POST", "/api/bridge/disable"},
		{"POST", "/api/bridge/access"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s без токена: код %d", tc.method, tc.path, rec.Code)
		}
	}
}

// ─────────── регрессии живого прогона 2026-09-01 ───────────

// Демонтаж идемпотентен: удаление того, чего нет, не отказ.
//
// Живой прогон упёрся ровно в это. Ручная сборка адрес ПК никогда не
// записывала, `uci delete netmode.main.bridge_pc_ip` вернул «Entry not
// found», и вся партия демонтажа рухнула на последнем шаге — конфигурация
// осталась собранной, а владелец получил apply_failed.
func TestBridgeDisableToleratesMissingTargets(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOn(t, f)
	f.Errors["ubus network.interface.homelan status"] = errors.New("Not found")
	f.Fixtures["bridge status"] = scriptStatus(true, false, 1)
	// Ровно то, что было на роутере: опции с адресом ПК нет, потому что
	// конфигурацию собирали руками.
	f.ErrorsBySuffix["delete netmode.main.bridge_pc_ip"] =
		errors.New("uci delete netmode.main.bridge_pc_ip: exit status 1: /sbin/uci: Entry not found")

	j := runBridgeOp(t, s, "/api/bridge/disable", "")
	if j.State != job.Done {
		t.Fatalf("демонтаж упал на отсутствующей цели: %s", jobErr(j))
	}
	if n := len(f.CallsContaining("apply-bridge disable")); n != 1 {
		t.Error("применение не вызвано — партия оборвалась до него")
	}
	// Настоящий отказ uci при этом остаётся отказом.
	s2, f2 := newServer(t)
	bridgeOn(t, f2)
	f2.ErrorsBySuffix["delete netmode.main.bridge_pc_ip"] = errors.New("uci: I/O error")
	j2 := runBridgeOp(t, s2, "/api/bridge/disable", "")
	wantBridgeFailure(t, s2, j2, breasonApplyFailed)
}

// Отмена черновика адресует секции ИМЕНАМИ ИЗ uci changes.
//
// Вторая регрессия того же прогона: `uci revert network.@bridge-vlan[0]`
// на живом роутере вернул 0 и не отменил ничего — удалённая анонимная
// секция живёт в стейджинге под внутренним именем. Пять удалений застряли
// черновиком, а демон доложил apply_failed вместо stale_draft, то есть
// посоветовал повторить там, где повтор упрётся в foreign_staged_changes.
func TestBridgeRevertAddressesSectionsFromChanges(t *testing.T) {
	s, f := newServer(t)
	bridgeOn(t, f)
	// Ломаем партию на последнем шаге firewall, когда удаления анонимных
	// секций network уже в стейджинге.
	f.ErrorsBySuffix["del_list firewall.@zone[1].masq_src=!192.168.0.0/24"] = errors.New("uci упал")

	j := runBridgeOp(t, s, "/api/bridge/disable", "")
	wantBridgeFailure(t, s, j, breasonApplyFailed)

	// Главное: стейджинг чист. Именно это и не выходило на роутере.
	if len(f.Staged) != 0 {
		t.Errorf("черновик застрял: %v", f.Staged)
	}
	// И revert адресован ВНУТРЕННИМИ именами из uci changes, а не
	// индексами: по индексу живой uci не отменяет ничего, отвечая успехом,
	// и фейк ведёт себя так же (executor.Fake.anonRevertName).
	reverts := f.CallsContaining("revert ")
	if len(reverts) == 0 {
		t.Fatal("revert не звался вовсе")
	}
	for _, byIndex := range reverts {
		if strings.Contains(byIndex, "@") {
			t.Errorf("revert адресован индексом (%q) — на роутере это молчаливый промах", byIndex)
		}
	}
	// Именованные секции при этом адресуются своими именами.
	if callIndex(f.Calls, "revert network.homelan") < 0 {
		t.Errorf("именованная секция не отменена: %v", reverts)
	}
}

// Включение ставит ХОСТ-МАРШРУТ к ПК, и без него доступ из LAN не работает
// ни при каком firewall.
//
// Регрессия живого прогона 2026-09-01. После включения в главной таблице
// два маршрута к uplink-подсети — через phy0.0-sta0 и через br-lan.2, — и
// для транзитного пакета из LAN ядро выбирает первый: пакет к ПК уходит в
// эфир uplink, где ПК нет. Таблицы relayd не спасают: его правила смотрят
// на iif phy0.0-sta0 и iif br-lan.2, а пакет из LAN приходит на br-lan.1.
// Симптом выглядел как «firewall не пускает», хотя счётчик accept уже
// считал пакеты.
func TestBridgeEnableAddsHostRouteToPC(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
	f.QueueFixture("bridge status", scriptStatus(true, false, 1), scriptStatus(true, true, 1))

	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	for _, want := range []string{
		"add-named network.netmode_pcroute=route",
		"set network.netmode_pcroute.interface=homelan",
		"set network.netmode_pcroute.target=" + bPCIP,
		"set network.netmode_pcroute.netmask=255.255.255.255",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("нет %q — из LAN пакет к ПК уйдёт в эфир uplink", want)
		}
	}
	// Маршрут не привязан к доступу из точки доступа: он про то, ГДЕ живёт
	// ПК, а доступ открывает firewall.
	if strings.Contains(calls, "netmode_fwd_l2h") {
		t.Error("маршрут потянул за собой правила доступа, которых не просили")
	}
}

// Включение ставит ПРАВИЛА маршрутизации, покрывающие виртуалки.
//
// Маршрут /32 знает только адрес ПК, а на ПК живут виртуалки со своими
// адресами в той же подсети (измерено на живом роутере: 192.168.0.5 и .250,
// MAC VirtualBox). Их адресов панель не знает и знать не может — покрыть их
// можно только тем, что relayd выучил сам.
//
// Правил два: relayd кладёт хосты одной стороны моста в базу таблиц, другой —
// в базу+1, и какая сторона куда попадёт, зависит от порядка получения
// интерфейсов. Просмотр обеих снимает зависимость от порядка.
func TestBridgeEnableAddsRelaydLookupRules(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOff(t, f)
	f.Fixtures["ubus network.interface.homelan status"] = homelanStatus(true, bLegIP)
	f.QueueFixture("bridge status", scriptStatus(true, false, 1), scriptStatus(true, true, 1))

	j := runBridgeOp(t, s, "/api/bridge/enable", `{"leg_ip":"`+bLegIP+`","pc_ip":"`+bPCIP+`"}`)
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	for _, want := range []string{
		"add-named network.netmode_rule_a=rule",
		"set network.netmode_rule_a.in=lan",
		"set network.netmode_rule_a.lookup=16800",
		"set network.netmode_rule_a.priority=3",
		"add-named network.netmode_rule_b=rule",
		"set network.netmode_rule_b.lookup=16801",
		"set network.netmode_rule_b.priority=4",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("нет %q — виртуалки останутся недоступны из Wi-Fi роутера", want)
		}
	}
	// Имя интерфейса в ПРАВИЛЕ — ЛОГИЧЕСКОЕ: netifd разворачивает его сам,
	// а захардкоженное br-lan.1 сломалось бы на другой сборке (ADR-0019).
	// Проверяются именно строки правил: br-lan.1 законно встречается в
	// партии как значение lan.device.
	for _, c := range f.CallsContaining("netmode_rule_") {
		if strings.Contains(c, "br-lan") {
			t.Errorf("в правило попало имя устройства: %s", c)
		}
	}
}

// Демонтаж снимает маршрут ПО СОДЕРЖИМОМУ — включая маршрут, заведённый
// не нами (у ручной сборки своих имён нет).
func TestBridgeDisableRemovesHostRoute(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOn(t, f)
	// Анонимный маршрут через ногу моста — так он выглядел бы у сборки,
	// сделанной руками.
	f.Fixtures["uci show network"] = append(f.Fixtures["uci show network"], []byte(
		"network.@route[0]=route\n"+
			"network.@route[0].interface='homelan'\n"+
			"network.@route[0].target='192.168.0.90'\n"+
			"network.@route[0].netmask='255.255.255.255'\n")...)
	f.Errors["ubus network.interface.homelan status"] = errors.New("Not found")
	f.Fixtures["bridge status"] = scriptStatus(true, false, 1)

	j := runBridgeOp(t, s, "/api/bridge/disable", "")
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	if !strings.Contains(strings.Join(f.Calls, "\n"), "delete network.@route[0]") {
		t.Error("маршрут к ноге моста пережил демонтаж")
	}
}

// Демонтаж снимает и ПРАВИЛА — тоже по содержимому, по таблице, в которую
// они смотрят. Оставленное правило указывало бы в пустую таблицу: вреда нет,
// но конфигурация врала бы о том, что проброс ещё жив.
func TestBridgeDisableRemovesRelaydRules(t *testing.T) {
	fastBridge(t)
	s, f := newServer(t)
	bridgeOn(t, f)
	// Анонимные правила — так они выглядели бы у сборки, сделанной руками.
	f.Fixtures["uci show network"] = append(f.Fixtures["uci show network"], []byte(
		"network.@rule[0]=rule\n"+
			"network.@rule[0].in='lan'\n"+
			"network.@rule[0].lookup='16800'\n"+
			"network.@rule[1]=rule\n"+
			"network.@rule[1].in='lan'\n"+
			"network.@rule[1].lookup='16801'\n"+
			// Чужое правило в другую таблицу трогать нельзя.
			"network.@rule[2]=rule\n"+
			"network.@rule[2].in='lan'\n"+
			"network.@rule[2].lookup='42'\n")...)
	f.Errors["ubus network.interface.homelan status"] = errors.New("Not found")
	f.Fixtures["bridge status"] = scriptStatus(true, false, 1)

	j := runBridgeOp(t, s, "/api/bridge/disable", "")
	if j.State != job.Done {
		t.Fatalf("джоб %q: %s", j.State, jobErr(j))
	}
	calls := strings.Join(f.Calls, "\n")
	for _, want := range []string{"delete network.@rule[0]", "delete network.@rule[1]"} {
		if !strings.Contains(calls, want) {
			t.Errorf("правило %q пережило демонтаж", want)
		}
	}
	if strings.Contains(calls, "delete network.@rule[2]") {
		t.Error("снесено ЧУЖОЕ правило в постороннюю таблицу — демонтаж трогает только своё")
	}
}
