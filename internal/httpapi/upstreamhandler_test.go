package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"netmoded/internal/executor"
	"netmoded/internal/job"
)

// ─────────── оснастка ───────────
//
// Имена секций и радио в тестах — литералы, и это разрешено: они приходят из
// записанной фикстуры живого роутера (docs/recon/raw/10-uci-show-wireless.txt),
// а не из догадки о том, как роутер устроен. Гейт check-no-hardcoded-if.sh
// исключает _test.go по той же причине.
const (
	secActive   = "wifinet0" // John24, включена (опции disabled нет вовсе)
	secHomeAP   = "wifinet1" // grenderNet — домашняя точка, НЕ наша
	secSaved    = "wifinet2" // ATOM, disabled='1' — цель переключения
	ssidActive  = "John24"
	ssidSaved   = "ATOM"
	stationRad  = "radio0"
	fixtureKeys = "REDACTED_PSK" // общее начало обоих паролей в фикстуре
)

// fastUpstream ужимает окна ожидания до миллисекунд.
//
// Без него каждая из восьми причин стоила бы двадцати-тридцати секунд, весь
// файл шёл бы минуты, и его первым же делом пометили бы как медленный и
// перестали гонять. Восстановление через Cleanup обязательно: пороги —
// переменные пакета, и оставленное значение поехало бы в соседние тесты.
func fastUpstream(t *testing.T) {
	t.Helper()
	assoc, ipv4, poll := upstreamAssocTimeout, upstreamIPv4Timeout, upstreamPollInterval
	upstreamAssocTimeout = 60 * time.Millisecond
	upstreamIPv4Timeout = 60 * time.Millisecond
	upstreamPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		upstreamAssocTimeout, upstreamIPv4Timeout, upstreamPollInterval = assoc, ipv4, poll
	})
}

// iwinfoBody — ответ `ubus call iwinfo info`. Пустой ssid означает «станция
// не ассоциирована»: именно так это выглядит у живого iwinfo — поля ssid
// в ответе просто нет (docs/recon/ubus.md).
func iwinfoBody(ssid string) []byte {
	if ssid == "" {
		return []byte(`{"phy":"phy0","mode":"Client"}`)
	}
	return []byte(`{"phy":"phy0","mode":"Client","ssid":` + strconv.Quote(ssid) + `}`)
}

// wwanNoIPv4 — интерфейс поднят, аренды DHCP нет. Ровно то состояние, ради
// различения которого netif.Status.Online смотрит не только на up.
func wwanNoIPv4() []byte {
	return []byte(`{"up":true,"ipv4-address":[],"route":[]}`)
}

// seesSSID заставляет iwinfo отвечать одним и тем же именем всегда.
func seesSSID(f *executor.Fake, ssid string) {
	f.Fixtures["ubus iwinfo info"] = iwinfoBody(ssid)
}

// seesThen задаёт разные ответы для снятия «прежнего» ssid и для ожидания.
func seesThen(f *executor.Fake, before, after string) {
	f.QueueFixture("ubus iwinfo info", iwinfoBody(before), iwinfoBody(after))
}

// appendWireless дописывает секции в фикстуру `uci show wireless`.
func appendWireless(f *executor.Fake, lines string) {
	f.Fixtures["uci show wireless"] = append(f.Fixtures["uci show wireless"], []byte(lines)...)
}

// makeAmbiguous включает вторую станционную секцию, убирая её disabled.
// Это первая из двух причин Ambiguous — «включено ≥ 2».
func makeAmbiguous(f *executor.Fake) {
	cur := string(f.Fixtures["uci show wireless"])
	cur = strings.ReplaceAll(cur, "wireless."+secSaved+".disabled='1'\n", "")
	f.Fixtures["uci show wireless"] = []byte(cur)
}

// switchTo делает POST /api/upstream со свежим отпечатком.
func switchTo(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, s, "/api/upstream", `{"id":"`+id+`"}`, etag(t, s))
}

// runSwitch переключается и дожидается завершения джоба.
func runSwitch(t *testing.T, s *Server, id string) *job.Job {
	t.Helper()
	rec := switchTo(t, s, id)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d, ожидался 202: %s", rec.Code, rec.Body.String())
	}
	if !s.jobs.Wait(5 * time.Second) {
		t.Fatal("джоб не завершился")
	}
	j := s.jobs.Current()
	if j == nil {
		t.Fatal("джоб исчез из статуса до проверки")
	}
	return j
}

// lastFail читает доклад из /api/status — тем же путём, каким его увидит
// панель, а не заглядывая в поле напрямую.
func lastFail(t *testing.T, s *Server) *Fail {
	t.Helper()
	rec := do(t, s, "GET", "/api/status", true)
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("статус не разбирается: %v", err)
	}
	return st.LastFail
}

// wantFailure — общая проверка неудачного переключения: джоб упал, причина
// та самая и она же осела в last_fail.
func wantFailure(t *testing.T, s *Server, j *job.Job, reason string) {
	t.Helper()
	if j.State != job.Failed {
		t.Fatalf("состояние джоба %q, ожидалось failed", j.State)
	}
	lf := lastFail(t, s)
	if lf == nil {
		t.Fatal("last_fail пуст: владелец, вернувшийся через минуту, не узнает, что не вышло")
	}
	if lf.Reason != reason {
		t.Errorf("причина %q, ожидалась %q", lf.Reason, reason)
	}
	if lf.SSID != ssidSaved {
		t.Errorf("в докладе ssid %q, ожидался %q — владелец опознаёт сеть по имени в эфире, не по секции", lf.SSID, ssidSaved)
	}
}

// callIndex — позиция точного вызова в журнале, -1 если его не было.
func callIndex(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

// isUCICall — изменяющий вызов uci (в отличие от запуска скрипта).
func isUCICall(c string) bool {
	for _, p := range []string{"set ", "delete ", "add-named ", "commit ", "revert "} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// ─────────── порядок записи ───────────

// Порядок «сначала погасить все прочие, потом включить цель» — не
// стилистика. В обратном порядке в стейджинге на несколько команд живёт
// конфигурация с двумя включёнными станционными секциями, и чужой uci commit
// ровно в этот момент опубликовал бы неоднозначность нашими руками.
func TestSwitchDisablesOthersBeforeEnablingTarget(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)

	if j := runSwitch(t, s, secSaved); j.State != job.Done {
		t.Fatalf("джоб %q, ошибка: %v", j.State, j.Error)
	}

	lastOff, firstOn := -1, -1
	for i, c := range f.Calls {
		switch {
		case strings.HasSuffix(c, ".disabled=1"):
			lastOff = i
		case strings.HasSuffix(c, ".disabled=0") && firstOn < 0:
			firstOn = i
		}
	}
	if lastOff < 0 || firstOn < 0 {
		t.Fatalf("партия записей не похожа на переключение: %v", f.Calls)
	}
	if lastOff > firstOn {
		t.Errorf("целевая включена раньше, чем погашены прочие: %v", f.Calls)
	}
	if got := callIndex(f.Calls, "set wireless."+secActive+".disabled=1"); got < 0 {
		t.Errorf("прежняя активная сеть не погашена: %v", f.Calls)
	}
	if got := callIndex(f.Calls, "set wireless."+secSaved+".disabled=0"); got < 0 {
		t.Errorf("целевая сеть не включена: %v", f.Calls)
	}
}

// Коммит один и последний среди uci-вызовов, применение — после него.
//
// Промежуточные коммиты означали бы, что состояние «включены две» или
// «включено ноль» существует в ОПУБЛИКОВАННОЙ конфигурации хотя бы
// мгновение — и переживает падение демона ровно в это мгновение.
func TestSwitchCommitsOnceAndAppliesAfter(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)

	runSwitch(t, s, secSaved)

	var commits, lastUCI, apply int
	commits, lastUCI, apply = 0, -1, -1
	for i, c := range f.Calls {
		if c == "commit wireless" {
			commits++
		}
		if isUCICall(c) {
			lastUCI = i
		}
		if strings.HasPrefix(c, "apply-upstream ") {
			apply = i
		}
	}
	if commits != 1 {
		t.Errorf("коммитов %d, допустим ровно один: %v", commits, f.Calls)
	}
	if lastUCI < 0 || f.Calls[lastUCI] != "commit wireless" {
		t.Errorf("после коммита есть ещё записи в uci: %v", f.Calls)
	}
	if apply < 0 {
		t.Fatalf("применение не вызвано: %v", f.Calls)
	}
	if apply < lastUCI {
		t.Errorf("применение раньше коммита — применили бы незаписанное: %v", f.Calls)
	}
	if f.Calls[apply] != "apply-upstream "+stationRad {
		t.Errorf("применение адресовано %q, ожидалось станционное радио %q", f.Calls[apply], stationRad)
	}
}

// Отказ UCISet в середине партии: коммита нет, черновик отменён.
//
// Без отмены недописанная партия осталась бы в стейджинге, следующий запрос
// упёрся бы в foreign_staged_changes с текстом про LuCI, которого нет, и
// панель заперла бы себя навсегда — до uci revert по ssh (ADR-0028).
func TestSwitchWriteFailureRevertsAndDoesNotCommit(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Errors["set wireless."+secSaved+".disabled=0"] = errors.New("uci занят")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonApplyFailed)

	if i := callIndex(f.Calls, "commit wireless"); i >= 0 {
		t.Errorf("коммит при неудавшейся записи: %v", f.Calls)
	}
	for _, sec := range []string{secActive, secSaved} {
		if callIndex(f.Calls, "revert wireless."+sec) < 0 {
			t.Errorf("черновик секции %s не отменён: %v", sec, f.Calls)
		}
	}
	// Отмена обязана быть НАСТОЯЩЕЙ, а не строкой в журнале: стейджинг
	// после неё пуст, и следующая запись проходит.
	changes, err := f.UCIChanges(context.Background(), "wireless")
	if err != nil {
		t.Fatalf("uci changes: %v", err)
	}
	if len(strings.TrimSpace(string(changes))) != 0 {
		t.Errorf("после отмены в стейджинге осталось: %q", changes)
	}
	if strings.HasPrefix(f.Calls[len(f.Calls)-1], "apply-upstream") {
		t.Error("применение после неудавшейся записи — применили бы чужое состояние")
	}
}

// Отмена адресная: чужой черновик, появившийся в окне между проверкой и
// отказом, она не трогает. Это и есть граница ADR-0028 — снос по пакету
// выглядел бы в остальных тестах так же, пока однажды не унёс бы чужую
// работу.
func TestSwitchRevertLeavesForeignDraftAlone(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Errors["set wireless."+secSaved+".disabled=0"] = errors.New("uci занят")

	rec := switchTo(t, s, secSaved)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if !s.jobs.Wait(5 * time.Second) {
		t.Fatal("джоб не завершился")
	}

	for _, c := range f.Calls {
		if c == "revert wireless" || c == "revert wireless." {
			t.Fatalf("отмена по пакету: %v", f.Calls)
		}
	}
	// Домашняя точка не адресована ни одной отменой — как и ни одной
	// записью.
	if hits := f.CallsContaining(secHomeAP); len(hits) != 0 {
		t.Errorf("секция домашней точки адресована: %v", hits)
	}
}

// ─────────── Ambiguous: новый вход не открыл старые двери ───────────

func TestSwitchAllowedFromAmbiguous(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	makeAmbiguous(f)
	seesSSID(f, ssidSaved)

	j := runSwitch(t, s, secSaved)
	if j.State != job.Done {
		t.Fatalf("переключение из ambiguous не прошло: %q, ошибка %v", j.State, j.Error)
	}
	if callIndex(f.Calls, "set wireless."+secActive+".disabled=1") < 0 {
		t.Errorf("вторая включённая сеть не погашена: %v", f.Calls)
	}
	if callIndex(f.Calls, "set wireless."+secSaved+".disabled=0") < 0 {
		t.Errorf("целевая сеть не включена: %v", f.Calls)
	}
}

// Нераспознанное значение disabled — ВТОРАЯ причина Ambiguous, и её тоже
// надо снять: оставив такую секцию, джоб вышел бы в ту же неоднозначность,
// ради выхода из которой переключение из Ambiguous и разрешено.
func TestSwitchDisablesSectionsWithUnparseableDisabled(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	appendWireless(f, `wireless.up_weird=wifi-iface
wireless.up_weird.device='radio0'
wireless.up_weird.mode='sta'
wireless.up_weird.network='wwan'
wireless.up_weird.ssid='Странная'
wireless.up_weird.encryption='none'
wireless.up_weird.disabled='может быть'
`)
	seesSSID(f, ssidSaved)

	j := runSwitch(t, s, secSaved)
	if j.State != job.Done {
		t.Fatalf("джоб %q, ошибка %v", j.State, j.Error)
	}
	if callIndex(f.Calls, "set wireless.up_weird.disabled=1") < 0 {
		t.Errorf("секция с нераспознанным disabled не погашена: %v", f.Calls)
	}
}

// Регресс: именованный вход для переключения не смеет разрешать правку и
// удаление при Ambiguous. Их запрет держится на другом доводе, и он не
// исчезает от того, что человек нажал кнопку (ADR-0026).
func TestAmbiguousStillBlocksEditAndDelete(t *testing.T) {
	s, f := newServer(t)
	makeAmbiguous(f)

	rec := post(t, s, "/api/wifi/networks",
		`{"id":"`+secSaved+`","ssid":"Другое"}`, etag(t, s))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "ambiguous_selection" {
		t.Errorf("правка при ambiguous: код %d/%s, ожидался 409 ambiguous_selection", rec.Code, errCode(t, rec))
	}

	rec = del(t, s, "/api/wifi/networks/"+secSaved, etag(t, s))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "ambiguous_selection" {
		t.Errorf("удаление при ambiguous: код %d/%s, ожидался 409 ambiguous_selection", rec.Code, errCode(t, rec))
	}

	if len(f.Calls) != 0 {
		t.Errorf("отвергнутые запросы что-то записали: %v", f.Calls)
	}
}

// ─────────── отказы ДО записи ───────────

func TestSwitchToAlreadySelectedIsConflict(t *testing.T) {
	s, f := newServer(t)

	rec := switchTo(t, s, secActive)
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "already_selected" {
		t.Errorf("код ошибки %q, ожидался already_selected", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("применять нечего, а записи сделаны: %v", f.Calls)
	}
}

func TestSwitchRejectsUnsupportedField(t *testing.T) {
	s, f := newServer(t)

	rec := post(t, s, "/api/upstream", `{"id":"`+secSaved+`","ssid":"мимо"}`, etag(t, s))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "unsupported_field" {
		t.Errorf("код ошибки %q, ожидался unsupported_field", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("отвергнутый запрос что-то записал: %v", f.Calls)
	}
}

func TestSwitchRequiresFingerprint(t *testing.T) {
	s, f := newServer(t)

	rec := post(t, s, "/api/upstream", `{"id":"`+secSaved+`"}`, "")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "fingerprint_required" {
		t.Errorf("код %d/%s, ожидался 409 fingerprint_required", rec.Code, errCode(t, rec))
	}
	rec = post(t, s, "/api/upstream", `{"id":"`+secSaved+`"}`, "sha256:0000000000000000")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "fingerprint_mismatch" {
		t.Errorf("код %d/%s, ожидался 409 fingerprint_mismatch", rec.Code, errCode(t, rec))
	}
	if len(f.Calls) != 0 {
		t.Errorf("отвергнутые запросы что-то записали: %v", f.Calls)
	}
}

// Чужой стейджинг: наш commit опубликовал бы чужой черновик целиком, а
// следующее применение немедленно унесло бы его в живую систему — теперь уже
// с настоящим эффектом на радио.
func TestSwitchRefusesForeignStagedChanges(t *testing.T) {
	s, f := newServer(t)
	tag := etag(t, s)
	f.Staged["wireless"] = "wireless." + secSaved + ".ssid='кто-то правит'\n"

	rec := post(t, s, "/api/upstream", `{"id":"`+secSaved+`"}`, tag)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "foreign_staged_changes" {
		t.Errorf("код %d/%s, ожидался 409 foreign_staged_changes", rec.Code, errCode(t, rec))
	}
	if len(f.Calls) != 0 {
		t.Errorf("записи при чужом черновике: %v", f.Calls)
	}
}

// Домашняя точка не является участником выбора upstream ни при каком
// состоянии конфигурации: адресовать её нельзя даже на чтение ради записи.
func TestSwitchToHomeAPIsNotFound(t *testing.T) {
	s, f := newServer(t)

	rec := switchTo(t, s, secHomeAP)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404: %s", rec.Code, rec.Body.String())
	}
	if len(f.Calls) != 0 {
		t.Errorf("записи при попытке адресовать домашнюю точку: %v", f.Calls)
	}
}

// Неполная секция отвергается ДО записи: роутер принял бы такую
// конфигурацию молча, станция не поднялась бы, и владелец получил бы
// not_associated — диагноз, уводящий искать проблему в эфире, хотя она в
// файле.
func TestSwitchRefusesIncompleteNetwork(t *testing.T) {
	cases := []struct {
		name    string
		section string
		lines   string
	}{
		{"без ssid", "up_nossid", `wireless.up_nossid=wifi-iface
wireless.up_nossid.device='radio0'
wireless.up_nossid.mode='sta'
wireless.up_nossid.network='wwan'
wireless.up_nossid.encryption='none'
wireless.up_nossid.disabled='1'
`},
		{"шифрование без пароля", "up_nokey", `wireless.up_nokey=wifi-iface
wireless.up_nokey.device='radio0'
wireless.up_nokey.mode='sta'
wireless.up_nokey.network='wwan'
wireless.up_nokey.ssid='БезПароля'
wireless.up_nokey.encryption='psk2'
wireless.up_nokey.disabled='1'
`},
		{"чужая L3-сеть", "up_lan", `wireless.up_lan=wifi-iface
wireless.up_lan.device='radio0'
wireless.up_lan.mode='sta'
wireless.up_lan.network='lan'
wireless.up_lan.ssid='НеТуда'
wireless.up_lan.encryption='none'
wireless.up_lan.disabled='1'
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, f := newServer(t)
			appendWireless(f, c.lines)

			rec := switchTo(t, s, c.section)
			if rec.Code != http.StatusConflict {
				t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != "network_incomplete" {
				t.Errorf("код ошибки %q, ожидался network_incomplete", got)
			}
			if len(f.Calls) != 0 {
				t.Errorf("отвергнутый запрос что-то записал: %v", f.Calls)
			}
		})
	}
}

// Джоб берётся ДО первой записи. Доказательство — ноль записей при
// job_busy: возьми мы его после, отбитый запрос успел бы опубликовать
// намерение, которое никто не применяет.
func TestSwitchJobBusyWritesNothing(t *testing.T) {
	s, f := newServer(t)

	release := make(chan struct{})
	if _, err := s.jobs.Start("test", "", "занято", 1, func(context.Context) error {
		<-release
		return nil
	}); err != nil {
		t.Fatalf("подготовительный джоб: %v", err)
	}
	defer func() {
		close(release)
		s.jobs.Wait(5 * time.Second)
	}()

	rec := switchTo(t, s, secSaved)
	if rec.Code != http.StatusConflict {
		t.Fatalf("код %d, ожидался 409: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "job_busy" {
		t.Errorf("код ошибки %q, ожидался job_busy", got)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("job_busy оставил опубликованное намерение: %v", f.Calls)
	}
}

// ─────────── вердикт по ассоциации ───────────

// ГЛАВНЫЙ тест фазы. Скрипт вернул 0 — и это ничего не значит: измерено, что
// `reconf` без reload возвращает 0, не изменив ассоциации (ADR-0025,
// «мина»). Судить по коду возврата здесь — ровно тот класс ошибок, ради
// закрытия которого написан ADR.
func TestSwitchStaleApplyIsFailure(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	// Применение «удалось», а станция как была на John24, так и осталась.
	f.UpstreamExitCode = 0
	seesSSID(f, ssidActive)

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonStayedOnPrevious)

	if callIndex(f.Calls, "apply-upstream "+stationRad) < 0 {
		t.Fatal("применение не вызывалось — тест проверяет не то")
	}
	if j.Error == nil || !strings.Contains(*j.Error, ssidActive) {
		t.Errorf("в тексте ошибки нет прежней сети: %v", j.Error)
	}
}

func TestSwitchOtherSSIDIsFailure(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	// До записи станция на John24, после применения уехала на третью сеть.
	seesThen(f, ssidActive, "СовсемДругая")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonOtherSSID)
}

func TestSwitchNotAssociatedIsFailure(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, "") // станция не ассоциирована ни до, ни после

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonNotAssociated)
}

// Прочитать состояние не удалось — значит мы не знаем, переключилось ли.
// Сказать «не переключилось» было бы выдумкой, и владелец пошёл бы чинить
// возможно работающую сеть.
func TestSwitchUnverifiableWhenIwinfoSilent(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	f.Errors["ubus iwinfo info"] = errors.New("ubus не отвечает")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonUnverifiable)
}

// Ассоциация есть, адреса нет. Панель, показавшая успех, увела бы владельца
// искать причину в движках обхода.
func TestSwitchNoIPv4IsFailure(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Fixtures["ubus network.interface.wwan status"] = wwanNoIPv4()

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonNoIPv4)
}

// ─────────── исходы применения ───────────

func TestSwitchApplyExitCodesMapToReasons(t *testing.T) {
	cases := []struct {
		code   int
		reason string
	}{
		{3, reasonBusy},
		{4, reasonApplyFailed},
		{7, reasonPrereqMissing},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			fastUpstream(t)
			s, f := newServer(t)
			seesSSID(f, ssidSaved)
			f.UpstreamExitCode = c.code

			j := runSwitch(t, s, secSaved)
			wantFailure(t, s, j, c.reason)

			// Коммит состоялся до применения: намерение опубликовано, и
			// доклад обязан быть об этом честным.
			if callIndex(f.Calls, "commit wireless") < 0 {
				t.Errorf("коммита не было, значит проверяется не путь применения: %v", f.Calls)
			}
		})
	}
}

// ─────────── что переключение не трогает ───────────

func TestSwitchNeverTouchesHomeAPOrMode(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)

	runSwitch(t, s, secSaved)

	if hits := f.CallsContaining(secHomeAP); len(hits) != 0 {
		t.Errorf("секция домашней точки адресована: %v", hits)
	}
	if hits := f.CallsContaining("wifi-device"); len(hits) != 0 {
		t.Errorf("адресована секция wifi-device: %v", hits)
	}
	if hits := f.CallsContaining("apply-mode"); len(hits) != 0 {
		t.Errorf("вызвано применение РЕЖИМА: режим не менялся: %v", hits)
	}
	if hits := f.CallsContaining("netmode."); len(hits) != 0 {
		t.Errorf("тронут пакет netmode: %v", hits)
	}
	// Объект ubus `network` дал бы демону универсальный `network restart`
	// отовсюду. Его нет в allowedUbusObjects, и путь переключения не должен
	// становиться поводом его туда добавить: network reload делает скрипт.
	for _, r := range f.Reads() {
		if strings.HasPrefix(r, "ubus network ") {
			t.Errorf("вызов к объекту ubus network: %q", r)
		}
	}
}

// ─────────── пароль ───────────

// Пароль не уезжает наружу ни одним из двух путей, по которым текст
// покидает демона: тело отказа и job.error (ADR-0012).
func TestSwitchNeverLeaksKey(t *testing.T) {
	fastUpstream(t)

	t.Run("в теле 409", func(t *testing.T) {
		s, f := newServer(t)
		appendWireless(f, `wireless.up_nokey=wifi-iface
wireless.up_nokey.device='radio0'
wireless.up_nokey.mode='sta'
wireless.up_nokey.network='wwan'
wireless.up_nokey.ssid='БезПароля'
wireless.up_nokey.encryption='psk2'
wireless.up_nokey.disabled='1'
`)
		rec := switchTo(t, s, "up_nokey")
		if strings.Contains(rec.Body.String(), fixtureKeys) {
			t.Errorf("пароль в теле отказа: %s", rec.Body.String())
		}
	})

	t.Run("в job.error", func(t *testing.T) {
		s, f := newServer(t)
		seesSSID(f, ssidActive) // останется на прежней — джоб упадёт
		f.Errors["set wireless."+secSaved+".disabled=0"] = errors.New("uci занят")

		j := runSwitch(t, s, secSaved)
		if j.Error == nil {
			t.Fatal("джоб упал без текста ошибки")
		}
		if strings.Contains(*j.Error, fixtureKeys) {
			t.Errorf("пароль в job.error: %s", *j.Error)
		}
	})
}

// ─────────── last_fail ───────────

// Успешное переключение стирает прошлую неудачу: доклад о том, чего уже нет,
// заставит владельца чинить починенное.
func TestSuccessfulSwitchClearsLastFail(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)

	// Сначала неудача — настоящая, а не подставленная в поле.
	seesSSID(f, ssidActive)
	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonStayedOnPrevious)

	// Затем удача. Возвращаемся на John24: первая попытка успела
	// закоммитить намерение (упала она уже на вердикте), и wifinet2 теперь
	// единственная включённая — второй раз на неё не переключишься.
	seesSSID(f, ssidActive)
	if j := runSwitch(t, s, secActive); j.State != job.Done {
		t.Fatalf("второе переключение %q, ошибка %v", j.State, j.Error)
	}
	if lf := lastFail(t, s); lf != nil {
		t.Errorf("last_fail пережил успех: %+v", lf)
	}
}

// last_fail читается мимо кэша статуса: полсекунды задержки означали бы, что
// панель показывает «всё в порядке» уже после провала, а стёртая неудача ещё
// полсекунды висит рядом с работающей сетью.
func TestLastFailIsNotDelayedByStatusCache(t *testing.T) {
	s, _ := newServer(t)

	// Прогреваем кэш статуса.
	if lf := lastFail(t, s); lf != nil {
		t.Fatalf("на старте last_fail не пуст: %+v", lf)
	}
	s.fails.Set(ssidSaved, reasonNoIPv4, time.Now())

	lf := lastFail(t, s)
	if lf == nil || lf.Reason != reasonNoIPv4 {
		t.Fatalf("свежая неудача не видна сразу: %+v", lf)
	}
	s.fails.Clear()
	if lf := lastFail(t, s); lf != nil {
		t.Errorf("стёртая неудача всё ещё показывается: %+v", lf)
	}
}

// Причина — машинный код из таксономии, а не русская фраза: переводит её
// панель. Русский текст в этом поле означал бы словарь на роутере, то есть
// копию web/i18n.js.
func TestLastFailReasonIsMachineCode(t *testing.T) {
	all := []string{
		reasonApplyFailed, reasonBusy, reasonPrereqMissing, reasonStayedOnPrevious,
		reasonOtherSSID, reasonNotAssociated, reasonNoIPv4, reasonUnverifiable,
	}
	if len(all) != 8 {
		t.Fatalf("причин %d, таксономия зафиксирована на восьми", len(all))
	}
	seen := map[string]bool{}
	for _, r := range all {
		if seen[r] {
			t.Errorf("причина %q повторяется", r)
		}
		seen[r] = true
		// Строчная латиница, цифры и подчёркивание — и ничего больше.
		// Кириллица здесь означала бы словарь на роутере, то есть копию
		// web/i18n.js; пробел — фразу, которую панель не сможет перевести.
		for _, ch := range r {
			ok := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_'
			if !ok {
				t.Errorf("причина %q не машинный код: символ %q", r, ch)
			}
		}
	}
}

// ─────────── switchable считает сервер ───────────

// Правило живёт в одном месте, и это место — сервер. Панель, вычисляющая его
// на JS, стала бы вторым местом, где оно разъедется.
func TestSwitchableMatchesServerDecision(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)

	rec := do(t, s, "GET", "/api/wifi/networks", true)
	var got struct {
		Networks []struct {
			ID         *string `json:"id"`
			Enabled    bool    `json:"enabled"`
			Switchable bool    `json:"switchable"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}

	for _, n := range got.Networks {
		if n.ID == nil {
			continue
		}
		wantSwitchable := *n.ID != secActive
		if n.Switchable != wantSwitchable {
			t.Errorf("сеть %s: switchable=%v, ожидалось %v", *n.ID, n.Switchable, wantSwitchable)
		}
		// Сервер и обработчик обязаны говорить одно: там, где панель не
		// рисует кнопку, POST отвечает 409, и наоборот.
		rec := switchTo(t, s, *n.ID)
		if n.Switchable && rec.Code == http.StatusConflict && errCode(t, rec) == "already_selected" {
			t.Errorf("сеть %s помечена switchable, но обработчик ответил already_selected", *n.ID)
		}
		if !n.Switchable && rec.Code == http.StatusAccepted {
			t.Errorf("сеть %s не помечена switchable, но переключение принято", *n.ID)
		}
		s.jobs.Wait(5 * time.Second)
	}
}

// Ассоциация есть, а состояние внешнего канала прочитать не удалось. Это
// unverifiable, а не no_ipv4: «адреса нет» и «мы не спросили» — разные
// новости, и вторая не даёт права утверждать первую.
func TestSwitchUnverifiableWhenUpstreamStatusSilent(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Errors["ubus network.interface.wwan status"] = errors.New("ubus не отвечает")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonUnverifiable)
	if j.Error == nil || strings.Contains(*j.Error, "не получил адрес") {
		t.Errorf("текст утверждает отсутствие адреса там, где мы просто не спросили: %v", j.Error)
	}
}
