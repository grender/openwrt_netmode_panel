// Package httpapi — HTTP-слой демона.
//
// Слушает только на LAN-адресе и требует токен (ADR-0014). Наружу не
// выставляется ни при каких условиях: `0.0.0.0` отвергается при старте,
// а не логируется предупреждением.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"netmoded/internal/b4"
	"netmoded/internal/executor"
	"netmoded/internal/job"
	"netmoded/internal/logs"
	"netmoded/internal/netif"
	"netmoded/internal/nikki"
	"netmoded/internal/uci"
	"netmoded/internal/wireless"
)

// upstreamIf — L3-интерфейс внешнего канала (raw/11). Держится здесь, а не
// в конфиге: сделать его настраиваемым означало бы дать способ увести
// вызовы на чужой объект.
//
// Имён радио рядом НЕТ намеренно. Какое радио работает станцией, а какое
// домашней точкой, выводится из системы за каждую сборку статуса
// (wireless.ResolveRadios, ADR-0019): индекс радио задаётся порядком
// регистрации драйверов, а не диапазоном, и после перепрошивки константа
// молча указывала бы на домашнюю сеть.
const upstreamIf = "wwan"

// StatusCacheTTL — кэш дорогих чтений.
//
// Панель опрашивает /api/status раз в секунду по SPEC §7, и каждый опрос
// иначе стоил бы четырёх запусков процессов на роутере с 512 МБ. Это кэш,
// а не состояние: его отсутствие меняет только задержку, и ни один путь
// записи в него не заглядывает.
const StatusCacheTTL = 500 * time.Millisecond

// Источники статуса — ключи для журнала деградации.
//
// Имена совпадают с тем, что видно в системе (команда или сервис): в syslog
// роутера строка обязана указывать на то, что чинить, без чтения исходника.
const (
	srcWirelessCfg   = "uci show wireless"
	srcWirelessState = "ubus network.wireless status"
	srcIwinfo        = "ubus iwinfo info"
	srcUpstream      = "ubus network.interface." + upstreamIf + " status"
	srcNikki         = "nikki"
	srcB4            = "b4"
	srcMode          = "uci get netmode.main.mode"
)

// Status — тело ответа GET /api/status.
//
// Форма зафиксирована в docs/api/openapi.yaml, рядом лежат примеры в
// docs/api/examples/: панель разрабатывается от того же контракта.
// Механического сторожа у формы нет — правка полей обязана доходить до
// обоих файлов руками.
type Status struct {
	GeneratedAt string `json:"generated_at"`
	Hostname    string `json:"hostname"`
	Mode        string `json:"mode"`

	SelectionState string   `json:"selection_state"`
	ConfiguredSSID *string  `json:"configured_ssid"`
	AssociatedSSID *string  `json:"associated_ssid"`
	Conflict       []string `json:"conflict,omitempty"`
	PendingApply   bool     `json:"pending_apply"`
	Fingerprint    string   `json:"wireless_fingerprint"`

	AP     APStatus     `json:"ap"`
	Online OnlineStatus `json:"online"`
	Links  Links        `json:"links"`

	Job          *job.Job      `json:"job"`
	Subscription *Subscription `json:"subscription"`
	LastFail     *Fail         `json:"last_fail"`
	Nikki        Service       `json:"nikki"`
	B4           Service       `json:"b4"`
}

type APStatus struct {
	SSID    string `json:"ssid"`
	Band    string `json:"band"`
	Clients *int   `json:"clients"` // null, пока RQ-05 не отвечён
}

type OnlineStatus struct {
	OK bool `json:"ok"`
	// Checked — удалось ли вообще выяснить состояние канала.
	//
	// Без этого признака ok=false означает сразу две разные вещи: «связи
	// нет» и «спросить не смогли». Первое чинится сетью, второе — демоном,
	// и панель обязана их различать.
	Checked   bool   `json:"checked"`
	CheckedAt string `json:"checked_at"`
}

// Links — адреса чужих веб-интерфейсов для браузера владельца: две морды
// движков и веб-интерфейс самого роутера. Разница между ними не косметическая
// и разобрана в ADR-0024: движки гасит netmode-apply, поэтому их кнопки панель
// прячет по available, а uhttpd мы не трогаем, и кнопка LuCI остаётся всегда.
//
// Заполняются в обработчике, а не в build: адрес собирается из заголовка
// Host конкретного запроса, а build отдаёт один снимок, который кэшируется
// на 500 мс и уходит РАЗНЫМ клиентам. Ссылка, собранная по Host первого
// зашедшего, увела бы второго на чужой хост.
//
// null означает «адрес не собран»: Host непригоден — пусто, петля, мусорные
// символы. Все поля заполняются одинаково; «ещё не реализовано» среди причин
// больше нет. Клиент не имеет права достраивать ссылку сам — ни из своего
// window.location, ни из hostname в этом же ответе.
//
// От available поля не зависят, а показ кнопки — зависит, и это разные
// предметы. У обоих движков веб-морда живёт на том же слушателе, который
// опрашивает демон, поэтому при available:false ссылка гарантированно упрётся
// в connection refused, и панель кнопку не рисует
// (ADR-0024: docs/adr/0024-panel-links-only-when-service-answers.md).
// Обнулять поле здесь нельзя: null перестал бы различать «хост не вывелся»
// и «служба легла», а живость приехала бы из кэша в некэшируемое поле.
//
// LuCI в этой компании — исключение, и оно записано в самом правиле, а не
// сделано вопреки ему: ADR-0024 проводит границу через «управляется
// netmode-apply», раздел «Границы правила». uhttpd им не управляется, гаснуть
// по нашей вине не может, и available для него мы не считаем.
//
// Порядок полей — часть контракта: он же порядок в docs/api/openapi.yaml и
// порядок ключей в фикстурах. Сторожа у совпадения нет (сказано там же),
// поэтому новое поле дописывается В КОНЕЦ, а не втискивается по алфавиту.
type Links struct {
	Nikki *string `json:"nikki"`
	B4    *string `json:"b4"`
	LuCI  *string `json:"luci"`
}

// Subscription — результат последнего обновления подписки.
type Subscription struct {
	LastUpdate *string `json:"last_update"`
	Status     string  `json:"status"`
	Nodes      int     `json:"nodes"`
	Error      *string `json:"error"`
}

// Fail — последняя неудачная смена внешней сети (см. failStore).
//
// Reason — типизированный машинный код, а не string: в JSON он уезжает той же
// строкой, что и раньше (контракт не менялся), но внутри демона его нельзя
// перепутать с ssid, который лежит соседним полем и соседним параметром
// failStore.Set.
type Fail struct {
	SSID   string     `json:"ssid"`
	Reason FailReason `json:"reason"`
	At     string     `json:"at"`
}

// Service — состояние подключаемого движка (nikki, b4).
//
// omitempty здесь не стоит нигде, кроме Pinned, и это принципиально:
// пропавшее поле означает для клиента «демон не прислал», а не «значения
// нет». `enabled_count: 0` — это факт («b4 отвечает, включённых сетов нет»),
// и молчать о нём значит выдавать факт за сбой связи.
type Service struct {
	Available bool `json:"available"`
	// Version — null, пока движок не ответил. Именно null, а не пропуск:
	// поле обещано контрактом как всегда присутствующее.
	Version *string `json:"version"`
	// Set — активный узел (Nikki) или сет (b4). "" — движок недоступен либо
	// однозначного выбора нет.
	Set string `json:"set"`
	// Pinned — выбран ли узел вручную.
	//
	// Без этого признака панель не отличит «движок подобрал» от «закреплено
	// руками»: имя в Set в обоих случаях одинаковое.
	//
	// Единственное поле с omitempty: признак осмыслен только при
	// Available, и его отсутствие читается как «не закреплено».
	Pinned       bool `json:"pinned,omitempty"`
	EnabledCount int  `json:"enabled_count"`
}

// StatusReader собирает статус, кэшируя дорогие чтения.
type StatusReader struct {
	ex    executor.Executor
	b4    b4.Client
	nikki nikki.Client
	jobs  *job.Manager
	logs  *logs.Log
	fails *failStore
	now   func() time.Time
	logf  func(string, ...any)

	mu     sync.Mutex
	cached *Status
	at     time.Time
	// down — источники, о недоступности которых уже сказано в журнале.
	// Живёт под тем же mu, что и кэш: пишется только из build.
	down map[string]bool
}

func NewStatusReader(ex executor.Executor, logf func(string, ...any)) *StatusReader {
	return newStatusReader(&StatusReader{ex: ex, b4: b4.New(b4.DefaultBaseURL)}, logf)
}

// NewStatusReaderWith собирает читателя с готовыми клиентами.
func NewStatusReaderWith(ex executor.Executor, b4c b4.Client, nk nikki.Client, logf func(string, ...any)) *StatusReader {
	return newStatusReader(&StatusReader{ex: ex, b4: b4c, nikki: nk}, logf)
}

func newStatusReader(r *StatusReader, logf func(string, ...any)) *StatusReader {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r.now = time.Now
	r.logf = logf
	r.down = map[string]bool{}
	return r
}

// Read возвращает статус, не старше StatusCacheTTL.
//
// Ошибка всегда nil, и ветку под неё заводить не надо: статус обязан
// отвечать при любом сбое источников, а сам сбой виден в ответе
// (online.checked, mode, *.available) и в журнале демона. Второе значение
// оставлено только потому, что Read — экспортированный контракт; если оно
// когда-нибудь станет ненулевым, это будет отказ отвечать вообще.
//
// Наружу уходит КОПИЯ, и на промахе тоже. Раньше промах отдавал тот самый
// указатель, который клался в r.cached: обработчик, дописавший в ответ хоть
// одно поле (например, ссылку на веб-морду, зависящую от заголовка Host),
// портил бы кэш через раз — при опросе раз в секунду и TTL 500 мс чужой
// адрес прилипал бы к ответу примерно каждые две секунды, а тест с одним
// запросом такого не видит вовсе. Это свойство Read, а не конкретного поля:
// мина стояла бы под любым будущим обработчиком.
//
// Копия ПОВЕРХНОСТНАЯ и остаётся такой сознательно: срез Conflict, поля
// *string и *job.Job общие с кэшем. Это допустимо ровно потому, что
// обработчики их не мутируют, а присваивают целиком — st.Links.B4 = &u
// меняет копию, а не то, на что она смотрит. Дописать элемент в s.Conflict
// или поправить *s.ConfiguredSSID означало бы испортить кэш заново, уже
// другим способом; такой правке нужен глубокий клон здесь же.
func (r *StatusReader) Read(ctx context.Context) (*Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached == nil || r.now().Sub(r.at) >= StatusCacheTTL {
		s := r.build(ctx)
		r.cached, r.at = s, r.now()
	}

	// Джоб обновляем даже из кэша: он меняется чаще, чем раз в 500 мс,
	// и застывший прогресс выглядел бы как зависшая операция.
	cp := *r.cached
	if r.jobs != nil {
		cp.Job = r.jobs.Current()
	}
	// last_fail читается ЗДЕСЬ, а не в build, по той же причине, что и
	// джоб, но цена ошибки другая. Из кэша он приезжал бы дважды неверным:
	// свежая неудача ждала бы до полусекунды (а панель в этот момент
	// показывает пустой статус и «всё в порядке»), а стёртая успешным
	// переключением ещё полсекунды висела бы рядом с уже работающей сетью.
	// Оба случая — доклад о том, чего нет, а поле заведено ровно затем,
	// чтобы докладывать честно (ADR-0025).
	if r.fails != nil {
		cp.LastFail = r.fails.Get()
	}
	return &cp, nil
}

// noteLocked сообщает журналу о СМЕНЕ доступности источника.
//
// Дедупликация здесь не украшение. Панель опрашивает /api/status раз в
// секунду, кэш живёт 500 мс, источников семь: строка на каждое неудачное
// чтение — это до семи записей в секунду в syslog роутера, который пишет
// на overlay-флеш. Такой журнал изнашивает флеш и топит в себе всё
// остальное, то есть сам становится вторым сбоем. Поэтому пишутся только
// переходы «работало → сломалось» и обратно, а повторы молчат.
//
// Отменённый контекст источником сбоя НЕ считается: это наш собственный
// отказ от чтения (клиент ушёл со страницы, сработал таймаут панели), а не
// молчание роутера. Без этой проверки каждая отмена писала бы «источник
// недоступен», а следующее чтение — «снова отвечает», и журнал заполнялся
// бы парами строк о том, чего не происходило.
//
// Вызывается из build, то есть под r.mu.
func (r *StatusReader) noteLocked(src string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		if r.down[src] {
			return
		}
		r.down[src] = true
		r.logf("статус: %s недоступен — %v", src, err)
		return
	}
	if r.down[src] {
		delete(r.down, src)
		r.logf("статус: %s снова отвечает", src)
	}
}

// build читает систему заново.
//
// Ни одно чтение не считается обязательным: недоступность любого источника
// деградирует соответствующее поле, но не валит весь статус. Панель,
// которая перестала отвечать целиком из-за упавшего b4, бесполезна ровно
// тогда, когда нужна. Отсюда и сигнатура без ошибки: возвращать её было бы
// нечестно — вызывающему нечего с ней делать, кроме как отдать 500 вместо
// работающей панели.
//
// Молча зануляемое поле — это тоже сбой, просто невидимый, поэтому каждое
// чтение проходит через noteLocked: сбой обязан быть в журнале.
func (r *StatusReader) build(ctx context.Context) *Status {
	now := r.now().UTC().Format(time.RFC3339)
	s := &Status{
		GeneratedAt: now,
		Mode:        r.mode(ctx),
		Online:      OnlineStatus{CheckedAt: now},
		Nikki:       Service{Available: false},
		B4:          Service{Available: false},
	}

	// Конфигурация wireless: отпечаток и разбор.
	//
	// Разбор считается частью чтения: неразобранный вывод — такой же слепой
	// статус, как и не полученный, и в журнале обязан выглядеть так же.
	var cfg *uci.Config
	rawWireless, err := r.ex.UCIShow(ctx, "wireless")
	if err == nil {
		s.Fingerprint = wireless.Fingerprint(rawWireless)
		cfg, err = uci.ParseShow("wireless", rawWireless)
	}
	r.noteLocked(srcWirelessCfg, err)

	// Живое состояние радио: pending и то, что даёт разрешение ролей.
	var st map[string]wireless.Radio
	b, err := r.ex.UbusCall(ctx, "network.wireless", "status", nil)
	if err == nil {
		if st, err = wireless.ParseStatus(b); err == nil {
			s.PendingApply = wireless.PendingApply(st)
		}
	}
	r.noteLocked(srcWirelessState, err)

	// Привязка «радио ↔ роль ↔ диапазон» разрешается ОДИН раз за сборку и
	// дальше переиспользуется: два разрешения в одном ответе могли бы
	// разойтись между собой, и статус описывал бы два разных роутера.
	//
	// Не вывелась — поля просто остаются пустыми. Статус обязан отвечать
	// при любом сбое источников (ADR-0017), поэтому 503 radio_unknown —
	// удел путей, которые собираются что-то ТРОГАТЬ, а не докладывать.
	radios := wireless.ResolveRadios(st, cfg)

	if cfg != nil && radios.Known() {
		sel := wireless.Classify(cfg, radios.Station)
		s.SelectionState = string(sel.State)
		s.Conflict = sel.Conflict
		if sel.State == wireless.Single {
			if sec, ok := cfg.Section(sel.Active); ok {
				v := sec.Options["ssid"]
				s.ConfiguredSSID = &v
			}
		}
	}
	if s.SelectionState == "" {
		s.SelectionState = string(wireless.Empty)
	}

	// Домашняя точка: имя из конфигурации, диапазон из разрешения. Имя её
	// интерфейса не выводится и не нужно: число клиентов остаётся null,
	// пока не отвечён RQ-05.
	if cfg != nil {
		if ap := apSection(cfg, radios.AP); ap != nil {
			s.AP.SSID = ap.Options["ssid"]
		}
	}
	s.AP.Band = radios.APBand

	// Имя станционного интерфейса — по выведенному радио, а не по константе.
	// Пусто, если радио не выяснено или станция не поднята.
	stationIf := wireless.IfnameForMode(st, radios.Station, "sta")

	// Ассоциация. Имя интерфейса выведено, а не захардкожено.
	//
	// Без имени интерфейса спрашивать нечего и жаловаться не на что: это
	// следствие уже отмеченного сбоя выше, а не отдельный сбой iwinfo.
	if stationIf != "" {
		b, err := r.ex.UbusCall(ctx, "iwinfo", "info", map[string]any{"device": stationIf})
		if err == nil {
			var info wireless.Info
			if info, err = wireless.ParseInfo(b); err == nil && info.Associated {
				v := info.SSID
				s.AssociatedSSID = &v
			}
		}
		r.noteLocked(srcIwinfo, err)
	}

	// Внешний канал. Checked отделяет «проверили, канала нет» от «проверить
	// не смогли»: без него оба случая выглядят как ok=false.
	b, err = r.ex.UbusCall(ctx, "network.interface."+upstreamIf, "status", nil)
	if err == nil {
		var ifs netif.Status
		if ifs, err = netif.ParseStatus(b); err == nil {
			s.Online.OK = ifs.Online()
			s.Online.Checked = true
		}
	}
	r.noteLocked(srcUpstream, err)

	// Nikki: недоступность гасит список узлов, но не валит статус.
	if r.nikki != nil {
		all, err := r.nikki.Proxies(ctx)
		if err == nil {
			s.Nikki.Available = true
			if g, ok := all["PROXY"]; ok {
				s.Nikki.Set = g.Now
				s.Nikki.Pinned = g.Pinned
			}
			if v, verr := r.nikki.Version(ctx); verr == nil {
				s.Nikki.Version = &v
			}
		}
		r.noteLocked(srcNikki, err)
	}

	// b4: недоступность гасит чипы сетов, но не валит статус.
	if r.b4 != nil {
		sets, err := r.b4.Sets(ctx)
		if err == nil {
			s.B4.Available = true
			s.B4.Set = b4.Selected(sets)
			s.B4.EnabledCount = b4.EnabledCount(sets)
			if v, verr := r.b4.Version(ctx); verr == nil {
				s.B4.Version = &v.Version
			}
		}
		r.noteLocked(srcB4, err)
	}

	// Текущая операция. Кэш её не задерживает: джоб меняется чаще, чем
	// раз в 500 мс, и панель обязана видеть прогресс сразу.
	if r.jobs != nil {
		s.Job = r.jobs.Current()
	}

	// Последнее обновление подписки — из журнала, а не из памяти: после
	// перезапуска демона картина обязана восстанавливаться (SPEC §12).
	if r.logs != nil {
		if e, ok, lerr := r.logs.Last(); lerr == nil && ok {
			ts := e.TS.UTC().Format(time.RFC3339)
			sub := &Subscription{LastUpdate: &ts, Status: e.Status, Nodes: e.Nodes}
			if e.Err != "" {
				msg := e.Err
				sub.Error = &msg
			}
			s.Subscription = sub
		} else {
			s.Subscription = &Subscription{Status: "never"}
		}
	}

	s.Hostname = r.hostname(ctx)
	return s
}

// mode читает намерение владельца.
//
// Значение вне набора не исправляется: отдаём "unknown" и оставляем файл
// как есть (ADR-0010). Отсутствие записи — это "off", а не сбой: свежая
// установка выглядит именно так.
//
// Сбой чтения — тоже "unknown", а не "off": выдать "off" означало бы
// сказать панели «владелец выключил режим», хотя мы просто не спросили.
// Владелец увидел бы выключенный тумблер при работающем nikki.
func (r *StatusReader) mode(ctx context.Context) string {
	v, err := r.ex.UCIGet(ctx, "netmode", "main", "mode")
	switch {
	case errors.Is(err, executor.ErrNotFound):
		r.noteLocked(srcMode, nil)
		return "off"
	case err != nil:
		r.noteLocked(srcMode, err)
		return "unknown"
	}
	r.noteLocked(srcMode, nil)

	switch v {
	case "nikki", "b4":
		return v
	case "off", "":
		// Пустое значение неотличимо от свежей установки: запись есть,
		// намерения в ней нет.
		return "off"
	default:
		return "unknown"
	}
}

func (r *StatusReader) hostname(ctx context.Context) string {
	if v, err := r.ex.UCIGet(ctx, "system", "@system[0]", "hostname"); err == nil && v != "" {
		return v
	}
	return "OpenWrt"
}

// apSection находит домашнюю точку доступа — только чтобы показать её имя.
// Мы её не трогаем: она не наша (ADR-0003).
//
// Радио передаётся, а не берётся из константы: какое из них домашняя точка,
// выясняется из системы (ADR-0019). Пустое имя означает «не выяснили» — и
// тогда секции нет, а не «подойдёт любая точка доступа».
func apSection(c *uci.Config, radio string) *uci.Section {
	if radio == "" {
		return nil
	}
	for i := range c.Sections {
		s := &c.Sections[i]
		if s.Type == "wifi-iface" && s.Options["device"] == radio && s.Options["mode"] == "ap" {
			return s
		}
	}
	return nil
}

// MarshalStatus — сериализация с отступами, как в golden-фикстурах.
func MarshalStatus(s *Status) ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}
