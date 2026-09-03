package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"netmoded/internal/happ"
	"netmoded/internal/nikki"
	"netmoded/internal/subs"
)

// ProxyGroup — группа, из которой панель выбирает узел.
//
// Имя фиксировано профилем mihomo (SPEC §2), а не настраивается: сделать
// его опцией значило бы дать способ выбрать чужую группу и сломать
// fail-closed, не заметив этого.
const ProxyGroup = "PROXY"

func (s *Server) handleNikkiProxies(w http.ResponseWriter, r *http.Request) {
	s.writeProxies(w, r, nil)
}

// writeProxies отдаёт тело ProxiesResponse, добавив к нему extra.
//
// Вынесено из обработчика, потому что тело описано в контракте один раз, а
// отдают его три пути (список, выбор узла, замер). Собирать его на каждом
// заново значило бы завести три копии, расходящиеся по одной за правку.
func (s *Server) writeProxies(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}

	all, err := s.nikki.Proxies(r.Context())
	if err != nil {
		if errors.Is(err, nikki.ErrUnavailable) {
			writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable",
				"Clash API не отвечает. Узлы недоступны, режим переключается по-прежнему.")
			return
		}
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}

	g, ok := all[ProxyGroup]
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "group_missing",
			"В профиле mihomo нет группы "+ProxyGroup)
		return
	}

	ver := ""
	if v, err := s.nikki.Version(r.Context()); err == nil {
		ver = v
	}

	body := map[string]any{
		"available": true,
		"version":   ver,
		"group":     ProxyGroup,
		"type":      g.Type,
		"selected":  g.Now,
		// Закреплён вручную или выбран движком — по одному selected это
		// неразличимо, а для панели разница принципиальна: закрепление
		// временное, и возврат к AUTO надо держать на виду.
		"fixed":      g.Fixed,
		"pinned":     g.Pinned,
		"selectable": g.Selectable,
		// Порядок и виды строк — из манифеста подписки, живость и задержка
		// — из движка (subs.Order). Ни один источник не главнее: mihomo
		// отдаёт узлы объектом и авторский порядок провайдера теряет, а
		// манифест не знает, отвечает ли узел прямо сейчас.
		//
		// Манифеста нет (свежая установка, ни одного обновления) — Order
		// отдаёт ровно живой список mihomo, как было до подписки. Это не
		// запасной путь, а нормальное состояние, и оно обязано работать.
		"members": subs.Order(all, ProxyGroup, s.manifestEntries()),
	}
	// extra не может затереть поля тела: ключи контракта раскладываются
	// последними. Иначе добавка «сверху» однажды подменила бы members и
	// расхождение с openapi не поймал бы ни один гвард.
	for k, v := range extra {
		if _, busy := body[k]; !busy {
			body[k] = v
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// handleNikkiTest — замер задержки всех участников группы.
//
// Это ТРИГГЕР свежей пробы, а не источник чисел (RQ-06): задержки приходят в
// GET /proxies и без нажатия — mihomo обновляет history сам примерно раз в
// пять минут. Кнопка нужна, когда ждать этих пяти минут нельзя: подписка
// только что обновилась или узел, похоже, лёг.
//
// Синхронный ответ, без джоба: операция ничего не меняет ни в UCI, ни в
// профиле, повтор безвреден, и единственная её цена — время (probeBudget).
// Джоб дал бы владельцу полоску вместо ответа и второй путь опроса ради
// операции, которая всё равно укладывается в один запрос.
//
// Двойное нажатие сервер не блокирует: панель держит свой busy, а два
// параллельных замера дадут лишний трафик и одинаковый результат — блокировка
// на демоне стоила бы разделяемого состояния ради этого.
func (s *Server) handleNikkiTest(w http.ResponseWriter, r *http.Request) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}

	all, err := s.nikki.Proxies(r.Context())
	if err != nil {
		if errors.Is(err, nikki.ErrUnavailable) {
			writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable",
				"Clash API не отвечает. Замерять нечего.")
			return
		}
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}
	if _, ok := all[ProxyGroup]; !ok {
		writeErr(w, http.StatusServiceUnavailable, "group_missing",
			"В профиле mihomo нет группы "+ProxyGroup)
		return
	}

	// Замеряются ТОЛЬКО строки вида node.
	//
	// Разделитель («⬇️ Обходы белых списков ⬇️»), «Авто» и непереводимая
	// запись узлами не являются: пробы по такому имени движок не сделает,
	// ответит «не найдено», и результат осел бы в failed. То есть кнопка
	// честно доложила бы о мёртвом узле там, где узла никогда и не было, —
	// а владелец пошёл бы искать поломку в исправной подписке.
	//
	// Тот же фильтр закрывает и узел, записанный в файл провайдера, но
	// движком не показанный: Order помечает его unsupported, и пробовать
	// его нечем — в mihomo такого имени нет.
	//
	// Без манифеста все строки — node, и замер остаётся ровно тем, чем был.
	members := subs.Order(all, ProxyGroup, s.manifestEntries())
	names := make([]string, 0, len(members))
	for _, m := range members {
		if m.Kind != happ.KindNode {
			continue
		}
		names = append(names, m.Name)
	}
	// Саму группу не проверяем: /proxies/{группа}/delay отдаёт задержку
	// выбранного члена, а он и так есть в списке — вышла бы двойная проба
	// одного узла под двумя именами.

	sum := nikki.ProbeAll(r.Context(), s.nikki, names)
	s.logf("nikki: замер %d узлов — измерено %d, не ответили %d, не успели %d, %d мс",
		sum.Total, sum.Measured, sum.Failed, sum.Skipped, sum.ElapsedMS)

	// Ответ 200 даже когда не ответил никто, а исход — в теле.
	//
	// Кодом ошибки это было бы неправдой: движок отработал, узлы опрошены,
	// результат «все мертвы» — такой же результат, как «все живы», и панели
	// он нужен вместе со свежим списком. Молчать о нём тоже нельзя: числа в
	// списке останутся прежними, и нажатие будет неотличимо от бездействия.
	// Поэтому итог едет полем test, а разговаривает с владельцем панель.
	s.writeProxies(w, r, map[string]any{"test": sum})
}

// handleNikkiPanel отдаёт адрес веб-панели mihomo — единственный ответ
// демона, в котором есть секрет Clash API.
//
// Отдельный запрос, а не поле статуса: /api/status опрашивается раз в
// секунду, и секрет ездил бы в каждом ответе. Здесь он уходит ровно один
// раз, по клику владельца.
//
// GET, а не POST: операция читающая и без побочных эффектов, POST на чтение
// — ложь контракту (и повод для промежуточных слоёв решить, что запрос
// небезопасно повторять). «Явность» действия обеспечивает не метод, а то,
// что запрос делает обработчик клика, а не опрос страницы.
//
// НИ URL, НИ СЕКРЕТ НЕ ПОПАДАЮТ В ЖУРНАЛ ни при одном исходе — ни через
// s.logf, ни текстом ошибки в writeErr. Поэтому сообщения об отказах здесь
// написаны словами, а не собраны из err.Error(): ошибка клиента содержит
// адрес, по которому он ходил.
//
// Все три отказа — 503 и в порядке «чинится в браузере → чинится на
// роутере»: host_unknown (открыто через туннель), nikki_unconfigured
// (api_secret/api_listen не заданы), panel_missing (статики по /ui/ нет).
func (s *Server) handleNikkiPanel(w http.ResponseWriter, r *http.Request) {
	host, ok := panelHost(r.Host)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "host_unknown",
			"По заголовку Host адрес роутера не выводится: панель открыта "+
				"через туннель или по петле. Откройте её по адресу роутера в LAN.")
		return
	}

	port, portOK := nikki.PanelPort(s.cfg.NikkiURL)
	if !portOK || s.cfg.NikkiSecret == "" || s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unconfigured",
			"Адрес или секрет Clash API не заданы: нужны nikki.mixin.api_listen "+
				"и nikki.mixin.api_secret. Без секрета панель откроется, но не войдёт.")
		return
	}

	// Живая проба ДО выдачи адреса. Без неё секрет уезжает в адресную строку
	// и оседает в истории браузера ради страницы 404: дашборд качается с
	// GitHub при первом запуске nikki, и без интернета его там просто нет.
	if err := s.nikki.PanelAlive(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "panel_missing",
			"Веб-панель nikki не отвечает: статика дашборда не скачана "+
				"(nikki.mixin.ui_url) или сам движок не запущен.")
		return
	}

	u, ok := nikki.PanelURL(host, port, s.cfg.NikkiSecret)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unconfigured",
			"Адрес панели не собрался из имеющихся данных.")
		return
	}

	// Cache-Control: no-store ставит writeJSON для всех ответов. Здесь это не
	// украшение, а условие выдачи: в теле секрет, и его копия в кэше браузера
	// или прокси пережила бы ротацию. Пинится TestNikkiPanelIsNotCacheable.
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

// AutoSentinel — значение, возвращающее группу к автовыбору.
//
// Имя из SPEC §7: спека предполагала, что в профиле придётся завести
// вложенную группу AUTO. Разбор исходников mihomo показал, что этого не
// нужно — движок умеет снимать закрепление сам
// (DELETE /proxies/{группа} → ForceSet("")). Внешний контракт спеки
// сохранён, реализация оказалась проще.
const AutoSentinel = "AUTO"

// handleNikkiProxy — быстрая операция (SPEC §5): без джоба и светодиода.
func (s *Server) handleNikkiProxy(w http.ResponseWriter, r *http.Request) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}

	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Не указано имя узла")
		return
	}

	// Вид строки проверяется ДО похода в движок, и это не оптимизация.
	//
	// Заголовок раздела в подписке — побайтовая копия соседнего узла
	// (docs/recon/happ.md), различаются они только оформлением имени.
	// Отправив такое имя в mihomo, мы получили бы либо 404 «узла нет» —
	// неправду, потому что строка в списке есть и владелец её видит, — либо,
	// если имена где-то совпадут, молчаливое переключение не на тот сервер.
	// 409 говорит ровно то, что случилось: строка есть, узлом не является.
	//
	// AUTO под правило не подпадает: это наш сентинель снятия закрепления,
	// а не имя строки списка. Строка «Авто» из подписки (kind: auto) —
	// подпадает, и панель на нажатие по ней шлёт как раз AUTO.
	//
	// Манифеста нет — видов не существует вовсе, проверять нечего, и запрос
	// уходит в движок как раньше. Запрет по незнанию отнял бы у владельца
	// рабочий узел на свежей установке.
	if in.Name != AutoSentinel {
		if kind, known := s.entryKind(in.Name); known && kind != happ.KindNode {
			writeErr(w, http.StatusConflict, "member_not_selectable",
				"Строка «"+in.Name+"» узлом не является (вид «"+string(kind)+
					"»), подключиться к ней нельзя. Выберите узел; "+
					"для возврата к автовыбору — AUTO.")
			return
		}
	}

	var err error
	if in.Name == AutoSentinel {
		err = s.nikki.Unfix(r.Context(), ProxyGroup)
	} else {
		err = s.nikki.Select(r.Context(), ProxyGroup, in.Name)
	}
	switch {
	case err == nil:
	case errors.Is(err, nikki.ErrNotSelectable):
		// 409, а не 501: конфликт с текущей конфигурацией, а не
		// нереализованная возможность. Чинится правкой профиля.
		writeErr(w, http.StatusConflict, "group_not_selectable", err.Error())
		return
	case errors.Is(err, nikki.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	case errors.Is(err, nikki.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Clash API не отвечает")
		return
	default:
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}

	s.handleNikkiProxies(w, r)
}

// entryKind — вид строки по манифесту подписки.
//
// Второе значение — «манифест про это имя знает». Ложь означает одно из
// двух: манифеста нет вовсе (свежая установка, ни одного обновления) или
// узел появился в движке мимо подписки. Оба случая не повод отказывать —
// вид записи неизвестен, а не «неподходящий».
//
// Побеждает ПЕРВОЕ совпадение, и это то же правило, по которому строит
// список subs.Order; совпадение двух правил — контракт, а не случайность:
// разойдись мы с Order — отказ прилетел бы строке, которую панель показала
// выбираемой. Сами дубликаты имён в манифесте, написанном этим демоном,
// невозможны: happ.dedupeNames разводит их суффиксом до записи. Здесь —
// страховка на случай манифеста от другой версии демона или правленного
// руками.
//
// Линейный поиск на три десятка записей вместо индекса: карту пришлось бы
// пересобирать при каждом обновлении манифеста и держать в согласии с
// кэшем, а выигрыш измерялся бы наносекундами на нажатие кнопки.
func (s *Server) entryKind(name string) (happ.Kind, bool) {
	for _, e := range s.manifestEntries() {
		if e.Name == name {
			return e.Kind, true
		}
	}
	return "", false
}
