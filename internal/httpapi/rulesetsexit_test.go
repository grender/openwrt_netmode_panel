package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"netmoded/internal/job"
	"netmoded/internal/mixin"
	"netmoded/internal/nikki"
)

// addExitGroup кладёт в подделку движка группу-выход профиля (ADR-0049).
func addExitGroup(t *testing.T, s *Server, name, now string) {
	t.Helper()
	nikkiFake(t, s).all[name] = nikki.Proxy{
		Name: name, Type: "Selector", Alive: true,
		Members: []string{now, "REJECT"}, Now: now, Selectable: true,
	}
}

// errorCode — код из тела ошибки.
func errorCode(t *testing.T, rec interface{ Bytes() []byte }) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(rec.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не разбирается: %s", rec.Bytes())
	}
	return e
}

// TestRulesetsPutWritesExitTargets — набор и правило в группу-выход уходят в
// файл правилами с именем группы и читаются обратно с той же целью.
func TestRulesetsPutWritesExitTargets(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	addExitGroup(t, s, "EXIT-RESI", "kz-resi")
	addExitGroup(t, s, "EXIT-DC", "kz-dc")
	sets := []mixin.Set{
		{Name: "openai", Action: mixin.ActionExit, Group: "EXIT-RESI"},
		{Name: "youtube", Action: mixin.ActionTunnel},
		{Name: "telegram", IP: true, Action: mixin.ActionExit, Group: "EXIT-DC"},
	}
	nikkiFake(t, s).ruleProviders = loadedProviders(sets...)

	rec := putRulesets(t, s, `{"policy":"direct","sets":[`+
		`{"name":"openai","action":"exit","group":"EXIT-RESI"},`+
		`{"name":"youtube","action":"tunnel"},`+
		`{"name":"telegram","action":"exit","group":"EXIT-DC"}],`+
		`"rules":[{"kind":"suffix","value":"ipinfo.io","action":"exit","group":"EXIT-RESI"}]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	if state, msg := jobOutcome(t, s); state != job.Done {
		t.Fatalf("джоб %s: %s", state, msg)
	}

	got, _ := mixinBytes(t, s)
	for _, line := range []string{
		"  - 'DOMAIN-SUFFIX,ipinfo.io,EXIT-RESI'\n",
		"  - 'RULE-SET,nm-geosite-openai,EXIT-RESI'\n",
		"  - 'RULE-SET,nm-geosite-youtube,BYPASS'\n",
		"  - 'RULE-SET,nm-geoip-telegram,EXIT-DC,no-resolve'\n",
	} {
		if !strings.Contains(string(got), line) {
			t.Errorf("в файле нет строки %q:\n%s", line, got)
		}
	}

	body := rulesetsGet(t, s)
	if g := setByName(t, body, "openai"); g["action"] != "exit" || g["group"] != "EXIT-RESI" {
		t.Errorf("openai после записи: %v", g)
	}
	if g := setByName(t, body, "youtube"); g["group"] != nil {
		t.Errorf("у туннельного набора group %v, ожидалось отсутствие", g["group"])
	}
	rules, _ := body["rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["group"] != "EXIT-RESI" {
		t.Errorf("правила после записи: %v", body["rules"])
	}
}

// TestRulesetsGetListsExitGroups — в цели предлагаются только живые группы с
// приставкой EXIT-, по имени; движок молчит — null, а не пустой список.
func TestRulesetsGetListsExitGroups(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	addExitGroup(t, s, "EXIT-RESI", "kz-resi")
	addExitGroup(t, s, "EXIT-DC", "kz-dc")
	// Узел с похожим именем — не группа, и в цели не годится.
	nikkiFake(t, s).all["EXIT-node"] = nikki.Proxy{Name: "EXIT-node", Type: "Vless", Alive: true}

	groups, _ := rulesetsGet(t, s)["exit_groups"].([]any)
	var names []string
	for _, g := range groups {
		m := g.(map[string]any)
		names = append(names, m["name"].(string))
		if m["name"] == "EXIT-RESI" && m["now"] != "kz-resi" {
			t.Errorf("EXIT-RESI now %v", m["now"])
		}
	}
	if strings.Join(names, ",") != "EXIT-DC,EXIT-RESI" {
		t.Errorf("exit_groups %v, ожидалось EXIT-DC,EXIT-RESI", names)
	}

	nikkiFake(t, s).err = errors.New("движок лёг")
	body := rulesetsGet(t, s)
	if v, ok := body["exit_groups"]; !ok || v != nil {
		t.Errorf("при молчащем движке exit_groups %v (есть ключ: %v), ожидался null", v, ok)
	}

	// Движок ответил, а EXIT-групп в профиле нет — пустой список, не null.
	s2, _ := newServer(t)
	if v, ok := rulesetsGet(t, s2)["exit_groups"].([]any); !ok || len(v) != 0 {
		t.Errorf("без EXIT-групп exit_groups %v, ожидался []", rulesetsGet(t, s2)["exit_groups"])
	}
}

// TestRulesetsPutRefusesUnknownExitGroup — правило на группу, которой нет у
// движка, не пишется: nikki с таким правилом не стартовал бы.
func TestRulesetsPutRefusesUnknownExitGroup(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"набор в несуществующую группу", `{"policy":"direct","sets":[{"name":"youtube","action":"exit","group":"EXIT-NOPE"}]}`},
		{"правило в несуществующую группу", `{"policy":"direct","sets":[],"rules":[{"kind":"suffix","value":"x.com","action":"exit","group":"EXIT-NOPE"}]}`},
		{"имя есть, но это узел", `{"policy":"direct","sets":[{"name":"youtube","action":"exit","group":"EXIT-node"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, f := newServer(t)
			addBypass(t, s)
			nikkiFake(t, s).all["EXIT-node"] = nikki.Proxy{Name: "EXIT-node", Type: "Vless", Alive: true}
			fp := rulesetsFP(t, s)
			f.Calls = nil

			rec := putRulesets(t, s, tc.body, fp)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("код %d, ожидался 400; тело %s", rec.Code, rec.Body.String())
			}
			if e := errorCode(t, rec.Body); e.Code != "unknown_group" {
				t.Errorf("код %q (%s), ожидался unknown_group", e.Code, e.Error)
			}
			if _, ok := mixinBytes(t, s); ok {
				t.Error("файл наборов записан при отказе")
			}
			if len(f.Calls) != 0 {
				t.Errorf("отказ дошёл до системы: %v", f.Calls)
			}
		})
	}
}

// TestRulesetsPutExitGroupsWhenEngineSilent — без ответа движка уже
// применённая группа проходит (проверена прошлой записью), новая — 503.
func TestRulesetsPutExitGroupsWhenEngineSilent(t *testing.T) {
	s, f := newServer(t)
	applied := mixin.Set{Name: "youtube", Action: mixin.ActionExit, Group: "EXIT-RESI"}
	writeMixin(t, s, mixin.Render(mixin.Config{
		Policy: mixin.PolicyDirect, Download: mixin.DownloadDirect, Sets: []mixin.Set{applied},
	}))
	nikkiFake(t, s).err = errors.New("движок лёг")
	fp := rulesetsFP(t, s)

	f.Calls = nil
	rec := putRulesets(t, s, `{"policy":"direct","sets":[{"name":"youtube","action":"exit","group":"EXIT-DC"}]}`, fp)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("новая группа: код %d, ожидался 503; тело %s", rec.Code, rec.Body.String())
	}
	if e := errorCode(t, rec.Body); e.Code != "groups_unverified" || !strings.Contains(e.Error, "EXIT-DC") {
		t.Errorf("ответ %+v, ожидался groups_unverified с именем группы", e)
	}
	if len(f.Calls) != 0 {
		t.Errorf("отказ дошёл до системы: %v", f.Calls)
	}

	// Применённая группа, сменилась только политика — запись идёт.
	rec = putRulesets(t, s, `{"policy":"tunnel","sets":[{"name":"youtube","action":"exit","group":"EXIT-RESI"}]}`, fp)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("применённая группа: код %d, ожидался 202; тело %s", rec.Code, rec.Body.String())
	}
	jobOutcome(t, s)
	got, _ := mixinBytes(t, s)
	if !strings.Contains(string(got), "'RULE-SET,nm-geosite-youtube,EXIT-RESI'") {
		t.Errorf("файл без правила на EXIT-RESI:\n%s", got)
	}
}

// TestRulesetsPutRejectsGroupOnTunnel — группа при tunnel/direct — ошибка
// формы, а не молча отброшенное поле.
func TestRulesetsPutRejectsGroupOnTunnel(t *testing.T) {
	s, _ := newServer(t)
	addBypass(t, s)
	addExitGroup(t, s, "EXIT-RESI", "kz-resi")
	rec := putRulesets(t, s, `{"policy":"direct","sets":[{"name":"youtube","action":"tunnel","group":"EXIT-RESI"}]}`, rulesetsFP(t, s))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400; тело %s", rec.Code, rec.Body.String())
	}
	if e := errorCode(t, rec.Body); e.Code != "bad_request" {
		t.Errorf("код %q, ожидался bad_request", e.Code)
	}
}
