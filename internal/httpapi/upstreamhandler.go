package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"netmoded/internal/executor"
	"netmoded/internal/job"
	"netmoded/internal/netif"
	"netmoded/internal/uci"
	"netmoded/internal/wireless"
)

// Таксономия неудач переключения — РОВНО эти восемь строк.
//
// Список закрыт и менять его нельзя в одиночку: те же строки переводит
// панель (web/i18n.js) и описывает docs/api/openapi.yaml. Новая причина,
// добавленная только здесь, приедет к владельцу непереведённым машинным
// кодом — то есть худшим видом доклада: похожим на ошибку демона.
//
// Границу между двумя самыми близкими стоит назвать явно, иначе её проведут
// заново и по-другому:
//
//   - apply_failed — переключение НЕ состоялось по нашей стороне: запись не
//     удалась, отпечаток уехал, коммит не прошёл, скрипт отказал. Про живую
//     систему при этом известно всё, что нужно: она осталась прежней;
//   - unverifiable — применение состоялось, а прочитать исход не удалось:
//     ubus или iwinfo молчали всё окно ожидания. Мы не знаем, переключилось
//     ли, и говорим именно это, а не «не переключилось».
//
// Слить их значило бы объявить неудачей то, что могло удаться, — и владелец
// пошёл бы чинить работающую сеть.
const (
	reasonApplyFailed      = "apply_failed"
	reasonBusy             = "busy"
	reasonPrereqMissing    = "prereq_missing"
	reasonStayedOnPrevious = "stayed_on_previous"
	reasonOtherSSID        = "other_ssid"
	reasonNotAssociated    = "not_associated"
	reasonNoIPv4           = "no_ipv4"
	reasonUnverifiable     = "unverifiable"
)

// Пороги ожидания — ПЕРЕМЕННЫЕ пакета, а не константы.
//
// Прецедент и довод те же, что у job.Manager.timeout: проверить вердикт на
// истёкшем окне иначе значило бы держать тест двадцать секунд на каждую из
// восьми причин, то есть не проверять их вовсе. Тест, который слишком долго
// идёт, однажды помечают //nolint или удаляют, и это худший исход, чем
// изменяемая переменная.
//
// Значения выведены из замера с кратным запасом (ADR-0025): ассоциация
// измерена в 2–4 с, IPv4 — в 5–13 с. Ждать ровно измеренное нельзя — замер
// сделан на одной сети с одним уровнем сигнала, — поэтому окно втрое шире, а
// истечение окна докладывается как «не ассоциировалась», а не как ошибка
// демона.
//
// Отсчёт upstreamIPv4Timeout начинается ПОСЛЕ ассоциации, а не от начала
// операции: аренда DHCP запрашивается только после того, как станция
// подключилась, и общий бюджет означал бы, что медленная ассоциация съедает
// время у DHCP и сеть объявляется без адреса за секунду до его получения.
var (
	upstreamAssocTimeout = 20 * time.Second
	upstreamIPv4Timeout  = 30 * time.Second
	// upstreamPollInterval — шаг опроса. Полсекунды: ассоциация занимает
	// секунды, и опрашивать чаще значит впустую запускать ubus на роутере
	// с 512 МБ, а реже — врать владельцу о длительности.
	upstreamPollInterval = 500 * time.Millisecond
	// upstreamETASec — что показать в панели как ожидаемую длительность.
	// Ассоциация плюс адрес по замеру укладываются в 4–13 с; двадцать —
	// честная верхняя оценка, а не среднее: полоса, добежавшая до конца
	// раньше события, пугает сильнее медленной.
	upstreamETASec = 20
)

// UpstreamSwitch — тело POST /api/upstream.
//
// Одно поле, и набор закрыт (decodeStrict): всё прочее — 400
// unsupported_field. Форма зафиксирована ещё ADR-0016 и не менялась.
//
// Ни ssid, ни пароля здесь нет и быть не может: переключение адресует
// СОХРАНЁННУЮ секцию по имени (ADR-0005) и не является правкой — оно меняет
// одну опцию disabled и не трогает ни ssid, ни key, ни encryption
// (ADR-0026).
type UpstreamSwitch struct {
	ID string `json:"id"`
}

// handleUpstream переключает внешнюю сеть.
//
// Вся синхронная часть отказывает ДО 202 и, что важнее, до первой записи в
// UCI: это единственная точка, где мы ещё ничего не сломали. После
// uci commit отказаться нечем — отменить коммит нельзя (ADR-0006), а
// применение станет неизбежным при ближайшем чужом network reload
// (ADR-0025, раздел 4).
//
// Порядок проверок обязателен, и последняя из них — взятие джоба. Джоб
// берётся ПЕРЕД записью не для красоты: возьми его после, и job_busy
// оставил бы опубликованное намерение, которое никто не применяет —
// конфигурация говорит одно, живая система другое, и разошлись они по нашей
// вине. Поэтому весь путь записи целиком лежит внутри тела джоба.
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	var in UpstreamSwitch
	if errResp := decodeStrict(r, &in); errResp != nil {
		errResp.send(w)
		return
	}

	// Именованный вход записи: Ambiguous здесь ПРОПУСКАЕТСЯ (ADR-0026).
	g, errResp := s.openWriteForSwitch(r)
	if errResp != nil {
		errResp.send(w)
		return
	}
	sec, errResp := g.targetSwitchable(in.ID)
	if errResp != nil {
		errResp.send(w)
		return
	}
	if errResp := validateSwitchTarget(sec); errResp != nil {
		errResp.send(w)
		return
	}

	p := switchPlan{
		radio:   g.radios.Station,
		section: sec.Name,
		ssid:    sec.Options["ssid"],
		fp:      g.fp,
	}

	// Label остаётся русским: он читается в syslog и в диагностике по ssh.
	// Панель его не показывает — она строит подпись из kind и arg сама.
	//
	// arg — SSID, а не имя секции: панель показывает подпись владельцу, а
	// он выбирал сеть по имени в эфире. `wifinet2` в подписи означало бы,
	// что за смыслом операции надо идти в LuCI.
	j, err := s.jobs.Start("upstream", p.ssid, "Переключение внешней сети на "+p.ssid,
		upstreamETASec, func(ctx context.Context) error {
			return s.switchUpstream(ctx, p)
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

// validateSwitchTarget отказывает по сохранённой секции, которой нельзя
// пользоваться, — до записи и до применения.
//
// Все три проверки об одном: сеть, записанная не полностью, поднимется не
// как сеть. Роутер примет такую конфигурацию молча, станция не
// ассоциируется, и владелец получит `not_associated` — диагноз, который
// уводит искать проблему в эфире, хотя она в файле.
//
// Значение key в текст ответа не попадает ни в одном случае (ADR-0012):
// наружу идёт факт «пароль не сохранён», а не то, что сохранено.
func validateSwitchTarget(sec *uci.Section) *httpErr {
	if sec.Options["ssid"] == "" {
		return conflict("network_incomplete",
			"У этой сети не сохранено имя (ssid) — подключаться не к чему. "+
				"Задайте имя в LuCI или создайте сеть заново.")
	}
	// needsKey — та же функция, которой пользуется проверка ввода при
	// создании: два определения «нужен ли пароль» разъехались бы, и панель
	// разрешала бы создать сеть, на которую потом нельзя переключиться.
	if needsKey(sec.Options["encryption"]) && sec.Options["key"] == "" {
		return conflict("network_incomplete",
			"У этой сети задано шифрование, но не сохранён пароль. "+
				"Станция не подключится: задайте пароль и повторите.")
	}
	// Секция без network=wwan подняла бы станцию в другой L3-сети: адрес и
	// маршрут по умолчанию уехали бы не туда, а внешним каналом роутера
	// считается именно wwan (raw/11).
	if sec.Options["network"] != upstreamIf {
		return conflict("network_incomplete",
			"Эта сеть привязана не к внешнему каналу роутера ("+upstreamIf+"). "+
				"Переключение на неё не дало бы интернета.")
	}
	return nil
}

// switchPlan — всё, что джобу нужно знать, решённое до его старта.
//
// Значения, а не указатель на writeGuard: гард живёт запросом, а джоб — нет,
// и держать за него после ответа значило бы читать состояние, проверенное
// для другого момента времени. Отпечаток здесь именно затем, чтобы этот
// момент можно было сверить заново.
type switchPlan struct {
	radio   string // станционное радио, выведенное из системы (ADR-0019)
	section string // имя целевой секции — из разбора uci show, не литерал
	ssid    string
	fp      string
}

// switchUpstream — тело джоба. Порядок шагов обязателен, каждый объяснён.
//
// СВЕТОДИОД ЗДЕСЬ НЕ ТРОГАЕТСЯ, И ЭТО НЕ ЗАБЫВЧИВОСТЬ. Индикация показывает
// РЕЖИМ обработки трафика (nikki / b4 / off, led.StateForMode), а режим при
// смене внешней сети не меняется ни на секунду. Мигание «применяю» здесь
// сообщило бы владельцу, что происходит смена режима, а зажечь по итогам
// «ошибку» значило бы показать сломанным то, что работает: обход остался тем
// же, каким был. Дописывать сюда индикацию «для симметрии с applyMode» —
// значит сделать светодиод неоднозначным, а он единственный доклад, видимый
// без панели.
func (s *Server) switchUpstream(ctx context.Context, p switchPlan) error {
	// 1. Прежний SSID — ДО записи и до применения.
	//
	// Только здесь его ещё можно узнать. Прочитанный после применения, он
	// уже был бы результатом, а не исходной точкой, и «осталась на прежней
	// сети» стало бы неотличимо от «ушла на третью»: обе выглядят как
	// «ассоциирована не с тем, что просили». Это две разные новости —
	// первая означает, что переключение не произошло вовсе, вторая — что
	// произошло не туда.
	prev, _ := s.stationSSID(ctx, p.radio)

	// 2. Повторная сверка отпечатка.
	//
	// Окно между проверкой в обработчике и записью — миллисекунды, но
	// именно в нём чужой Save & Apply публикует свою работу. Пропустив
	// сверку, мы записали бы disabled в конфигурацию, которой владелец не
	// видел, и погасили бы секции, которых не было в его списке.
	raw, err := s.ex.UCIShow(ctx, "wireless")
	if err != nil {
		return s.switchFailed(p.ssid, reasonApplyFailed,
			fmt.Errorf("конфигурация не прочитана, ничего не изменено: %w", err))
	}
	if wireless.Fingerprint(raw) != p.fp {
		return s.switchFailed(p.ssid, reasonApplyFailed, errors.New(
			"конфигурация wireless изменилась между проверкой и записью — "+
				"ничего не изменено, обновите список и повторите"))
	}
	cfg, err := uci.ParseShow("wireless", raw)
	if err != nil {
		return s.switchFailed(p.ssid, reasonApplyFailed,
			fmt.Errorf("конфигурация не разбирается, ничего не изменено: %w", err))
	}

	// 3–4. Одна партия записей: сначала погасить прочие, затем включить цель.
	written, err := s.writeSwitch(ctx, cfg, p)
	if err != nil {
		// 5. Отказ до коммита: отменяем СВОЙ черновик и выходим.
		//
		// Без этого недописанная партия осталась бы в стейджинге, и
		// следующий запрос упёрся бы в foreign_staged_changes с текстом
		// про LuCI, которого нет, — навсегда, до uci revert по ssh
		// (ADR-0028). Хуже того: чужой uci commit опубликовал бы половину
		// нашей партии, то есть возможную конфигурацию с двумя
		// включёнными секциями или с нулём.
		s.revertSwitch(ctx, written)
		return s.switchFailed(p.ssid, reasonApplyFailed, err)
	}

	// 6. ОДИН коммит на всю партию.
	//
	// Промежуточные коммиты означали бы, что состояние «все выключены» или
	// «включены две» существует в опубликованной конфигурации хотя бы
	// мгновение — и переживает падение демона ровно в это мгновение.
	if err := s.ex.UCICommit(ctx, "wireless"); err != nil {
		// Revert сюда не дописывается: после попытки коммита неизвестно,
		// опубликовалась ли часть, и отмена стала бы возвратом
		// опубликованного, то есть откатом (ADR-0006, ADR-0028).
		return s.switchFailed(p.ssid, reasonApplyFailed,
			errors.New("не удалось применить изменения в /etc/config/wireless"))
	}

	// 7. Применение узким глаголом. Тупой `wifi` целиком запрещён навсегда:
	//    он измеренно роняет домашнюю точку (ADR-0025, отрицательный
	//    контроль). Скрипт делает `network reload` сам — поэтому объекта
	//    `network` нет в allowedUbusObjects и добавлять его сюда нельзя:
	//    это выдало бы демону универсальный `network restart` отовсюду.
	if err := s.ex.ApplyUpstream(ctx, executor.UpstreamTarget{Radio: p.radio}); err != nil {
		return s.switchFailed(p.ssid, applyReason(err), err)
	}

	// 8. Вердикт — по ассоциации, НИКОГДА по коду возврата.
	//
	// nil от ApplyUpstream означает ровно «глагол принят»: измерено, что
	// rc=0 приходит и когда не сделано ничего, и когда станция вернулась на
	// старую сеть (ADR-0025, «мина»). Судить по коду здесь — это в точности
	// тот класс ошибок, ради закрытия которого написан весь ADR.
	return s.verifySwitch(ctx, p, prev)
}

// writeSwitch пишет партию: все прочие станционные — «выключено», целевая —
// «включено». Возвращает секции, которых партия успела коснуться, — их и
// только их отменяет revert.
//
// Порядок «сначала погасить, потом включить» обязателен. В обратном порядке
// в стейджинге на несколько команд существует конфигурация с двумя
// включёнными секциями, и чужой uci commit ровно в этот момент опубликовал
// бы неоднозначность нашими руками.
func (s *Server) writeSwitch(ctx context.Context, cfg *uci.Config, p switchPlan) ([]string, error) {
	var written []string

	for _, sec := range cfg.Sections {
		if sec.Name == p.section || !isStationSection(sec, p.radio) {
			continue
		}
		// Уже выключенную не трогаем: лишняя запись — лишняя строка в
		// стейджинге и лишний шанс отказать на полпути.
		//
		// А вот секцию с НЕРАСПОЗНАННЫМ значением disabled гасим наравне с
		// включённой, и это не перестраховка. Нераспознанное значение —
		// вторая причина Ambiguous (wireless.Classify), и оставить его
		// значит выйти из джоба в ту же неоднозначность, ради выхода из
		// которой переключение из Ambiguous вообще разрешено (ADR-0026).
		// Как netifd прочтёт `disabled='maybe'`, мы не знаем — значит
		// обязаны считать, что как «включено».
		if sec.DisabledValid() && sec.Disabled() {
			continue
		}
		written = append(written, sec.Name)
		if err := s.ex.UCISet(ctx, "wireless", sec.Name, "disabled", "1"); err != nil {
			return written, fmt.Errorf("не удалось выключить прочие сети: %w", err)
		}
	}

	// Включение цели — записью литерала "0", а НЕ удалением опции.
	// Отсутствие disabled означает «включено» (ADR-0004), поэтому удаление
	// включило бы сеть способом, который в диффе не выглядит как включение.
	// Разрешённого случая для такого удаления не существует ни в одной фазе
	// (ADR-0026, правило 2) — потому имени соответствующего метода нет и в
	// этой строке: греп-гейт не обязан отличать комментарий от кода.
	written = append(written, p.section)
	if err := s.ex.UCISet(ctx, "wireless", p.section, "disabled", "0"); err != nil {
		return written, fmt.Errorf("не удалось включить выбранную сеть: %w", err)
	}
	return written, nil
}

// revertSwitch отменяет наш черновик адресно, посекционно.
//
// По секциям, а не по пакету: `uci revert wireless` снёс бы и чужую правку,
// попавшую в стейджинг в окне между проверкой шага 1 openWriteCommon и
// отказом. Наши адреса известны точно — мы их только что записали
// (ADR-0028).
//
// Отказ самой отмены ответа не меняет и второй попытки не вызывает: клиент
// уже получил 202, джоб всё равно завершится неудачей, а повторный revert
// по тому же адресу — это ровно та инициатива демона, которой здесь быть не
// должно. Строка в журнал — всё, что тут можно честно сделать.
func (s *Server) revertSwitch(ctx context.Context, sections []string) {
	for _, name := range sections {
		if err := s.ex.UCIRevert(ctx, "wireless", name); err != nil {
			s.logf("переключение: не удалось отменить черновик секции %s — %v", name, err)
		}
	}
}

// verifySwitch ждёт ассоциации, затем адреса, и выносит вердикт.
func (s *Server) verifySwitch(ctx context.Context, p switchPlan, prev string) error {
	if reason := s.awaitAssociation(ctx, p, prev); reason != "" {
		return s.switchFailed(p.ssid, reason, associationError(reason, p.ssid, prev))
	}
	if reason := s.awaitIPv4(ctx); reason != "" {
		// Текст обязан следовать за причиной: awaitIPv4 возвращает не
		// только no_ipv4, но и unverifiable — если состояние wwan
		// прочитать не удалось ни разу. Одна формулировка на оба случая
		// утверждала бы «адреса нет» там, где мы просто не спросили.
		detail := errors.New("станция подключилась к «" + p.ssid + "», но внешний канал так и " +
			"не получил адрес IPv4 — сеть подключена, интернета нет")
		if reason == reasonUnverifiable {
			detail = errors.New("станция подключилась к «" + p.ssid + "», но состояние внешнего " +
				"канала прочитать не удалось — есть ли интернет, неизвестно")
		}
		return s.switchFailed(p.ssid, reason, detail)
	}

	// Успех стирает прошлую неудачу: доклад о том, чего уже нет, заставит
	// владельца чинить починенное.
	s.fails.Clear()
	return nil
}

// awaitAssociation ждёт, пока станция подключится к запрошенной сети.
//
// Пустая строка — успех; иначе причина из таксономии. Вердикт выносится
// ПОСЛЕ истечения окна, а не при первом же несовпадении: сразу после
// применения станция несколько секунд остаётся на прежней сети, и ранний
// приговор объявлял бы неудачей нормальный ход переключения.
func (s *Server) awaitAssociation(ctx context.Context, p switchPlan, prev string) string {
	deadline := time.Now().Add(upstreamAssocTimeout)
	var last string
	var everRead bool

	for {
		ssid, ok := s.stationSSID(ctx, p.radio)
		if ok {
			everRead, last = true, ssid
			if ssid == p.ssid {
				return ""
			}
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, upstreamPollInterval) {
			break
		}
	}

	switch {
	case !everRead:
		// Ни одного успешного чтения за всё окно: про исход нам неизвестно
		// ничего. Сказать «не переключилось» было бы выдумкой.
		return reasonUnverifiable
	case last == "":
		return reasonNotAssociated
	case prev != "" && last == prev:
		// Самый важный из исходов: глагол вернул 0, а станция осталась там
		// же. Ровно это измерено в ADR-0025 и ровно поэтому вердикт не
		// выносится по коду возврата.
		return reasonStayedOnPrevious
	default:
		return reasonOtherSSID
	}
}

// awaitIPv4 ждёт аренду DHCP на внешнем канале.
//
// Отсчёт начинается здесь, то есть уже ПОСЛЕ ассоциации: см. комментарий к
// upstreamIPv4Timeout.
//
// Отсутствие адреса — неудача, а не предупреждение: сеть без адреса и
// маршрута по умолчанию интернета не даёт, а панель, показавшая успех, увела
// бы владельца искать причину в движках обхода.
func (s *Server) awaitIPv4(ctx context.Context) string {
	deadline := time.Now().Add(upstreamIPv4Timeout)
	var everRead bool

	for {
		b, err := s.ex.UbusCall(ctx, "network.interface."+upstreamIf, "status", nil)
		if err == nil {
			if st, perr := netif.ParseStatus(b); perr == nil {
				everRead = true
				if st.Online() {
					return ""
				}
			}
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, upstreamPollInterval) {
			break
		}
	}

	if !everRead {
		return reasonUnverifiable
	}
	return reasonNoIPv4
}

// stationSSID читает, к какой сети подключена станция ПРЯМО СЕЙЧАС.
//
// Источник — живая система (ADR-0002): network.wireless status даёт имя
// станционного интерфейса, iwinfo info — ассоциацию. Имя интерфейса
// выводится, а не берётся литералом: оно меняется с конфигурацией радио и на
// другом железе выглядит иначе (ADR-0019).
//
// Второе значение отделяет «прочитали: не подключена» от «прочитать не
// смогли». Без него молчащий ubus выглядел бы как отсутствие ассоциации, и
// unverifiable превратился бы в not_associated — то есть в уверенное
// утверждение о том, чего мы не знаем.
//
// Отсутствие станционного интерфейса в ответе ubus — это ЧТЕНИЕ, а не сбой:
// сразу после reconf интерфейс на секунды исчезает из статуса, и считать это
// неудачей чтения значило бы объявлять unverifiable ровно в тот момент,
// когда всё идёт по плану.
func (s *Server) stationSSID(ctx context.Context, radio string) (string, bool) {
	b, err := s.ex.UbusCall(ctx, "network.wireless", "status", nil)
	if err != nil {
		return "", false
	}
	st, err := wireless.ParseStatus(b)
	if err != nil {
		return "", false
	}
	ifname := wireless.IfnameForMode(st, radio, "sta")
	if ifname == "" {
		return "", true
	}
	ib, err := s.ex.UbusCall(ctx, "iwinfo", "info", map[string]any{"device": ifname})
	if err != nil {
		return "", false
	}
	info, err := wireless.ParseInfo(ib)
	if err != nil {
		return "", false
	}
	if !info.Associated {
		return "", true
	}
	return info.SSID, true
}

// applyReason переводит исход netmode-wifi в причину для доклада.
//
// Три класса различаются потому, что различается ответ владельцу: «занято»
// значит повторить через секунды, «нет предусловия» — доставить пакет на
// роутер, «применить не удалось» — что записанное намерение лежит в файле и
// уедет в живую систему при ближайшем чужом применении (ADR-0027).
//
// Неизвестный код и незапустившийся скрипт попадают в apply_failed: это
// самый безопасный из трёх диагнозов — он не обещает ни того, что повтор
// поможет, ни того, что система не тронута.
func applyReason(err error) string {
	switch {
	case errors.Is(err, executor.ErrUpstreamBusy):
		return reasonBusy
	case errors.Is(err, executor.ErrUpstreamPrereq):
		return reasonPrereqMissing
	default:
		return reasonApplyFailed
	}
}

// associationError — текст для job.error по причине.
//
// Отдельно от кода намеренно: reason читает панель, текст читает человек в
// логе и в поле error. Оба нужны, и оба обязаны говорить одно и то же.
func associationError(reason, want, prev string) error {
	switch reason {
	case reasonStayedOnPrevious:
		return errors.New("станция осталась на прежней сети «" + prev + "» — " +
			"переключение на «" + want + "» не состоялось, хотя применение прошло без ошибки")
	case reasonOtherSSID:
		return errors.New("станция подключилась не к той сети: просили «" + want + "»")
	case reasonUnverifiable:
		return errors.New("применение выполнено, но состояние станции прочитать не удалось — " +
			"переключилась ли она, неизвестно; сверьтесь со статусом")
	default:
		return errors.New("станция не подключилась к «" + want + "» за отведённое время")
	}
}

// switchFailed записывает доклад и возвращает ошибку джобу.
//
// Две дороги у одного факта, и обе нужны: job.error виден, пока открыта
// вкладка, last_fail — когда владелец вернулся через минуту и джоба в
// статусе уже нет (ADR-0025, «Честный доклад»).
func (s *Server) switchFailed(ssid, reason string, err error) error {
	if s.fails != nil {
		s.fails.Set(ssid, reason, time.Now())
	}
	s.logf("переключение на «%s» не удалось (%s): %v", ssid, reason, err)
	return err
}

// isStationSection — наша ли это секция: станция на станционном радио.
//
// Единственная защита домашней точки на пути записи (ADR-0026, правило 4).
// Радио сверяется с ВЫВЕДЕННЫМ, а не с литералом: после перепрошивки radio0
// и radio1 меняются местами, и литерал молча погасил бы домашнюю сеть
// (ADR-0019).
func isStationSection(s uci.Section, radio string) bool {
	return s.Type == "wifi-iface" &&
		s.Options["device"] == radio &&
		s.Options["mode"] == "sta"
}

// sleepCtx спит шаг опроса. false означает «контекст отменён, дальше не
// ждём»: джоб обязан заканчиваться вместе со своим сроком, а не досиживать
// окно ожидания в уже мёртвом контексте.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
