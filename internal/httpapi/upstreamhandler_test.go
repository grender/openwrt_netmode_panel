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
// Без него каждая из причин таксономии стоила бы двадцати-тридцати секунд, весь
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
func wantFailure(t *testing.T, s *Server, j *job.Job, reason FailReason) {
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

// jobErr — текст ошибки джоба для сообщений теста.
//
// Без него в отчёте о провале печатается адрес указателя, и разбирать
// упавший тест приходится в отладчике вместо чтения строки.
func jobErr(j *job.Job) string {
	if j == nil || j.Error == nil {
		return "<текста ошибки нет>"
	}
	return *j.Error
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
		reason FailReason
	}{
		{3, reasonBusy},
		{4, reasonApplyFailed},
		{7, reasonPrereqMissing},
	}
	for _, c := range cases {
		t.Run(string(c.reason), func(t *testing.T) {
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
// Список здесь НЕ переписывается: тест ходит в allReasons — то самое
// единственное перечисление. Раньше он держал свою копию из восьми строк, и
// девятую причину можно было добавить так, что промолчали бы и компилятор, и
// go vet, и он сам.
func TestLastFailReasonIsMachineCode(t *testing.T) {
	all := allReasons
	if len(all) == 0 {
		t.Fatal("таксономия пуста: тест проверял бы пустоту")
	}
	seen := map[FailReason]bool{}
	for _, r := range all {
		if seen[r] {
			t.Errorf("причина %q повторяется", r)
		}
		seen[r] = true
		// Причина, не опознаваемая воронкой, до владельца доедет
		// непереведённым кодом: knownReason и allReasons обязаны описывать
		// один и тот же набор.
		if !knownReason(r) {
			t.Errorf("причина %q есть в allReasons, но knownReason её не знает", r)
		}
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

// ─────────── оснастка: журнал, медленное окно, чужая точка ───────────

// Приёмник журнала — общий с проверками статуса (logSink в status_test.go):
// второй такой же тип означал бы две оснастки для одного предмета.
//
// Журнал приходится проверять потому, что половина честности этой фазы живёт
// не в ответе, а в syslog: причина, по которой чтение не удалось, наружу не
// уезжает вовсе — без проверки она пропадала первой же правкой.

// count — сколько строк журнала содержат подстроку. Именно количество, а не
// «встречается ли»: строка на каждый опрос — это до сорока записей за окно
// ожидания в syslog роутера, который пишет на overlay-флеш.
func (l *logSink) count(sub string) int { return len(l.matching(sub)) }

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// captureLog перехватывает журнал сервера.
//
// Подменяется s.logf, а не конфигурация: читатель статуса держит собственную
// ссылку на журнал, и через него в приёмник текла бы деградация источников —
// то есть шум, в котором проверяемая строка неотличима от фоновой.
func captureLog(s *Server) *logSink {
	l := &logSink{}
	s.logf = l.logf
	return l
}

// slowUpstream растягивает окно ожидания: нужен тестам, которые смотрят на
// состояние демона ПОКА джоб идёт, а не после него.
func slowUpstream(t *testing.T, window time.Duration) {
	t.Helper()
	assoc, ipv4, poll := upstreamAssocTimeout, upstreamIPv4Timeout, upstreamPollInterval
	upstreamAssocTimeout = window
	upstreamIPv4Timeout = window
	upstreamPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		upstreamAssocTimeout, upstreamIPv4Timeout, upstreamPollInterval = assoc, ipv4, poll
	})
}

// secGuestAP — ВКЛЮЧЁННАЯ точка доступа на СТАНЦИОННОМ радио.
//
// В фикстуре живого роутера домашняя точка сидит на другом радио, и её
// отсекает уже проверка device. Из-за этого вторая половина правила «наша
// секция — это wifi-iface с mode=sta на станционном радио» не участвовала ни
// в одной проверке: убери сравнение mode из isStationSection, из resolve или
// из wireless.ours — и все тесты остаются зелёными.
//
// Такая секция на роутере обычна: гостевая сеть на 2.4 ГГц рядом со
// станцией. Она же — конфигурация после перепрошивки, ради которой написан
// ADR-0019. Погасив её партией переключения, панель уронила бы сеть, которой
// не касалась ни одна кнопка.
const secGuestAP = "guest_ap"

func appendGuestAPOnStationRadio(f *executor.Fake) {
	appendWireless(f, `wireless.`+secGuestAP+`=wifi-iface
wireless.`+secGuestAP+`.device='radio0'
wireless.`+secGuestAP+`.mode='ap'
wireless.`+secGuestAP+`.network='lan'
wireless.`+secGuestAP+`.ssid='Гостевая'
wireless.`+secGuestAP+`.encryption='psk2'
wireless.`+secGuestAP+`.key='REDACTED_PSK_GUEST'
`)
}

// ─────────── прежняя сеть: прочитали или нет ───────────

// Самый частый повод нажать кнопку — переключение С УПАВШЕЙ СЕТИ, и ровно на
// нём выброшенный признак «прочитали ли» превращал доклад в дезинформацию.
//
// Прежнюю сеть прочитать не удалось, применение отработало вхолостую
// (измеренная мина ADR-0025), станция осталась там же. Вердикт по одному лишь
// prev == "" падал в other_ssid: «подключилась к какой-то третьей сети,
// проверьте эфир» — и владелец шёл искать несуществующую точку доступа,
// вместо того чтобы нажать кнопку ещё раз.
func TestSwitchWithUnreadPrevIsNotConfidentOtherSSID(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	log := captureLog(s)
	// Первый ответ iwinfo не разбирается — ubus молчит ровно в тот момент,
	// когда снимается прежняя сеть. Дальше станция прекрасно видна, и видна
	// она на ПРЕЖНЕЙ сети.
	f.QueueFixture("ubus iwinfo info", []byte("не json"), iwinfoBody(ssidActive))

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonUnverifiable)

	if j.Error == nil || strings.Contains(*j.Error, "не к той сети") {
		t.Errorf("текст уверенно называет третью сеть там, где прежняя не прочитана: %s", jobErr(j))
	}
	if log.count("прежняя сеть не прочитана") == 0 {
		t.Errorf("причина неудачного чтения не доехала до журнала:\n%s", log.all())
	}
}

// Обратная ветка, и она обязана остаться прежней: пустой, но ПРОЧИТАННЫЙ
// prev — законный other_ssid. Станция не была ассоциирована ни с чем, теперь
// ассоциирована с третьей сетью — это ровно та новость, о которой панель
// говорит «проверьте эфир поблизости».
func TestSwitchWithReadEmptyPrevIsStillOtherSSID(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesThen(f, "", "СовсемДругая")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonOtherSSID)
}

// ─────────── воронка таксономии ───────────

// Причина вне allReasons доедет до панели непереведённым кодом, и владелец
// увидит «причина неизвестна, обновите панель» — панель, которая уже
// обновлена. Компилятор такого не ловит (FailReason — не закрытая сумма),
// значит обязан ловить журнал.
func TestSwitchFailedShoutsAboutReasonOutsideTaxonomy(t *testing.T) {
	s, _ := newServer(t)
	log := captureLog(s)

	s.switchFailed(ssidSaved, FailReason("сеть_не_взлетела"), errors.New("тест"))

	if log.count("вне таксономии") == 0 {
		t.Errorf("расхождение с таксономией прошло молча:\n%s", log.all())
	}
	// Доклад всё равно записан: сказать владельцу хоть что-то честнее, чем
	// не сказать ничего.
	if lf := lastFail(t, s); lf == nil {
		t.Error("незнакомая причина отброшена вместе с докладом")
	}

	// А законная причина кричать не смеет: сторож, срабатывающий всегда,
	// перестают читать через неделю.
	s2, _ := newServer(t)
	log2 := captureLog(s2)
	s2.switchFailed(ssidSaved, reasonNoIPv4, errors.New("тест"))
	if log2.count("вне таксономии") != 0 {
		t.Errorf("сторож сработал на законной причине:\n%s", log2.all())
	}
}

// ─────────── неудачи чтения в журнале ───────────

// «Проверить результат не удалось» в панели и ровно та же тавтология в
// logread — это доклад, из которого нельзя узнать ничего. Причина обязана
// быть в журнале, и обязана быть там ОДИН раз: строка на каждый опрос — до
// сорока записей за окно на overlay-флеш роутера.
func TestSwitchLogsWhyAssociationUnverifiable(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	log := captureLog(s)
	f.Errors["ubus iwinfo info"] = errors.New("ubus не отвечает")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonUnverifiable)

	n := log.count("ubus не отвечает")
	if n == 0 {
		t.Errorf("причина, по которой вердикт неизвестен, нигде не записана:\n%s", log.all())
	}
	// Один раз за снятие прежней сети плюс один за окно ожидания. Больше —
	// значит печатаем в цикле.
	if n > 2 {
		t.Errorf("причина печатается в цикле: %d строк\n%s", n, log.all())
	}
}

// То же для второго этапа: ошибку ubus и ошибку разбора awaitIPv4 выбрасывал
// обе.
func TestSwitchLogsWhyUpstreamStatusUnread(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	log := captureLog(s)
	seesSSID(f, ssidSaved)
	f.Errors["ubus network.interface.wwan status"] = errors.New("wwan молчит")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonUnverifiable)

	n := log.count("wwan молчит")
	if n == 0 {
		t.Errorf("причина, по которой адрес не проверен, нигде не записана:\n%s", log.all())
	}
	if n > 1 {
		t.Errorf("причина печатается в цикле: %d строк\n%s", n, log.all())
	}
}

// ─────────── исход применения неизвестен ───────────

// Скрипт, снятый по таймауту, кода возврата не имеет. Раньше он попадал в
// default и объявлялся apply_failed, чей текст обещает «оба способа
// применения отказали», а верификация пропускалась вовсе. Но `reconf` мог
// отработать за сотые доли секунды — и станция уже на целевой сети.
func TestSwitchVerifiesWhenApplyOutcomeUnknown(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved) // станция УЖЕ на целевой сети
	f.UpstreamExitCode = -1

	j := runSwitch(t, s, secSaved)
	if j.State != job.Done {
		t.Fatalf("джоб %q при работающем переключении, ошибка: %s", j.State, jobErr(j))
	}
	if lf := lastFail(t, s); lf != nil {
		t.Errorf("доклад о неудаче поверх удавшегося переключения: %+v", lf)
	}
}

// И наоборот: неизвестный исход применения не превращает вердикт в успех.
// Вердикт по-прежнему выносит ассоциация — просто теперь до неё доходит.
func TestSwitchUnknownApplyOutcomeStillJudgedByAssociation(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidActive) // станция как была на прежней, так и осталась
	f.UpstreamExitCode = -1

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonStayedOnPrevious)
}

// ─────────── провал отмены черновика ───────────

// Если отмена не удалась, наш черновик остаётся в стейджинге, и следующий
// запрос навсегда получает foreign_staged_changes с текстом «вероятно, открыт
// LuCI» — а там пусто, черновик наш. Два сообщения противоречат друг другу, и
// оба неверны. Значит провал отмены обязан доехать до владельца отдельной
// причиной и назвать команду, которой он вытащит панель из тупика.
func TestSwitchRevertFailureReachesOwner(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Errors["set wireless."+secSaved+".disabled=0"] = errors.New("uci занят")
	f.Errors["revert wireless."+secSaved] = errors.New("uci не отвечает")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonStaleDraft)

	if j.Error == nil || !strings.Contains(*j.Error, "uci revert wireless") {
		t.Errorf("владельцу не названа команда, которой снимается застрявший черновик: %s", jobErr(j))
	}
}

// ─────────── слот неудачи ───────────

// Слот взводится девятью путями, а стирался одним — успехом нашего же
// переключения. Владелец, починивший DHCP на той стороне, видел красный блок
// «подключилась, но без адреса» рядом с работающей сетью до перезапуска
// демона.
//
// Здесь проверяется первая половина лечения: новая попытка стирает вердикт
// старой ПЕРВОЙ же строкой, а не в конце.
func TestSwitchClearsPreviousFailBeforeItStarts(t *testing.T) {
	slowUpstream(t, time.Second)
	s, f := newServer(t)
	seesSSID(f, ssidActive) // джоб в итоге упадёт, но не скоро
	s.fails.Set("ПрежняяСеть", reasonNoIPv4, time.Now())

	rec := switchTo(t, s, secSaved)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	defer s.jobs.Wait(5 * time.Second)

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		lf := lastFail(t, s)
		if lf == nil {
			return
		}
		if lf.SSID != "ПрежняяСеть" {
			t.Fatalf("слот занят вердиктом новой попытки — проверяется не то: %+v", lf)
		}
		if time.Now().After(deadline) {
			t.Fatal("доклад прошлой неудачи висит рядом с идущей операцией")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Паника в теле джоба давала failed без last_fail: панель говорит «причина
// ещё не доехала, подождите», а она не доедет никогда — писать её уже
// некому.
func TestSwitchPanicReachesOwnerAsReason(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Panics["set wireless."+secSaved+".disabled=0"] = "внезапно всё"

	j := runSwitch(t, s, secSaved)
	if j.State != job.Failed {
		t.Fatalf("состояние джоба %q, ожидалось failed", j.State)
	}
	lf := lastFail(t, s)
	if lf == nil {
		t.Fatal("паника оставила владельца с «причина ещё не доехала» навсегда")
	}
	// unverifiable, а не apply_failed: где именно рухнуло — неизвестно, а
	// значит неизвестно и то, тронута ли живая система.
	if lf.Reason != reasonUnverifiable {
		t.Errorf("причина %q, ожидалась %q", lf.Reason, reasonUnverifiable)
	}
	// Паника обязана быть проброшена дальше: стек и текст — работа safe.Do,
	// и проглотить её здесь значило бы отдать владельцу done после падения.
	if j.Error == nil || !strings.Contains(*j.Error, "внезапно всё") {
		t.Errorf("текст паники не доехал до job.error: %s", jobErr(j))
	}
}

// Слот, которого нет, нечего заполнять и нечего стирать. Раньше switchFailed
// проверял указатель, а соседний verifySwitch звал Clear() без проверки: две
// функции в одном файле жили по разным правилам, и вторая ждала своего nil.
func TestFailStoreWithoutSlotIsSafe(t *testing.T) {
	var fs *failStore
	fs.Clear()
	fs.Set(ssidSaved, reasonNoIPv4, time.Now())
	if got := fs.Get(); got != nil {
		t.Errorf("несуществующий слот что-то вернул: %+v", got)
	}
}

// ─────────── точка доступа на СТАНЦИОННОМ радио ───────────

// Единственная защита домашней сети на пути записи — «wifi-iface + наше радио
// + mode=sta». В фикстуре домашняя точка сидит на другом радио, поэтому
// половина про mode не проверялась ничем.
func TestSwitchNeverTouchesAPOnStationRadio(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	appendGuestAPOnStationRadio(f)
	seesSSID(f, ssidSaved)

	j := runSwitch(t, s, secSaved)
	if j.State != job.Done {
		t.Fatalf("джоб %q, ошибка %v", j.State, j.Error)
	}
	if hits := f.CallsContaining(secGuestAP); len(hits) != 0 {
		t.Errorf("точка доступа на станционном радио погашена партией переключения: %v", hits)
	}
}

// Та же секция не является и «сохранённой сетью»: её нельзя ни показать в
// списке, ни адресовать на правку, удаление или переключение.
func TestAPOnStationRadioIsNotASavedNetwork(t *testing.T) {
	s, f := newServer(t)
	appendGuestAPOnStationRadio(f)

	rec := do(t, s, "GET", "/api/wifi/networks", true)
	var got struct {
		Networks []struct {
			ID   *string `json:"id"`
			SSID string  `json:"ssid"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	for _, n := range got.Networks {
		if n.ID != nil && *n.ID == secGuestAP {
			t.Errorf("точка доступа показана владельцу как внешняя сеть: %+v", n)
		}
	}

	for _, c := range []struct {
		name string
		call func() *httptest.ResponseRecorder
	}{
		{"правка", func() *httptest.ResponseRecorder {
			return post(t, s, "/api/wifi/networks", `{"id":"`+secGuestAP+`","ssid":"Другое"}`, etag(t, s))
		}},
		{"удаление", func() *httptest.ResponseRecorder {
			return del(t, s, "/api/wifi/networks/"+secGuestAP, etag(t, s))
		}},
		{"переключение", func() *httptest.ResponseRecorder {
			return switchTo(t, s, secGuestAP)
		}},
	} {
		if rec := c.call(); rec.Code != http.StatusNotFound {
			t.Errorf("%s точки доступа: код %d, ожидался 404: %s", c.name, rec.Code, rec.Body.String())
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("отвергнутые запросы что-то записали: %v", f.Calls)
	}
}

// ─────────── отказ коммита ───────────

// Коммит не прошёл — про живую систему известно всё, что нужно: она осталась
// прежней. Отсюда две обязанности сразу: причина apply_failed («система
// прежняя»), а не unverifiable («исход неизвестен») — это противоположные
// инструкции владельцу; и НИКАКОЙ отмены после попытки коммита: неизвестно,
// опубликовалась ли часть, и revert стал бы возвратом опубликованного, то
// есть откатом (ADR-0006, ADR-0028).
func TestSwitchCommitFailureIsApplyFailedAndNeverRollsBack(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)
	f.Errors["commit wireless"] = errors.New("uci: ошибка записи")

	j := runSwitch(t, s, secSaved)
	wantFailure(t, s, j, reasonApplyFailed)

	i := callIndex(f.Calls, "commit wireless")
	if i < 0 {
		t.Fatalf("коммит не вызывался — проверяется не тот путь: %v", f.Calls)
	}
	for _, c := range f.Calls[i+1:] {
		if strings.HasPrefix(c, "revert ") {
			t.Errorf("после попытки коммита сделан revert — это откат опубликованного: %v", f.Calls)
		}
	}
	if hits := f.CallsContaining("apply-upstream"); len(hits) != 0 {
		t.Errorf("применение при непрошедшем коммите: %v", hits)
	}
	if j.Error == nil || !strings.Contains(*j.Error, "/etc/config/wireless") {
		t.Errorf("текст не называет, что именно не удалось: %s", jobErr(j))
	}
}

// ─────────── повторная сверка отпечатка внутри джоба ───────────

// Окно между проверкой в обработчике и записью — миллисекунды, но именно в
// нём чужой Save & Apply публикует свою работу. Через HTTP это окно
// воспроизвести нечем: оно целиком внутри одного вызова. Поэтому тело джоба
// вызывается напрямую с УСТАРЕВШИМ отпечатком — ровно то состояние, ради
// которого поле fp вообще заведено в плане.
func TestSwitchJobRechecksFingerprintBeforeWriting(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)

	err := s.switchUpstream(context.Background(), switchPlan{
		radio:   stationRad,
		section: secSaved,
		ssid:    ssidSaved,
		fp:      "sha256:0000000000000000",
	})
	if err == nil {
		t.Fatal("устаревший отпечаток принят: мы погасили бы секции, которых владелец не видел")
	}
	if len(f.Calls) != 0 {
		t.Errorf("при устаревшем отпечатке что-то записано: %v", f.Calls)
	}
	lf := lastFail(t, s)
	if lf == nil || lf.Reason != reasonApplyFailed {
		t.Errorf("доклад %+v, ожидался apply_failed: система осталась прежней", lf)
	}
}

// ─────────── шаги панели: отпечаток из ТЕЛА ответа ───────────

// «Сохранить и подключиться» — два запроса подряд без промежуточного GET:
// панель берёт отпечаток из ТЕЛА ответа на создание и шлёт его как If-Match
// переключения. Все остальные проверки отпечатка ходят через хелпер со свежим
// GET, то есть обходят ровно то свойство, которым пользуется панель: убери
// поле fingerprint из тела — Go остаётся зелёным, а кнопка ломается на втором
// шаге.
func TestPanelUsesFingerprintFromCreateBody(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, "НоваяСеть")

	rec := post(t, s, "/api/wifi/networks",
		`{"ssid":"НоваяСеть","encryption":"psk2","key":"пароль12345"}`, etag(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("создание: код %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Fingerprint string `json:"fingerprint"`
		Networks    []struct {
			ID   *string `json:"id"`
			SSID string  `json:"ssid"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("разбор ответа создания: %v", err)
	}
	if body.Fingerprint == "" {
		t.Fatal("в теле ответа нет отпечатка — панели нечем сделать второй шаг без лишнего GET")
	}
	var id string
	for _, n := range body.Networks {
		if n.SSID == "НоваяСеть" && n.ID != nil {
			id = *n.ID
		}
	}
	if id == "" {
		t.Fatalf("созданная сеть не вернулась в теле: %s", rec.Body.String())
	}

	// Второй шаг — БЕЗ промежуточного GET, отпечатком из тела.
	rec = post(t, s, "/api/upstream", `{"id":"`+id+`"}`, body.Fingerprint)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("переключение отпечатком из тела: код %d, ожидался 202: %s", rec.Code, rec.Body.String())
	}
	if !s.jobs.Wait(5 * time.Second) {
		t.Fatal("джоб не завершился")
	}
	if j := s.jobs.Current(); j == nil || j.State != job.Done {
		t.Fatalf("переключение на только что созданную сеть не прошло: %+v", j)
	}
	if callIndex(f.Calls, "set wireless."+id+".disabled=0") < 0 {
		t.Errorf("созданная сеть не включена: %v", f.Calls)
	}
}

// ─────────── предел суждения демона ───────────

// ЭТО ТЕСТ-ДОКУМЕНТ, а не проверка желаемого. Он фиксирует известный предел:
// вердикт выносится по ИМЕНИ сети в эфире, а имя не уникально.
//
// Две сохранённые секции с одинаковым ssid — обычное дело: сеть пересоздали,
// сменив пароль, старую не удалили. Переключаемся на вторую, станция всё
// время остаётся на первой — и awaitAssociation возвращает успех первой же
// веткой, не дойдя до сравнения с прежней сетью. Демон докладывает done,
// last_fail пуст, а переключения не было.
//
// Различить это можно было бы по ifname или BSSID; такого замера нет
// (ADR-0027, раздел про предел суждения). Пока замера нет, поведение обязано
// быть хотя бы записанным: молчащий предел однажды примут за баг и «починят»
// догадкой.
func TestDuplicateSSIDMakesStayedOnPreviousUnreachable(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	appendWireless(f, `wireless.up_twin=wifi-iface
wireless.up_twin.device='radio0'
wireless.up_twin.mode='sta'
wireless.up_twin.network='wwan'
wireless.up_twin.ssid='`+ssidActive+`'
wireless.up_twin.encryption='psk2'
wireless.up_twin.key='REDACTED_PSK_TWIN'
wireless.up_twin.disabled='1'
`)
	// Станция всё время на СТАРОЙ секции — имя в эфире у обеих одно.
	seesSSID(f, ssidActive)

	j := runSwitch(t, s, "up_twin")
	if j.State != job.Done {
		t.Fatalf("зафиксированное поведение изменилось: джоб %q, ошибка %v", j.State, j.Error)
	}
	if lf := lastFail(t, s); lf != nil {
		t.Fatalf("зафиксированное поведение изменилось: доклад %+v", lf)
	}
}

// ─────────── что панель показывает при Ambiguous ───────────

// Переключение — единственное действие, снимающее неоднозначность, и запрет
// его из-за неё оставил бы владельцу только ssh (ADR-0026). Соседний Editable
// пришпилен, а Switchable — нет: мутация «switchable = named && !enabled»
// оставалась зелёной ровно в том поле, ради которого ADR и написан.
func TestSwitchableStaysTrueForEnabledSectionsWhenAmbiguous(t *testing.T) {
	s, f := newServer(t)
	makeAmbiguous(f)

	rec := do(t, s, "GET", "/api/wifi/networks", true)
	var got struct {
		SelectionState string `json:"selection_state"`
		Networks       []struct {
			ID         *string `json:"id"`
			Enabled    bool    `json:"enabled"`
			Editable   bool    `json:"editable"`
			Switchable bool    `json:"switchable"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.SelectionState != "ambiguous" {
		t.Fatalf("состояние %q, ожидалось ambiguous — проверяется не то", got.SelectionState)
	}

	seen := 0
	for _, n := range got.Networks {
		if n.ID == nil || !n.Enabled {
			continue
		}
		seen++
		if !n.Switchable {
			t.Errorf("сеть %s включена при ambiguous и не switchable: владельцу остаётся только ssh", *n.ID)
		}
		if n.Editable {
			t.Errorf("сеть %s редактируема при ambiguous: правка активной сети рвёт ассоциацию", *n.ID)
		}
	}
	if seen < 2 {
		t.Fatalf("включённых секций %d, ambiguous начинается с двух", seen)
	}
}

// Неоднозначность снята — это про РЕЗУЛЬТАТ, а не про две отправленные
// команды: сузь цикл гашения, и проверка по журналу вызовов останется
// зелёной, а конфигурация останется неоднозначной.
func TestSwitchFromAmbiguousLeavesExactlyOneEnabled(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	makeAmbiguous(f)
	// ТРЕТЬЯ включённая секция — не украшение: с двумя (одна из которых
	// цель) гасить приходится ровно одну, и суженный цикл выглядел бы
	// точно так же, как правильный.
	appendWireless(f, `wireless.up_extra=wifi-iface
wireless.up_extra.device='radio0'
wireless.up_extra.mode='sta'
wireless.up_extra.network='wwan'
wireless.up_extra.ssid='Третья'
wireless.up_extra.encryption='none'
`)
	seesSSID(f, ssidSaved)

	if j := runSwitch(t, s, secSaved); j.State != job.Done {
		t.Fatalf("джоб %q, ошибка %v", j.State, j.Error)
	}

	rec := do(t, s, "GET", "/api/wifi/networks", true)
	var got struct {
		SelectionState string `json:"selection_state"`
		Networks       []struct {
			ID      *string `json:"id"`
			Enabled bool    `json:"enabled"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.SelectionState != "single" {
		t.Errorf("после переключения состояние %q, ожидалось single: неоднозначность не снята", got.SelectionState)
	}
	var enabled []string
	for _, n := range got.Networks {
		if n.Enabled && n.ID != nil {
			enabled = append(enabled, *n.ID)
		}
	}
	if len(enabled) != 1 || enabled[0] != secSaved {
		t.Errorf("включёнными остались %v, ожидалась одна %s", enabled, secSaved)
	}
}

// Подпись операции панель строит из kind и arg, и arg — ssid, а не имя
// секции: владелец выбирал сеть по имени в эфире. У режима и подписки это
// проверяется, у переключения не проверялось.
func TestSwitchJobDescribesItselfBySSID(t *testing.T) {
	fastUpstream(t)
	s, f := newServer(t)
	seesSSID(f, ssidSaved)

	j := runSwitch(t, s, secSaved)
	if j.Kind != "upstream" {
		t.Errorf("kind джоба %q, ожидался upstream", j.Kind)
	}
	if j.Arg != ssidSaved {
		t.Errorf("arg джоба %q, ожидался ssid %q — %q отправил бы владельца за смыслом в LuCI",
			j.Arg, ssidSaved, secSaved)
	}
}
