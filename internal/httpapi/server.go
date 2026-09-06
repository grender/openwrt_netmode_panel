package httpapi

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"netmoded/internal/b4"
	"netmoded/internal/executor"
	"netmoded/internal/happ"
	"netmoded/internal/job"
	"netmoded/internal/led"
	"netmoded/internal/logs"
	"netmoded/internal/luci"
	"netmoded/internal/nikki"
	"netmoded/internal/safe"
	"netmoded/internal/sched"
	"netmoded/internal/subs"
	"netmoded/internal/uci"
	"netmoded/internal/wireless"
)

// Panel — статика панели, зашитая в бинарь. Ничего не читается с диска:
// один файл проще ставить и невозможно рассинхронизировать с кодом.
//
//go:embed all:panel
var Panel embed.FS

// Config — параметры сервера из /etc/config/netmode.
type Config struct {
	Listen string // только LAN-адрес; 0.0.0.0 отвергается
	Port   int
	Token  string

	// NikkiURL и NikkiSecret читаются из пакета nikki при старте
	// (nikki.mixin.api_listen и api_secret). Копия не хранится: ротация
	// секрета не должна ломать демона (ADR-0002).
	NikkiURL    string
	NikkiSecret string

	// LogPath — журнал обновлений подписки. Пусто → путь по умолчанию
	// из SPEC §9 (/etc/nikki/updates.log, НЕ /var: там tmpfs).
	LogPath string
	// SubInterval — как часто обновлять подписку. Пусто → 12 часов.
	SubInterval time.Duration
	// SubscriptionURL — адрес подписки провайдера
	// (netmode.main.subscription_url). Пусто — валидное состояние: демон
	// работает, расписание молчит, панель показывает подсказку вместо
	// кнопки. Секрет: в строке лежит идентификатор подписки, поэтому
	// наружу он не уходит ни одним путём — ни в /api/status, ни в текст
	// ошибки, ни в журнал демона (ADR-0012).
	SubscriptionURL string
	// ProviderPath и ManifestPath — куда обновление кладёт файл провайдера
	// и манифест. Пусто → константы ниже. Это НЕ настройка владельца:
	// loadConfig их не читает, в /etc/config/netmode их нет и не будет
	// (см. комментарий к константам). Поля существуют ради одного —
	// чтобы тест мог собрать сервер боевым NewServer и увести файлы в
	// t.TempDir(): иначе проводка «обновление → кэш манифеста» не
	// проверяется ничем, и её удаление оставляло все тесты зелёными.
	ProviderPath string
	ManifestPath string
	// SubscriptionFetch — скачивание подписки. Пусто → happ.Fetch. Только
	// для тестов, по той же причине, что и пути выше; боевой путь обязан
	// идти через happ, который умеет не показывать адрес в ошибках.
	SubscriptionFetch func(ctx context.Context, url string) ([]byte, error)
	// LEDRoot — каталог светодиодов. Пусто → /sys/class/leds.
	LEDRoot string
	// Logf — журнал демона. Пусто → тишина.
	Logf func(string, ...any)
}

// Server отдаёт API и панель.
type Server struct {
	cfg    Config
	ex     executor.Executor
	b4     b4.Client
	nikki  nikki.Client
	jobs   *job.Manager
	logs   *logs.Log
	sched  *sched.Scheduler
	led    *led.Controller
	logf   func(string, ...any)
	status *StatusReader
	// fails — последняя неудачная смена внешней сети. Один слот в памяти,
	// перезапуск не переживает (ADR-0025).
	fails *failStore
	// bridgeFails — тот же слот для операций проброса (ADR-0030). Второй
	// экземпляр, а не общий: у моста своя таксономия и своё поле в ответе,
	// и смешав их, панель показывала бы неудачу переключения сети на
	// вкладке проброса.
	bridgeFails *bridgeFailStore
	// subURL — живой адрес подписки (ADR-0034). Не cfg.SubscriptionURL:
	// с появлением PUT /api/subscription адрес перестал быть снимком,
	// сделанным при старте, а читателей у него три — гейт обновления,
	// расписание и признак Configured в статусе.
	subURL *subURLStore
	// Кэш проб моста: ping держит запрос секунды, а вкладку панель
	// перезапрашивает часто.
	bridgeProbeMu  sync.Mutex
	bridgeProbeVal *BridgeProbes
	bridgeProbeAt  time.Time
	// Кэш манифеста подписки: порядок и виды записей, которых нет в Clash API.
	//
	// Файл читается не на каждый запрос намеренно. GET /api/nikki/proxies
	// панель дёргает часто (вкладка узлов опрашивается на каждом открытии и
	// после каждого замера), а манифест меняется раз в двенадцать часов —
	// то есть почти каждое чтение отдало бы ровно то же самое, заплатив за
	// это обращением к флешу.
	//
	// RWMutex, а не просто Mutex: читателей много и они конкурируют между
	// собой, писатель один и приходит раз в полсуток. Замок обязателен в
	// любом виде — читают HTTP-обработчики, пишет горутина расписания, и
	// без него это гонка, которую -race поймает, а роутер покажет мусором
	// в списке узлов.
	manifestMu sync.RWMutex
	manifest   []happ.Entry
	mux        *http.ServeMux
}

// Пути файлов подписки — константы, а не настройка в /etc/config/netmode.
//
// Оба они не настройка владельца, а часть договорённости с соседями:
// providerPath называет nikki (движок читает этот каталог сам),
// manifestPath — наш собственный каталог рядом с токеном. Вынеся их в
// /etc/config/netmode, мы получили бы значение, которое можно поменять
// только вместе с конфигурацией mihomo, — то есть настройку, ломающую
// демона при любом изменении. Одноимённые поля Config переопределяют их
// только из тестов (см. Config).
const (
	providerPath = "/etc/nikki/run/providers/sub.yaml"
	manifestPath = "/etc/netmoded/subscription.json"
	// providerName — имя провайдера в Clash API: PUT /providers/proxies/sub.
	//
	// Совпадает с именем файла без расширения ПО ЭТОМУ КОНКРЕТНОМУ ПРОФИЛЮ
	// nikki, а не по требованию mihomo: имя провайдера — ключ карты
	// proxy-providers: в конфиге движка, а path: внутри записи задаётся
	// отдельно и от имени файла не зависит вовсе (SPEC §2; evidence.json,
	// запись nikki.provider_file). Менять эту константу можно только
	// вместе с ключом в конфиге mihomo — переименование одного файла здесь
	// ничего не чинит и не ломает, а «парная» правка по прежней редакции
	// этого комментария дала бы 404 от движка при корректном файле на диске.
	providerName = "sub"
)

// apiError — единая форма ошибки (docs/contracts/errors.md).
// Код важнее текста: 409 в этой панели означает три разные вещи, и по
// одному номеру клиент не поймёт, что случилось.
type apiError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func NewServer(cfg Config, ex executor.Executor) (*Server, error) {
	if err := validateListen(cfg.Listen); err != nil {
		return nil, err
	}
	if err := validateToken(cfg.Token); err != nil {
		return nil, err
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("порт %d вне диапазона", cfg.Port)
	}

	// Журнал заводится ПЕРВЫМ и до конструкторов: всё, что умеет
	// деградировать молча (статус, светодиоды), обязано получить рабочий
	// logf, а не nil, оставшийся от порядка инициализации.
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	s := &Server{
		cfg:    cfg,
		ex:     ex,
		logf:   logf,
		b4:     b4.New(b4.DefaultBaseURL),
		nikki:  nikki.New(cfg.NikkiURL, cfg.NikkiSecret),
		status: NewStatusReader(ex, logf),
		jobs:   job.NewManager(logf),
		logs:   logs.New(cfg.LogPath),
		fails:  &failStore{},
		mux:    http.NewServeMux(),
	}
	s.bridgeFails = &bridgeFailStore{}
	s.led = led.New(cfg.LEDRoot, s.logf)

	// Обновление подписки. Reload замыкается на s, а не на s.nikki: клиент
	// подменяется после конструктора (SetNikkiClient — этим пользуются и
	// тесты, и дев-сервер), и замыкание на значение поля заморозило бы
	// подмену — обновление ходило бы в отброшенного клиента.
	if cfg.ProviderPath == "" {
		cfg.ProviderPath = providerPath
	}
	if cfg.ManifestPath == "" {
		cfg.ManifestPath = manifestPath
	}
	s.cfg = cfg
	s.subURL = &subURLStore{v: cfg.SubscriptionURL}
	up := &subs.Updater{
		URL:          s.subURL.get,
		ProviderPath: cfg.ProviderPath,
		ManifestPath: cfg.ManifestPath,
		Fetch:        cfg.SubscriptionFetch,
		Reload: func(ctx context.Context) error {
			// Проверка ВНУТРИ замыкания, а не при сборке: клиент
			// подменяется после конструктора, и nil здесь — это
			// состояние на момент вызова. Без проверки subs.Update
			// увидел бы непустой Reload, записал бы оба файла и упал
			// паникой на нулевом интерфейсе — «половина дела», от
			// которой предохранитель Reload == nil как раз и защищает.
			if s.nikki == nil {
				return errors.New("subs: клиент Clash API не настроен, провайдера перечитать некому")
			}
			return s.nikki.ReloadProvider(ctx, providerName)
		},
		// Сверка после перечитывания: 204 от движка не значит «прочитал наш
		// файл» (см. subs.ErrProviderIgnored). Замыкание по той же причине,
		// что у Reload.
		Proxies: func(ctx context.Context) ([]string, error) {
			if s.nikki == nil {
				return nil, errors.New("subs: клиент Clash API не настроен")
			}
			return s.nikki.ProviderProxies(ctx, providerName)
		},
		Logf: s.logf,
	}
	s.sched = sched.New(&manifestRefresher{up: up, srv: s}, s.logs, cfg.SubInterval, s.logf)
	// Кэш заполняется на старте, а не при первом запросе: пустой манифест —
	// валидное состояние свежей установки, и отличить его от «ещё не
	// читали» по самому кэшу было бы нечем.
	s.reloadManifest()
	s.status.jobs = s.jobs
	s.status.logs = s.logs
	// Признак «адрес подписки задан» — тот же, по которому отвечает
	// POST /api/subscription/update. Один источник на оба ответа: иначе
	// панель показывала бы кнопку рабочей ровно там, где нажатие отбивается
	// кодом subscription_not_configured. САМ АДРЕС сюда не едет — секрет.
	s.status.subConfigured = s.subURL.configured
	// Слот неудачи — ОДИН на демона, и читатель статуса обязан смотреть в
	// тот же, в который пишет джоб переключения. Второй экземпляр здесь
	// означал бы, что доклад пишется в один объект, а показывается из
	// другого, — и last_fail был бы вечно пуст (ровно так уже терялся
	// клиент Nikki, см. TestNewServerWiresClientsIntoStatus).
	s.status.fails = s.fails
	// Клиент Nikki отдаётся читателю статуса явно.
	//
	// NewStatusReader заводит своего клиента b4, но не Nikki: адрес и секрет
	// Clash API известны только из конфигурации, до которой у него доступа
	// нет. Без этой строки поле остаётся nil, весь блок чтения Nikki в build
	// молча пропускается, и /api/status вечно докладывает «недоступен» —
	// в то время как GET /api/nikki/proxies, ходящий через s.nikki, отвечает
	// списком узлов. Панель показывала бы владельцу два взаимоисключающих
	// утверждения сразу.
	//
	// Ошибку не поймали тесты, потому что и SetNikkiClient, и тестовые
	// помощники подставляют клиента принудительно: тестовая проводка
	// отличалась от боевой ровно в сломанном месте. Отсюда TestNewServerWires
	// ниже — он идёт боевым путём NewServer, а не через подмену.
	s.status.nikki = s.nikki
	s.routes()
	warnMissingExecutors(ex, logf)
	return s, nil
}

// warnMissingExecutors кричит в журнал, если применять нечем.
//
// Старт при этом НЕ отказывает, и это решение, а не мягкость. Демон без
// netmode-wifi всё ещё показывает статус, журнал, состояние движков и
// ссылку на LuCI; мёртвый демон не показывает ничего, а владелец без панели
// остаётся с одним ssh. Отказ был бы уместен, если бы без скрипта не
// работало ничего, — но не работает одна операция из четырёх.
//
// Молчать при этом нельзя. Ровно так и вышло на живом роутере: демон фазы 2
// приехал без /usr/local/bin/netmode-wifi (deploy.sh льёт его только под
// --install), панель отдавалась с рабочей на вид кнопкой, и владелец узнал
// правду после uci commit — когда намерение уже опубликовано и висит
// неприменённым. Прецедент довода лежит в самом deploy.sh, в проверке
// flock: «узнать об этом при первом нажатии — значит узнать в худший
// момент».
//
// Второй адресат того же факта — /api/status (поле missing_executors) и
// баннер панели. Журнал нужен и при этом: без панели, по ssh, logread —
// единственная дорога.
func warnMissingExecutors(ex executor.Executor, logf func(string, ...any)) {
	missing := ex.MissingExecutors()
	if len(missing) == 0 {
		return
	}
	logf("ВНИМАНИЕ: на роутере нет скриптов применения (%s) — "+
		"смена режима и смена внешней сети запишут намерение, но применить его будет нечем; "+
		"переустановите пакет: deploy.sh --install", strings.Join(missing, ", "))
}

// validateListen не даёт демону выйти наружу.
//
// Отката на «слушать везде» нет ни при каких условиях: это не настройка
// с неудачным значением по умолчанию, а граница безопасности. Пустой адрес
// и 0.0.0.0 — отказ запуска, а не предупреждение в лог (ADR-0014).
func validateListen(addr string) error {
	if addr == "" {
		return errors.New("адрес прослушивания не задан")
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("адрес %q не разбирается", addr)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("адрес %q означает «все интерфейсы» — демон наружу не выставляется", addr)
	}
	return nil
}

// validateToken требует ASCII без разделителей.
//
// Не педантизм: значение куки по RFC 6265 обязано быть ASCII, и net/http
// молча выбрасывает недопустимые байты. Токен с кириллицей дал бы рабочий
// заголовок Authorization и неработающую куку — то есть API отвечал бы,
// а панель не грузилась, и причина была бы неочевидна.
//
// Настоящий токен — base64url от crypto/rand, он этому удовлетворяет.
func validateToken(tok string) error {
	if tok == "" {
		return errors.New("токен пуст: демон без токена не запускается")
	}
	if len(tok) < 16 {
		return fmt.Errorf("токен длиной %d — слишком короткий, нужно ≥16 символов", len(tok))
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c < 0x21 || c > 0x7e || c == '"' || c == ';' || c == ',' || c == '\\' {
			return fmt.Errorf("токен содержит недопустимый для куки символ в позиции %d", i)
		}
	}
	return nil
}

func (s *Server) routes() {
	// Маршруты обязаны совпадать с docs/api/openapi.yaml — сверяет
	// scripts/check-routes.sh.
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/wifi/networks", s.handleWifiNetworks)
	s.mux.HandleFunc("POST /api/wifi/networks", s.handleWifiWrite)
	s.mux.HandleFunc("DELETE /api/wifi/networks/{id}", s.handleWifiDelete)
	s.mux.HandleFunc("GET /api/wifi/scan", s.handleWifiScan)
	s.mux.HandleFunc("POST /api/mode", s.handleMode)
	s.mux.HandleFunc("POST /api/upstream", s.handleUpstream)
	s.mux.HandleFunc("GET /api/bridge", s.handleBridge)
	s.mux.HandleFunc("POST /api/bridge/enable", s.handleBridgeEnable)
	s.mux.HandleFunc("POST /api/bridge/disable", s.handleBridgeDisable)
	s.mux.HandleFunc("POST /api/bridge/access", s.handleBridgeAccess)
	s.mux.HandleFunc("GET /api/nikki/proxies", s.handleNikkiProxies)
	s.mux.HandleFunc("GET /api/nikki/panel", s.handleNikkiPanel)
	s.mux.HandleFunc("POST /api/nikki/proxy", s.handleNikkiProxy)
	s.mux.HandleFunc("POST /api/nikki/test", s.handleNikkiTest)
	s.mux.HandleFunc("GET /api/subscription", s.handleSubscriptionGet)
	s.mux.HandleFunc("PUT /api/subscription", s.handleSubscriptionPut)
	s.mux.HandleFunc("POST /api/subscription/update", s.handleSubscriptionUpdate)
	s.mux.HandleFunc("GET /api/logs", s.handleLogs)
	s.mux.HandleFunc("GET /api/b4/sets", s.handleB4Sets)
	s.mux.HandleFunc("POST /api/b4/set", s.handleB4Set)

	s.mux.Handle("GET /", s.panelHandler())
}

func (s *Server) Handler() http.Handler {
	return s.withToken(s.mux)
}

// Addr — адрес прослушивания.
func (s *Server) Addr() string {
	return net.JoinHostPort(s.cfg.Listen, fmt.Sprint(s.cfg.Port))
}

// withToken проверяет токен на ВСЕХ маршрутах, включая статику.
//
// Статика тоже под токеном намеренно: панель раскрывает имена сетей и
// топологию, и отдавать её без проверки означало бы, что защищён только
// API, а разведка целей — нет.
func (s *Server) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, fromQuery := s.tokenOK(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Неверный или отсутствующий токен")
			return
		}
		if fromQuery {
			// Токен пришёл ссылкой — закрепляем его кукой, иначе следующий
			// же запрос браузера провалится.
			//
			// За <script src> и <link rel=stylesheet> браузер не отправляет
			// ни заголовок Authorization, ни query из адреса страницы. Без
			// куки HTML отдавался бы с кодом 200, app.js получал бы 401, и
			// панель оставалась бы пустой — 200 на главной при этом честно
			// показывал бы, что «всё работает».
			http.SetCookie(w, &http.Cookie{
				Name:     "netmode_token",
				Value:    s.cfg.Token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   30 * 24 * 3600,
				// Secure не ставим: демон слушает LAN по http, и с этим
				// флагом браузер куку просто не сохранит.
			})
		}
		next.ServeHTTP(w, r)
	})
}

// tokenOK проверяет токен и сообщает, пришёл ли он из строки адреса.
func (s *Server) tokenOK(r *http.Request) (ok, fromQuery bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return s.match(strings.TrimPrefix(h, "Bearer ")), false
	}
	// Query проверяется раньше куки: так по свежей ссылке можно перебить
	// протухшую куку, не чистя её руками.
	if q := r.URL.Query().Get("token"); q != "" {
		return s.match(q), true
	}
	if c, err := r.Cookie("netmode_token"); err == nil {
		return s.match(c.Value), false
	}
	return false, false
}

// match сравнивает за постоянное время: иначе токен подбирается по времени
// ответа, а он живёт до ротации вручную.
func (s *Server) match(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

func (s *Server) panelHandler() http.Handler {
	sub, err := fs.Sub(Panel, "panel")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusInternalServerError, "internal", "панель не встроена")
		})
	}
	return http.FileServer(http.FS(sub))
}

// ─────────── обработчики ───────────

// SetB4Client подменяет клиента b4 — нужно тестам и cmd/netmoded-dev.
func (s *Server) SetB4Client(c b4.Client) { s.b4 = c; s.status.b4 = c }

// SetNikkiClient подменяет клиента Nikki.
func (s *Server) SetNikkiClient(c nikki.Client) { s.nikki = c; s.status.nikki = c }

// Close останавливает отложенные таймеры индикации.
func (s *Server) Close() {
	if s.led != nil {
		s.led.Close()
	}
}

// Scheduler возвращает планировщик — вызывающий обязан крутить его Run.
func (s *Server) Scheduler() *sched.Scheduler { return s.sched }

// manifestRefresher — обновление подписки плюс перечитывание кэша манифеста.
//
// Обёртка, а не вызов из планировщика: sched знает только КОГДА обновлять и
// что записать в журнал, а кто ещё интересуется результатом — не его дело
// (ADR-0001, круги). Так же и subs не знает про HTTP-сервер и его кэш.
type manifestRefresher struct {
	up  *subs.Updater
	srv *Server
}

// Update обновляет подписку и всегда перечитывает манифест — включая пути
// неудачи.
//
// Безусловно, и это осознанно. Частичный отказ (файлы записаны, движок
// провайдера не перечитал) тоже оставляет на диске НОВЫЙ манифест, а по
// одной ошибке отличить такой исход от «не скачалось» можно лишь заведя
// таксономию сентинелов — то есть договор между двумя пакетами ради одной
// ветки. Цена безусловного чтения — один лишний read файла раз в двенадцать
// часов; цена ошибки в таксономии — кэш, который врёт про порядок узлов до
// перезапуска демона.
func (m *manifestRefresher) Update(ctx context.Context) (happ.Summary, error) {
	sum, err := m.up.Update(ctx)
	m.srv.reloadManifest()
	return sum, err
}

// manifestEntries отдаёт кэш манифеста для склейки в subs.Order.
//
// Срез отдаётся как есть, без копии: его читают, но не меняют — Order
// только перебирает записи. Копировать тридцать структур на каждый запрос
// панели значило бы платить за дисциплину, которую всё равно держит не
// копия, а этот комментарий.
func (s *Server) manifestEntries() []happ.Entry {
	s.manifestMu.RLock()
	defer s.manifestMu.RUnlock()
	return s.manifest
}

// reloadManifest перечитывает манифест с диска в кэш.
//
// Ошибка чтения кэш НЕ обнуляет. Битый или внезапно нечитаемый файл — это
// потеря порядка и видов записей, но прежде прочитанный порядок всё ещё
// верен для того списка узлов, который сейчас в движке. Обнулив кэш, мы
// превратили бы одну неудачную запись файла в перетасованный список у
// владельца — причём молча, потому что список остался бы рабочим.
func (s *Server) reloadManifest() {
	entries, err := subs.LoadManifest(s.cfg.ManifestPath)
	if err != nil {
		s.logf("манифест подписки %s не прочитан (%v): "+
			"порядок и виды строк остаются от предыдущего чтения", s.cfg.ManifestPath, err)
		return
	}
	s.manifestMu.Lock()
	s.manifest = entries
	s.manifestMu.Unlock()
}

// handleStatus не имеет ветки ошибки: Read её не возвращает.
//
// Это не упущение, а решение. Статус обязан отвечать при любом сбое
// источников — недоступность каждого видна в самом ответе (online.checked,
// mode, available) и попадает в журнал демона.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, _ := s.status.Read(r.Context())

	// Ссылки собираются на КАЖДЫЙ запрос и именно здесь: у build нет доступа
	// к r.Host, а его результат кэшируется на 500 мс и уходит разным клиентам
	// (см. Links и Read в status.go).
	//
	// К available поля НЕ привязаны — и это про поле, а не про кнопку.
	//
	// Раньше здесь стоял довод «ссылка — это адрес, а не проба живости:
	// владелец скорее полезет в морду b4 именно тогда, когда b4 нам не
	// отвечает». Он опровергнут: он предполагал, что API и веб-морда — разные
	// слушатели, а у обоих движков слушатель ОДИН (b4 — ":::7000",
	// docs/recon/evidence.json, ключ b4_api.listen_observed; nikki —
	// «отдельного слушателя нет: external-controller '[::]:9090'
	// единственный», там же, ключ nikki.panel.port). При
	// available:false ссылка вела в connection refused в 100% случаев.
	//
	// Поле осталось независимым по другой причине, и она сильная: обнулив его
	// по available, мы (а) потеряли бы различение «хост не вывелся» и «служба
	// легла», на котором стоит текст links.why.host, и (б) втащили бы сюда
	// кэшированную на 500 мс живость — в поле, которое заведено некэшируемым
	// именно затем, чтобы следовать Host каждого запроса
	// (TestStatusLinksFollowRequestHostNotCache).
	//
	// Кнопку МОРДЫ ДВИЖКА панель рисует, только когда служба отвечает
	// (ADR-0024: docs/adr/0024-panel-links-only-when-service-answers.md).
	// К LuCI ниже это не относится, и не как исключение, а по самой
	// формулировке правила: оно про морды тех служб, которые гасит
	// netmode-apply. uhttpd мы не гасим и живость его не считаем.
	if host, ok := panelHost(r.Host); ok {
		if u, ok := b4.PanelURL(host); ok {
			st.Links.B4 = &u
		}
		// Адрес морды nikki собирается из ДВУХ источников: хост — из Host
		// запроса, порт — из конфигурации (nikki.mixin.api_listen). Хост из
		// конфигурации взять нельзя: там 127.0.0.1, по которому ходит демон.
		//
		// Секрета здесь нет и не будет ни при каких условиях: статус
		// опрашивается раз в секунду, и секрет ездил бы в каждом ответе,
		// оседая в кэшах и журналах промежуточных слоёв. Полный адрес отдаёт
		// GET /api/nikki/panel по клику. Пинится TestStatusNeverLeaksSecret.
		if port, ok := nikki.PanelPort(s.cfg.NikkiURL); ok {
			if u, ok := nikki.PanelBaseURL(host, port); ok {
				st.Links.Nikki = &u
			}
		}
		// Веб-интерфейс роутера. Живостью не гейтится — и это не забывчивость,
		// а прямое следствие формулировки ADR-0024, раздел «Границы правила»:
		// правило накрывает морды движков, которыми управляет netmode-apply,
		// а uhttpd мы не гасим. Пробы ради подсветки кнопки здесь не будет:
		// при опросе раз в секунду (ADR-0017) это 3600 запросов в час к чужой
		// службе.
		if u, ok := luci.PanelURL(host); ok {
			st.Links.LuCI = &u
		}
	}

	writeJSON(w, http.StatusOK, st)
}

// panelHost выделяет из заголовка Host имя, пригодное для ссылки в браузере.
//
// Второе значение — «годится ли». Отказ означает «ссылки не будет»: панель
// покажет на одну кнопку меньше, и это честнее, чем кнопка, ведущая не туда.
func panelHost(hostHeader string) (string, bool) {
	h := hostHeader
	if h == "" {
		// HTTP/1.0 без заголовка. Собирать ссылку не из чего.
		return "", false
	}

	// Порт отрезаем, свой подставим позже. Ошибка SplitHostPort здесь —
	// ШТАТНЫЙ случай, а не сбой: `Host: router.lan` приходит без порта, когда
	// панель открыта на 80-м, и такой заголовок вполне рабочий.
	if h2, _, err := net.SplitHostPort(h); err == nil {
		h = h2
	}
	// IPv6 без порта SplitHostPort не разбирает, скобки остаются на месте:
	// `Host: [fd00::1]`. Снимаем сами — обратно их поставит JoinHostPort.
	if len(h) > 1 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", false
	}

	// Фильтр символов. Заголовок Host приходит от клиента, а результат
	// уезжает в href в браузере владельца: проверка дублирует разбор выше
	// намеренно — это последний рубеж перед подстановкой в чужую страницу.
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_', c == ':':
		default:
			return "", false
		}
	}

	// Петля — отказ. Демон слушает LAN, но панель могли открыть через
	// ssh-туннель (`http://localhost:8088/`), и тогда `localhost` в ссылке
	// указывает на ноутбук владельца, а не на роутер: там на 7000-м либо
	// ничего нет, либо чужой сервис. Различить туннель и прямое обращение
	// демон не может, а угадывать он не будет — тот же принцип, что
	// `503 radio_unknown` в ADR-0019: не вывелось — отказ, а не догадка.
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return "", false
	}
	lower := strings.ToLower(h)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return "", false
	}

	return h, true
}

func (s *Server) handleWifiNetworks(w http.ResponseWriter, r *http.Request) {
	s.respondNetworks(w, r.Context(), http.StatusOK)
}

func (s *Server) handleWifiScan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// И имя интерфейса, и само станционное радио выводятся из живого
	// состояния, а не берутся литералом: они меняются с конфигурацией
	// радио и с ревизией железа (scripts/check-no-hardcoded-if.sh).
	b, err := s.ex.UbusCall(ctx, "network.wireless", "status", nil)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "ubus_unavailable", err.Error())
		return
	}
	st, err := wireless.ParseStatus(b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "parse_failed", err.Error())
		return
	}

	radios := s.resolveRadios(ctx, st, nil)
	if errResp := requireStationRadio(radios); errResp != nil {
		errResp.send(w)
		return
	}

	dev := wireless.IfnameForMode(st, radios.Station, "sta")
	if dev == "" {
		// Не угадываем `wlan0`: без подтверждённого имени скан невозможен.
		writeErr(w, http.StatusServiceUnavailable, "ifname_unknown",
			"Имя станционного интерфейса не определено")
		return
	}

	sb, err := s.ex.UbusCall(ctx, "iwinfo", "scan", map[string]any{"device": dev})
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "scan_failed", err.Error())
		return
	}
	nets, err := wireless.ParseScan(sb)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "parse_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scanned_at": time.Now().UTC().Format(time.RFC3339),
		"ifname":     dev,
		"band":       radios.StationBand,
		"note":       bandNote(radios.StationBand),
		"networks":   nets,
	})
}

// resolveRadios выводит привязку «радио ↔ роль ↔ диапазон» из системы.
//
// Аргументы — то, что вызывающий уже прочитал; nil означает «не читал, дочитай
// сам». Так путь скана не запускает `uci show` второй раз, а путь записи —
// `ubus call`: на роутере с 512 МБ лишний запуск процесса стоит дороже, чем
// эта развилка.
//
// Порядок источников задан в wireless.ResolveRadios и здесь не дублируется.
// Литерального запаса нет ни на одном шаге: не вывелось — вызывающий обязан
// отказать (ADR-0019).
func (s *Server) resolveRadios(ctx context.Context, st map[string]wireless.Radio, cfg *uci.Config) wireless.Radios {
	if st == nil {
		if b, err := s.ex.UbusCall(ctx, "network.wireless", "status", nil); err == nil {
			st, _ = wireless.ParseStatus(b)
		}
	}
	if cfg == nil {
		if raw, err := s.ex.UCIShow(ctx, "wireless"); err == nil {
			cfg, _ = uci.ParseShow("wireless", raw)
		}
	}
	return wireless.ResolveRadios(st, cfg)
}

// requireStationRadio превращает «не выяснили» в отказ.
//
// Отдельная функция, потому что отказ обязан быть одинаковым на всех путях:
// стоит одному из них вернуть «ну возьмём radio0», и весь смысл разрешения
// пропадает. 503, а не 500: радио может появиться (его включат) — повтор
// осмыслен.
func requireStationRadio(radios wireless.Radios) *httpErr {
	if radios.Known() {
		return nil
	}
	return &httpErr{http.StatusServiceUnavailable, "radio_unknown",
		"Не удалось определить, какое радио работает станцией: ни в " +
			"network.wireless status, ни в /etc/config/wireless нет интерфейса " +
			"с mode=sta. Демон не выбирает радио наугад."}
}

// bandNote — объяснение для панели про то, каких сетей в списке не будет.
//
// Текст условен, потому что утверждение проверяемо ложно на другом радио:
// «видны только 2.4 ГГц» при станции на 5 ГГц — это не пояснение, а
// дезинформация ровно в том месте, где владелец ищет пропавшую сеть.
func bandNote(band string) string {
	switch band {
	case "2g":
		return "Станция роутера работает в диапазоне 2.4 ГГц — сети 5 ГГц в этом списке не появятся."
	case "5g":
		return "Станция роутера работает в диапазоне 5 ГГц — сети 2.4 ГГц в этом списке не появятся."
	case "":
		return "Диапазон станционного радио выяснить не удалось — список может быть неполным."
	default:
		return "Станция роутера работает в диапазоне " + band +
			" — сети других диапазонов в этом списке не появятся."
	}
}

// ─────────── ответы ───────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, apiError{Code: errCode, Error: msg})
}

// ListenAndServe запускает сервер.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go s.awaitShutdown(ctx, srv.Shutdown)
	return srv.ListenAndServe()
}

// awaitShutdown гасит сервер по отмене контекста.
//
// Своя горутина нужна потому, что ListenAndServe занимает вызывающую до
// самого конца. Практическая вероятность паники в Shutdown мизерна, но
// перехват тут стоит одной строки, а непокрытая горутина через полгода
// выглядит как сознательное исключение, которого никто уже не помнит.
//
// Функция остановки передаётся параметром, а не берётся у srv: иначе
// перехват нельзя было бы проверить тестом, а защита, которую ни разу не
// видели работающей, — это только текст в файле.
//
// Ошибка Shutdown глотается, как и раньше: демон уже уходит, и сообщать о
// неудачном закрытии некому и незачем.
func (s *Server) awaitShutdown(ctx context.Context, shutdown func(context.Context) error) {
	<-ctx.Done()
	_ = safe.Do(s.logf, "остановка HTTP-сервера", func() error {
		// Пять секунд на дочитывание текущих ответов: панель опрашивает
		// статус часто, и обрывать её ради мгновенного выхода незачем.
		grace, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return shutdown(grace)
	})
}
