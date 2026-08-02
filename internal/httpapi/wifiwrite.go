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

	g, errResp := s.openWrite(r)
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

	g, errResp := s.openWrite(r)
	if errResp != nil {
		errResp.send(w)
		return
	}
	sec, errResp := g.target(id)
	if errResp != nil {
		errResp.send(w)
		return
	}

	ctx := r.Context()
	if err := s.ex.UCIDelete(ctx, "wireless", sec.Name, ""); err != nil {
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
		return "Поле \"network\" не принимается: выбор L3-сети станционной секции — " +
			"это смена upstream, отложенная до фазы 2 (ADR-0016). " +
			"Секция создаётся на " + upstreamIf + "."
	}
	return fmt.Sprintf("Поле %q не входит в тело запроса. Демон не принимает полей, "+
		"которых не понимает: молча пропущенное поле выглядит как выполненная просьба.", field)
}

// ─────────── охрана записи ───────────

// writeGuard — состояние, проверенное перед записью.
//
// radios здесь не для удобства: разрешение делается один раз на запрос, и
// все шаги записи обязаны говорить об ОДНОМ радио. Разреши его повторно в
// target() — и правка могла бы уйти на другое радио, чем то, по которому
// считалась однозначность выбора.
type writeGuard struct {
	cfg    *uci.Config
	sel    wireless.Selection
	radios wireless.Radios
}

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

// openWrite выполняет все проверки, общие для любой записи в wireless.
//
// Порядок важен: сначала то, что делает запись бессмысленной (чужой
// стейджинг), потом то, что делает её опасной (неоднозначность), потом
// устаревшее представление клиента.
func (s *Server) openWrite(r *http.Request) (*writeGuard, *httpErr) {
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
	if got := wireless.Fingerprint(raw); got != want {
		return nil, conflict("fingerprint_mismatch",
			"Конфигурация изменилась с момента чтения. Обновите список и повторите.")
	}

	// 5. Неоднозначность запрещает любую запись — включая правку
	//    выключенных секций. Пока неизвестно, какую секцию поднимет netifd,
	//    доказать безвредность правки нельзя (ADR-0010).
	if sel.State == wireless.Ambiguous {
		return nil, conflict("ambiguous_selection",
			"В конфигурации включено несколько станционных сетей. "+
				"Демон не выбирает за владельца: оставьте одну через LuCI или ssh.")
	}

	return &writeGuard{cfg: cfg, sel: sel, radios: radios}, nil
}

// target находит секцию для правки или удаления и проверяет, что её вообще
// можно трогать.
func (g *writeGuard) target(id string) (*uci.Section, *httpErr) {
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
	if sec.Type != "wifi-iface" ||
		sec.Options["device"] != g.radios.Station ||
		sec.Options["mode"] != "sta" {
		return nil, &httpErr{http.StatusNotFound, "not_found",
			"Секция не относится к внешним сетям роутера"}
	}
	// Инвариант фазы 1: включённую не трогаем (ADR-0009).
	if sec.DisabledValid() && !sec.Disabled() {
		return nil, conflict("enabled_network_readonly",
			"Это активная внешняя сеть. Её правка порвала бы связь при ближайшем "+
				"применении конфигурации, поэтому в первой фазе она доступна только для чтения.")
	}
	return sec, nil
}

// ─────────── операции ───────────

func (s *Server) createNetwork(w http.ResponseWriter, ctx context.Context, g *writeGuard, in NetworkWrite) {
	if err := validateNetwork(in, true); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	name, err := newSectionName()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	if err := s.ex.UCIAddNamed(ctx, "wireless", name, "wifi-iface"); err != nil {
		writeErr(w, http.StatusInternalServerError, "write_failed", err.Error())
		return
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
			writeErr(w, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}
	}
	if in.Key != nil {
		if err := s.ex.UCISet(ctx, "wireless", name, "key", *in.Key); err != nil {
			// Текст ошибки uci для key намеренно без значения (ADR-0012).
			writeErr(w, http.StatusInternalServerError, "write_failed", "Не удалось записать пароль")
			return
		}
	}

	// ЕДИНСТВЕННОЕ место во всём демоне, где пишется disabled, и только "1".
	//
	// Это не лазейка в инварианте, а его часть: отсутствие опции означает
	// ВКЛЮЧЕНА, поэтому создать секцию, не написав disabled, — значит
	// создать включённую станционную секцию, то есть ровно то, что
	// инвариант запрещает (ADR-0009).
	if err := s.ex.UCISet(ctx, "wireless", name, "disabled", "1"); err != nil {
		writeErr(w, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}

	s.commitAndRespond(w, ctx)
}

func (s *Server) editNetwork(w http.ResponseWriter, ctx context.Context, g *writeGuard, in NetworkWrite) {
	sec, errResp := g.target(in.ID)
	if errResp != nil {
		errResp.send(w)
		return
	}
	if err := validateNetwork(in, false); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if in.SSID != "" {
		if err := s.ex.UCISet(ctx, "wireless", sec.Name, "ssid", in.SSID); err != nil {
			writeErr(w, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}
	}
	if in.Encryption != "" {
		if err := s.ex.UCISet(ctx, "wireless", sec.Name, "encryption", in.Encryption); err != nil {
			writeErr(w, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}
	}
	// Отсутствие key — «не трогай», а не «сотри».
	if in.Key != nil {
		if err := s.ex.UCISet(ctx, "wireless", sec.Name, "key", *in.Key); err != nil {
			writeErr(w, http.StatusInternalServerError, "write_failed", "Не удалось записать пароль")
			return
		}
	}

	s.commitAndRespond(w, ctx)
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
