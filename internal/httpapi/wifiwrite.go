package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"netmoded/internal/uci"
	"netmoded/internal/wireless"
)

// NetworkWrite — тело POST /api/wifi/networks.
//
// Набор полей закрыт: всё, чего здесь нет, отвергается с 400, а не
// проглатывается (см. decodeStrict). Молчаливое игнорирование поля — худший
// из возможных ответов: клиент получает 200 и уверен, что его просьбу
// выполнили.
//
// Key — указатель намеренно. Отсутствие поля означает «пароль не трогай»,
// пустая строка — «запиши пустой». Слив их в одну строку, мы стирали бы
// пароль при каждой правке имени сети.
type NetworkWrite struct {
	ID         string  `json:"id"`
	SSID       string  `json:"ssid"`
	Encryption string  `json:"encryption"`
	Key        *string `json:"key"`
}

// Шифрования, которые демон умеет записывать.
//
// Список закрытый: записать `encryption` со значением, которого мы не
// понимаем, значит создать секцию, которая либо не поднимется, либо
// поднимется не так, как ждёт владелец.
var writableEncryption = map[string]bool{
	"psk2":      true,
	"psk":       true,
	"psk-mixed": true,
	"sae":       true,
	"sae-mixed": true,
	"none":      true,
}

// needsKey сообщает, обязателен ли пароль для этого шифрования.
func needsKey(enc string) bool { return enc != "none" }

func (s *Server) handleWifiWrite(w http.ResponseWriter, r *http.Request) {
	var in NetworkWrite
	if errResp := decodeStrict(r, &in); errResp != nil {
		errResp.send(w)
		return
	}

	g, errResp := s.openWriteForSaved(r)
	if errResp != nil {
		errResp.send(w)
		return
	}

	if in.ID == "" {
		s.createNetwork(w, r.Context(), g, in)
		return
	}
	s.editNetwork(w, r.Context(), g, in)
}

func (s *Server) handleWifiDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	g, errResp := s.openWriteForSaved(r)
	if errResp != nil {
		errResp.send(w)
		return
	}
	sec, errResp := g.targetEditable(id)
	if errResp != nil {
		errResp.send(w)
		return
	}

	ctx := r.Context()
	if err := s.ex.UCIDelete(ctx, "wireless", sec.Name, ""); err != nil {
		s.revertDraft(ctx, sec.Name)
		writeErr(w, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}
	s.commitAndRespond(w, ctx)
}

// ─────────── разбор тела ───────────

// decodeStrict разбирает тело запроса и отвергает поля, которых нет в схеме.
//
// Мотив — конкретный: клиент, приславший `"network": "lan"`, получал 200 и
// секцию на `wwan`. Просьбу не выполнили и об этом не сказали — худший
// возможный ответ, потому что он неотличим от выполненной. Реализовать же
// эту просьбу нельзя: выбор L3-сети станционной секции и есть смена
// upstream, отложенная до фазы 2 (ADR-0016).
//
// Проверка сделана общей, а не про одно поле: любое незнакомое поле означает,
// что клиент и демон расходятся в понимании контракта, и молчать об этом
// расхождении нельзя ни в одном из случаев.
func decodeStrict(r *http.Request, dst any) *httpErr {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if field, ok := unknownField(err); ok {
			return &httpErr{http.StatusBadRequest, "unsupported_field", unsupportedFieldMsg(field)}
		}
		return &httpErr{http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON"}
	}
	// Второе значение в теле — тот же класс ошибки: часть запроса была бы
	// прочитана и отброшена молча.
	if dec.More() {
		return &httpErr{http.StatusBadRequest, "bad_request",
			"В теле запроса больше одного JSON-значения"}
	}
	return nil
}

// unknownFieldPrefix — форма ошибки encoding/json при DisallowUnknownFields.
//
// Отдельного типа ошибки стандартная библиотека для этого случая не заводит,
// поэтому имя поля достаётся только из текста. Сверку держит тест
// TestUnknownFieldNameExtracted: если формулировка изменится в новой версии
// Go, упадёт он, а не ответ клиенту в проде.
const unknownFieldPrefix = `json: unknown field `

func unknownField(err error) (string, bool) {
	msg := err.Error()
	if !strings.HasPrefix(msg, unknownFieldPrefix) {
		return "", false
	}
	return strings.Trim(strings.TrimPrefix(msg, unknownFieldPrefix), `"`), true
}

// unsupportedFieldMsg объясняет отказ. Наружу идёт только ИМЯ поля: значения
// в текст ошибки не попадают никогда (ADR-0012).
func unsupportedFieldMsg(field string) string {
	if field == "network" {
		// Раньше здесь стояло «отложено до фазы 2». Фаза 2 наступила, и смена
		// upstream делается переключением сети, а не сменой L3-сети у секции:
		// секция всегда сидит на upstreamIf, меняется только то, какая из них
		// включена (ADR-0026).
		return "Поле \"network\" не принимается: станционная секция всегда живёт на " +
			upstreamIf + ". Сменить внешнюю сеть — это POST /api/upstream, " +
			"а не правка этого поля."
	}
	return fmt.Sprintf("Поле %q не входит в тело запроса. Демон не принимает полей, "+
		"которых не понимает: молча пропущенное поле выглядит как выполненная просьба.", field)
}

// ─────────── охрана записи ───────────

// writeGuard — состояние, проверенное перед записью.
//
// radios здесь не для удобства: разрешение делается один раз на запрос, и
// все шаги записи обязаны говорить об ОДНОМ радио. Разреши его повторно в
// resolve() — и правка могла бы уйти на другое радио, чем то, по которому
// считалась однозначность выбора.
//
// Наружу этот тип не выдаётся: получить его можно только внутри одного из
// именованных входов ниже.
type writeGuard struct {
	cfg    *uci.Config
	sel    wireless.Selection
	radios wireless.Radios
	// fp — отпечаток конфигурации, по которому гард выдан. Нужен джобу
	// переключения: между проверкой и записью проходят миллисекунды, но
	// именно в них чужой Save & Apply публикует свою работу, и сверить
	// отпечаток заново не с чем, если его не запомнить здесь.
	fp string
}

// savedGuard и switchGuard — два именованных входа записи (ADR-0026).
//
// Разные ТИПЫ, а не признак внутри одного: гард обязан помнить, каким входом
// он выдан, и «помнить» здесь означает, что перепутать невозможно. Метод
// targetSwitchable существует только у switchGuard, targetEditable — только
// у savedGuard; применить правила переключения к правке нельзя не потому,
// что кто-то за этим следит, а потому, что это не компилируется.
//
// Булев параметр openWrite(r, allowAmbiguous) отвергнут ADR-0026: в точке
// вызова видно `true`, а не смысл, и вопрос «кто разрешает запись при
// неоднозначности» перестаёт быть греповским. Гейт в каждом обработчике
// отвергнут там же: забытая проверка — это отсутствие строки, а отсутствия
// строки на ревью не видно. Новый обработчик записи физически не может
// «забыть» решить, чем он является: другого способа получить гард нет.
type savedGuard struct{ writeGuard }

type switchGuard struct{ writeGuard }

// httpErr — отложенная ошибка: проверки собирают её, обработчик отправляет.
type httpErr struct {
	status  int
	code    string
	message string
}

func (e *httpErr) send(w http.ResponseWriter) { writeErr(w, e.status, e.code, e.message) }

func conflict(code, msg string) *httpErr {
	return &httpErr{http.StatusConflict, code, msg}
}

// openWriteCommon выполняет проверки, общие для ЛЮБОЙ записи в wireless.
//
// Общая часть живёт в одной функции, а не копией в каждом входе: два входа
// вместо одного — это два места, где общая часть может разъехаться
// (ADR-0026, «Платим»), и единственная защита от этого — то, что второго
// места не существует.
//
// Порядок важен: сначала то, что делает запись бессмысленной (чужой
// стейджинг), потом устаревшее представление клиента. Реакция на
// неоднозначность здесь НЕ проверяется — это тот единственный шаг, которым
// входы расходятся, и он стоит в них самих.
func (s *Server) openWriteCommon(r *http.Request) (*writeGuard, *httpErr) {
	ctx := r.Context()

	// 1. Чужие незакоммиченные правки.
	//
	// `uci commit` публикует ВЕСЬ стейджинг пакета — своего и чужого не
	// различает. Закоммитив поверх чужого черновика, мы опубликовали бы
	// чужую работу под своим именем и в момент, который её автор не
	// выбирал. Отказ здесь — не перестраховка, а единственный честный
	// выход: своё из стейджинга не вынуть.
	changes, err := s.ex.UCIChanges(ctx, "wireless")
	if err != nil {
		return nil, &httpErr{http.StatusServiceUnavailable, "uci_unavailable", err.Error()}
	}
	if uci.HasStagedChanges(changes) {
		return nil, conflict("foreign_staged_changes",
			"В /etc/config/wireless есть незакоммиченные правки — вероятно, открыт LuCI. "+
				"Примените или отмените их, затем повторите.")
	}

	// 2. Текущее состояние.
	raw, err := s.ex.UCIShow(ctx, "wireless")
	if err != nil {
		return nil, &httpErr{http.StatusServiceUnavailable, "uci_unavailable", err.Error()}
	}
	cfg, err := uci.ParseShow("wireless", raw)
	if err != nil {
		return nil, &httpErr{http.StatusInternalServerError, "parse_failed", err.Error()}
	}

	// 3. Какое радио станционное. Раньше классификации: без ответа на этот
	//    вопрос неизвестно даже, какие секции наши, и инвариант фазы 1
	//    («трогаем только станционное радио», ADR-0009) проверять не по
	//    чему. Отказ, а не догадка (ADR-0019).
	radios := s.resolveRadios(ctx, nil, cfg)
	if errResp := requireStationRadio(radios); errResp != nil {
		return nil, errResp
	}
	sel := wireless.Classify(cfg, radios.Station)

	// 4. Отпечаток: клиент обязан доказать, что видел актуальное состояние.
	//
	// Без него панель, открытая полчаса назад, перезаписала бы правки,
	// сделанные в LuCI за это время, не заметив их.
	want := strings.TrimSpace(r.Header.Get("If-Match"))
	if want == "" {
		return nil, conflict("fingerprint_required",
			"Нужен заголовок If-Match с отпечатком из GET /api/wifi/networks")
	}
	fp := wireless.Fingerprint(raw)
	if fp != want {
		return nil, conflict("fingerprint_mismatch",
			"Конфигурация изменилась с момента чтения. Обновите список и повторите.")
	}

	return &writeGuard{cfg: cfg, sel: sel, radios: radios, fp: fp}, nil
}

// openWriteForSaved — вход для создания, правки и удаления сохранённых сетей.
//
// Неоднозначность запрещает любую такую запись — включая правку выключенной
// секции. Пока неизвестно, какую секцию поднимет netifd, доказать
// безвредность правки нельзя: секция, которую мы считаем спящей, может
// оказаться той, что уедет в живую систему при ближайшем применении, а
// применение теперь вызываем в том числе мы сами (ADR-0026, ADR-0010).
//
// Удаление при Ambiguous запрещено тем более: удалить одну из включённых
// секций — это и есть «решить за владельца, какая лишняя», то есть
// автопочинка, у которой всего лишь появилась кнопка.
func (s *Server) openWriteForSaved(r *http.Request) (*savedGuard, *httpErr) {
	g, errResp := s.openWriteCommon(r)
	if errResp != nil {
		return nil, errResp
	}
	if g.sel.State == wireless.Ambiguous {
		return nil, conflict("ambiguous_selection",
			"В конфигурации включено несколько станционных сетей. "+
				"Демон не выбирает за владельца: оставьте одну через LuCI или ssh.")
	}
	return &savedGuard{*g}, nil
}

// openWriteForSwitch — вход для смены внешней сети (POST /api/upstream).
//
// Ambiguous ПРОПУСКАЕТСЯ, и это не послабление, а определение операции
// (ADR-0026). Демон здесь ничего не решает: действие начинается с нажатия
// человека, секция названа по имени, результат задан нажатием целиком —
// включена ровно одна секция, та самая. Неоднозначность снимается не как
// побочный эффект, а как смысл операции; запретить её из-за неоднозначности
// значило бы отправить владельца в ssh мимо готовой кнопки.
func (s *Server) openWriteForSwitch(r *http.Request) (*switchGuard, *httpErr) {
	g, errResp := s.openWriteCommon(r)
	if errResp != nil {
		return nil, errResp
	}
	return &switchGuard{*g}, nil
}

// resolve отвечает на три вопроса, одинаковых для любой цели: годится ли
// форма идентификатора, есть ли такая секция и наша ли она.
//
// Что с ней МОЖНО сделать — вопрос четвёртый, и ответ на него у входов
// разный, поэтому он остался в targetEditable и targetSwitchable.
func (g *writeGuard) resolve(id string) (*uci.Section, *httpErr) {
	if id == "" {
		return nil, &httpErr{http.StatusBadRequest, "bad_request", "Не указан идентификатор сети"}
	}
	// Индексы наружу не выходят и внутрь не принимаются: они сдвигаются
	// при удалении в LuCI и указали бы на чужую сеть (ADR-0005).
	if strings.HasPrefix(id, "@") || strings.ContainsAny(id, "[]") {
		return nil, &httpErr{http.StatusBadRequest, "bad_request",
			"Идентификатор — имя секции, а не индекс: индексы сдвигаются при правках в LuCI"}
	}

	sec, ok := g.cfg.Section(id)
	if !ok {
		return nil, &httpErr{http.StatusNotFound, "not_found", "Сеть не найдена"}
	}
	// Чужая секция — не наша забота. Домашняя точка доступа сюда не попадёт:
	// радио сверяется с выведенным, а не с константой, иначе после
	// перестановки радио «чужой» оказалась бы как раз наша (ADR-0019).
	//
	// Это же единственная защита radio1 и wifi-device на пути записи
	// (ADR-0026, правило 4): не станционная секция не адресуется вовсе —
	// ни на правку, ни на выключение, ни на чтение ради записи.
	if sec.Type != "wifi-iface" ||
		sec.Options["device"] != g.radios.Station ||
		sec.Options["mode"] != "sta" {
		return nil, &httpErr{http.StatusNotFound, "not_found",
			"Секция не относится к внешним сетям роутера"}
	}
	return sec, nil
}

// targetEditable находит секцию для правки или удаления.
//
// Править и удалять включённую секцию нельзя — правило пережило фазу 1
// дословно и в фазе 2 стало весомее: смена ssid или пароля активной сети
// рвёт ассоциацию при ближайшем применении, а применение теперь вызываем в
// том числе мы (ADR-0026, «Что сохраняется из ADR-0009 дословно»).
func (g *savedGuard) targetEditable(id string) (*uci.Section, *httpErr) {
	sec, errResp := g.resolve(id)
	if errResp != nil {
		return nil, errResp
	}
	if sec.DisabledValid() && !sec.Disabled() {
		return nil, conflict("enabled_network_readonly",
			"Это активная внешняя сеть. Её правка порвала бы связь при ближайшем "+
				"применении конфигурации, поэтому в первой фазе она доступна только для чтения.")
	}
	return sec, nil
}

// targetSwitchable находит секцию, на которую переключаемся.
//
// Включённая секция здесь допустима — она и есть цель. Отказ ровно один:
// секция УЖЕ единственная включённая, то есть применять нечего, а применение
// «на всякий случай» — риск без цели (ADR-0025).
//
// Проверка сужена до Single намеренно. При Ambiguous целевая секция тоже
// может быть включена, но она не единственная — переключение на неё как раз
// и есть работа, которую надо сделать.
//
// 409, а не 200: панель считает switchable на сервере и такой кнопки не
// рисует, значит запрос означает устаревший список. 409 говорит панели
// перечитать — тем же приёмом, что fingerprint_mismatch. 200 без джоба
// потребовал бы отдельной формы ответа, которой больше нигде нет.
func (g *switchGuard) targetSwitchable(id string) (*uci.Section, *httpErr) {
	sec, errResp := g.resolve(id)
	if errResp != nil {
		return nil, errResp
	}
	if g.sel.State == wireless.Single && g.sel.Active == sec.Name {
		return nil, conflict("already_selected",
			"Эта сеть уже выбрана как единственная активная — переключать нечего. "+
				"Обновите список: он устарел.")
	}
	return sec, nil
}

// ─────────── операции ───────────

func (s *Server) createNetwork(w http.ResponseWriter, ctx context.Context, g *savedGuard, in NetworkWrite) {
	if err := validateNetwork(in, true); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	name, err := newSectionName()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Единственная точка отказа на всю запись — и это форма, а не вкус.
	// Отмена черновика обязана стоять на КАЖДОМ пути отказа, а забытый
	// revert в новой ветке выглядит как отсутствие строки и на ревью не
	// виден (ADR-0028, «Платим»). Одна ветка — одно место, где его можно
	// забыть, и оно здесь.
	if errResp := s.writeNewSection(ctx, g, name, in); errResp != nil {
		s.revertDraft(ctx, name)
		errResp.send(w)
		return
	}

	s.commitAndRespond(w, ctx)
}

// writeNewSection пишет секцию по частям. Порядок значим: disabled пишется
// ПОСЛЕДНИМ, и именно поэтому недописанная секция чаще всего не имеет его
// вовсе — то есть опубликованная чужим коммитом оказалась бы ВКЛЮЧЁННОЙ
// (ADR-0028, «Найденный дефект»). Отсюда обязательная отмена у вызывающего.
func (s *Server) writeNewSection(ctx context.Context, g *savedGuard, name string, in NetworkWrite) *httpErr {
	if err := s.ex.UCIAddNamed(ctx, "wireless", name, "wifi-iface"); err != nil {
		return &httpErr{http.StatusInternalServerError, "write_failed", err.Error()}
	}

	opts := [][2]string{
		{"device", g.radios.Station},
		{"mode", "sta"},
		{"network", upstreamIf},
		{"ssid", in.SSID},
		{"encryption", in.Encryption},
	}
	for _, kv := range opts {
		if err := s.ex.UCISet(ctx, "wireless", name, kv[0], kv[1]); err != nil {
			return &httpErr{http.StatusInternalServerError, "write_failed", err.Error()}
		}
	}
	if in.Key != nil {
		if err := s.ex.UCISet(ctx, "wireless", name, "key", *in.Key); err != nil {
			// Текст ошибки uci для key намеренно без значения (ADR-0012).
			return &httpErr{http.StatusInternalServerError, "write_failed", "Не удалось записать пароль"}
		}
	}

	// Одно из ДВУХ мест во всём демоне, где пишется disabled (второе —
	// путь переключения, upstreamhandler.go), и здесь только "1".
	//
	// Это не лазейка в инварианте, а его часть: отсутствие опции означает
	// ВКЛЮЧЕНА, поэтому создать секцию, не написав disabled, — значит
	// создать включённую станционную секцию, то есть ровно то, что
	// инвариант запрещает (ADR-0009, сохранено ADR-0026 правилом 5).
	if err := s.ex.UCISet(ctx, "wireless", name, "disabled", "1"); err != nil {
		return &httpErr{http.StatusInternalServerError, "write_failed", err.Error()}
	}
	return nil
}

// revertDraft отменяет незакоммиченный черновик ОДНОЙ секции на пути отказа.
//
// Без него первый же сбой uci запирал бы запись навсегда: наш недописанный
// черновик остаётся в стейджинге, шаг 1 openWriteCommon видит его и отвечает
// `409 foreign_staged_changes` с текстом про LuCI, которого нет, — и обойти
// эту проверку из панели нечем (ADR-0028).
//
// Отказ самой отмены ответа НЕ меняет: клиент уже получает 500 write_failed,
// и превращать неудачную уборку во второй код ошибки значило бы рассказывать
// владельцу про наш стейджинг вместо того, что случилось с его сетью.
// Строка в журнал — всё, что тут можно честно сделать.
func (s *Server) revertDraft(ctx context.Context, section string) {
	if err := s.ex.UCIRevert(ctx, "wireless", section); err != nil {
		s.logf("запись: не удалось отменить черновик секции %s — %v", section, err)
	}
}

func (s *Server) editNetwork(w http.ResponseWriter, ctx context.Context, g *savedGuard, in NetworkWrite) {
	sec, errResp := g.targetEditable(in.ID)
	if errResp != nil {
		errResp.send(w)
		return
	}
	if err := validateNetwork(in, false); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if errResp := s.writeSectionEdits(ctx, sec.Name, in); errResp != nil {
		// Половина правки в стейджинге запирает панель ровно так же, как
		// половина создания, и отменяется так же адресно (ADR-0028).
		s.revertDraft(ctx, sec.Name)
		errResp.send(w)
		return
	}

	s.commitAndRespond(w, ctx)
}

func (s *Server) writeSectionEdits(ctx context.Context, name string, in NetworkWrite) *httpErr {
	if in.SSID != "" {
		if err := s.ex.UCISet(ctx, "wireless", name, "ssid", in.SSID); err != nil {
			return &httpErr{http.StatusInternalServerError, "write_failed", err.Error()}
		}
	}
	if in.Encryption != "" {
		if err := s.ex.UCISet(ctx, "wireless", name, "encryption", in.Encryption); err != nil {
			return &httpErr{http.StatusInternalServerError, "write_failed", err.Error()}
		}
	}
	// Отсутствие key — «не трогай», а не «сотри».
	if in.Key != nil {
		if err := s.ex.UCISet(ctx, "wireless", name, "key", *in.Key); err != nil {
			return &httpErr{http.StatusInternalServerError, "write_failed", "Не удалось записать пароль"}
		}
	}
	return nil
}

// commitAndRespond публикует стейджинг и отдаёт обновлённый список.
//
// Отдаём список, а не пустой ответ: панели он всё равно нужен сразу, и
// второй запрос за ним — лишняя гонка с чужими правками.
func (s *Server) commitAndRespond(w http.ResponseWriter, ctx context.Context) {
	if err := s.ex.UCICommit(ctx, "wireless"); err != nil {
		// Сообщение uci не содержит значений опций, но подстраховываемся:
		// наружу идёт причина без контекста аргументов.
		writeErr(w, http.StatusInternalServerError, "commit_failed",
			"Не удалось применить изменения в /etc/config/wireless")
		return
	}
	s.respondNetworks(w, ctx, http.StatusOK)
}

// ─────────── проверка ввода ───────────

func validateNetwork(in NetworkWrite, creating bool) error {
	if creating || in.SSID != "" {
		if in.SSID == "" {
			return errors.New("не указано имя сети (ssid)")
		}
		// Предел стандарта — 32 байта, не символа.
		if len(in.SSID) > 32 {
			return fmt.Errorf("имя сети длиной %d байт, предел 32", len(in.SSID))
		}
		if !utf8.ValidString(in.SSID) {
			return errors.New("имя сети не в UTF-8")
		}
	}

	enc := in.Encryption
	if creating && enc == "" {
		return errors.New("не указано шифрование (encryption)")
	}
	if enc != "" && !writableEncryption[enc] {
		return fmt.Errorf("шифрование %q демон не записывает; допустимы: psk2, psk, psk-mixed, sae, sae-mixed, none", enc)
	}

	if creating && needsKey(enc) && in.Key == nil {
		return errors.New("для этого шифрования нужен пароль (key)")
	}
	if in.Key != nil && *in.Key != "" {
		// Предел WPA — 8..63 символа. Короткий пароль роутер примет в
		// конфиг и молча не поднимет сеть.
		if n := len(*in.Key); n < 8 || n > 63 {
			return fmt.Errorf("пароль длиной %d символов, допустимо 8–63", n)
		}
	}
	return nil
}

// newSectionName возвращает имя вида netmode_1a2b3c4d.
//
// Именованная, а не анонимная: анонимную нельзя адресовать стабильно
// (ADR-0005). Префикс netmode_ отличает наши секции в LuCI на глаз.
func newSectionName() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("генерация имени секции: %w", err)
	}
	return "netmode_" + hex.EncodeToString(b), nil
}

// respondNetworks — общий ответ для чтения и записи.
func (s *Server) respondNetworks(w http.ResponseWriter, ctx context.Context, code int) {
	raw, err := s.ex.UCIShow(ctx, "wireless")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "uci_unavailable", err.Error())
		return
	}
	cfg, err := uci.ParseShow("wireless", raw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "parse_failed", err.Error())
		return
	}
	// Радио разрешается заново: ответ идёт и после записи, и на голое
	// чтение, и врать в поле `radio` он не имеет права ни в одном из
	// случаев — панель показывает по нему, на каком диапазоне искать сеть.
	radios := s.resolveRadios(ctx, nil, cfg)
	if errResp := requireStationRadio(radios); errResp != nil {
		errResp.send(w)
		return
	}
	sel := wireless.Classify(cfg, radios.Station)
	fp := wireless.Fingerprint(raw)

	w.Header().Set("ETag", fp)
	writeJSON(w, code, map[string]any{
		"fingerprint":     fp,
		"selection_state": string(sel.State),
		"radio": map[string]string{
			"device": radios.Station,
			"band":   radios.StationBand,
			"mode":   "sta",
		},
		"networks": wireless.Networks(cfg, sel, radios.Station),
	})
}
