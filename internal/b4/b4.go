// Package b4 — клиент HTTP API обхода DPI.
//
// Файл /etc/b4/b4.json не читается и не пишется НИКОГДА (ADR-0007).
// Разбор исходников b4 v1.74.2 показал, что переключение сета через API
// сохраняется на диск до подмены указателя конфига
// (src/http/handler/config.go:439), поэтому дублировать выбор в файле
// незачем — а b4 сам мигрирует свой конфиг между версиями, и наша запись
// затёрла бы поля, которых мы не знаем.
//
// Контракт целиком: docs/recon/b4-api.md, подтверждён на живом роутере
// (docs/recon/raw/50-b4-api.txt, raw/51-b4-sets.json).
package b4

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultBaseURL — адрес b4 на этом роутере.
//
// Не настраивается: адрес — факт разведки, а конфигурируемость дала бы
// способ увести вызовы на чужой хост.
//
// Литерал обязан остаться ЕДИНЫМ и не разбираться на части. Гейт
// scripts/check-evidence.sh сверяет с docs/recon/evidence.json литералы вида
// `*:7000*`; после «рефакторинга» в `DefaultPort = "7000"` плюс
// `"http://127.0.0.1:" + DefaultPort` не совпадёт ни одна половина — в первой
// есть двоеточие без порта, во второй порт без двоеточия, — и гейт молча
// перестанет проверять адрес вообще. Порт, который понадобился отдельно,
// достаётся отсюда разбором (см. PanelURL), а не вторым литералом.
const DefaultBaseURL = "http://127.0.0.1:7000"

// Таймауты. Все вызовы b4 локальные, поэтому короткие: зависший клиент
// подвесил бы опрос статуса, который идёт раз в секунду.
const (
	probeTimeout  = 2 * time.Second
	callTimeout   = 5 * time.Second
	switchTimeout = 10 * time.Second
)

// Set — набор стратегий обхода.
//
// Полей стратегии здесь нет намеренно: каждый сет тащит ~2.5 КБ настроек
// (raw/51-b4-sets.json), а панель опрашивает статус раз в секунду. Наружу
// проецируется только то, что показывается.
type Set struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Version — ответ /api/version.
type Version struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

// Client — интерфейс, чтобы обработчики тестировались без сети.
type Client interface {
	Version(ctx context.Context) (Version, error)
	Sets(ctx context.Context) ([]Set, error)
	// SetEnabled включает или выключает ОДИН сет, не трогая остальные.
	SetEnabled(ctx context.Context, id string, on bool) error
	// Socks5Config и SetSocks5Config — встроенный SOCKS5-сервер b4
	// (ADR-0048). Новые настройки вступают в силу только после Restart.
	Socks5Config(ctx context.Context) (Socks5Config, error)
	SetSocks5Config(ctx context.Context, cfg Socks5Config) error
	Restart(ctx context.Context) error
}

// Socks5Config — секция system.socks5 конфига b4
// (src/config/types.go:303-311 в исходниках v1.74.2).
//
// Все поля, включая таймауты UDP, которые демону не нужны: POST
// /api/socks5/config заменяет секцию ЦЕЛИКОМ (src/http/handler/socks5.go:69-71),
// и не присланный таймаут стал бы нулём. Поэтому запись — только
// «прочитал, поменял нужное, отправил всё».
type Socks5Config struct {
	Enabled        bool   `json:"enabled"`
	Port           int    `json:"port"`
	BindAddress    string `json:"bind_address"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	UDPTimeout     int    `json:"udp_timeout"`
	UDPReadTimeout int    `json:"udp_read_timeout"`
}

// ErrUnavailable — b4 не отвечает.
//
// Отдельная ошибка, потому что это НЕ сбой демона: b4 перезапускается сам
// (в снимке разведки он как раз лежал — raw/25-netstat.txt), и панель
// обязана продолжать работать, погасив чипы сетов.
var ErrUnavailable = errors.New("b4: API не отвечает")

// ErrNotFound — сет с таким id не существует.
var ErrNotFound = errors.New("b4: сет не найден")

// ErrRejected — b4 ответил 200, но сообщил, что запрос не выполнил.
//
// Отдельно от ErrUnavailable: сервис жив и ответил по делу, повторять вызов
// вслепую бессмысленно — сначала надо перечитать список сетов.
var ErrRejected = errors.New("b4: запрос отклонён")

// ErrPartial больше нет, и это следствие ADR-0033, а не уборка. Половинчатое
// состояние было НАШИМ изобретением: эксклюзивность требовала двух вызовов
// подряд, и обрыв между ними гасил прочие сеты, не включив целевой. У одного
// вызова середины не существует — он либо выполнен, либо нет, — поэтому и
// код b4_partial из контракта убран вместе с ней.

// HTTP — реальный клиент.
type HTTP struct {
	BaseURL string
	client  *http.Client
}

func New(baseURL string) *HTTP {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &HTTP{
		BaseURL: baseURL,
		client: &http.Client{
			Transport: &http.Transport{
				// Соединение локальное; держать пул незачем, но и рвать
				// на каждый запрос при опросе раз в секунду расточительно.
				MaxIdleConns:        2,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

// do выполняет запрос и разбирает ответ.
//
// Аутентификации нет: на этом роутере `auth_check` вернул
// `{"auth_required":false}` (raw/50-b4-api.txt). Если её когда-нибудь
// включат, вызовы начнут возвращать 401 и клиент честно деградирует
// в ErrUnavailable, а не станет подбирать пароль.
func (c *HTTP) do(ctx context.Context, method, path string, body any, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("b4: сборка тела: %w", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("b4: запрос: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// Порядок проверок обязателен. client.Do заворачивает ЛЮБУЮ ошибку
		// в *url.Error, а тот реализует net.Error, — спроси мы про net.Error
		// первым, отменённый нами же контекст стал бы «b4 не отвечает».
		switch {
		case errors.Is(err, context.Canceled):
			// Это НАШ отказ, а не недоступность b4: панель ушла со страницы
			// и закрыла запрос. Обёртка в ErrUnavailable погасила бы чипы
			// сетов в статусе на ровном месте.
			return err
		case errors.Is(err, context.DeadlineExceeded):
			// b4 не успел за отведённый нами таймаут — для панели это то же
			// самое, что «не отвечает».
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		var nerr net.Error
		if errors.As(err, &nerr) {
			// Отказ соединения, обрыв, DNS — b4 сейчас нет.
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		// Не сеть и не отмена — скорее наш баг конструирования запроса.
		// Маскировать его под «сервис недоступен» вредно: панель покажет
		// «b4 перезапускается», и причину никто не пойдёт искать.
		return fmt.Errorf("b4: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: код %d", ErrUnavailable, resp.StatusCode)
	case resp.StatusCode >= 400:
		return fmt.Errorf("b4: код %d", resp.StatusCode)
	}

	if out == nil {
		return nil
	}
	// Потолок тела (ADR-0022), как у клиентов nikki, happ и geosite: статус
	// опрашивает b4 раз в секунду, и сбойная сборка с бесконечным телом
	// съедала бы память роутера до таймаута вызова. Байт сверх потолка
	// читается, чтобы отличить «не влезло» от «оборвалось».
	lr := &io.LimitedReader{R: resp.Body, N: maxBody + 1}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		if lr.N == 0 {
			return fmt.Errorf("b4: тело ответа %s превысило потолок %d Б", path, maxBody)
		}
		return fmt.Errorf("b4: разбор ответа %s: %w", path, err)
	}
	if lr.N == 0 {
		return fmt.Errorf("b4: тело ответа %s превысило потолок %d Б", path, maxBody)
	}
	return nil
}

// maxBody — потолок тела ответа b4. Сет — около 2,5 КБ JSON; мегабайт
// вмещает сотни сетов с большим запасом.
const maxBody = 1 << 20

// Version — проба живости.
//
// Единственный маршрут под /api/, освобождённый от аутентификации
// (src/http/auth.go:267-271, в комментарии прямо сказано «used as health
// check»), поэтому он же самый дешёвый способ узнать, жив ли b4.
func (c *HTTP) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.do(ctx, http.MethodGet, "/api/version", nil, &v, probeTimeout)
	return v, err
}

// Sets возвращает список сетов.
//
// Ответ — плоский массив (src/http/handler/sets.go:270-277), никогда не
// null. Имя НЕ уникально и не является ключом: id генерируется сервером
// как uuid (sets.go:315), поэтому имя резолвится в id сканированием.
func (c *HTTP) Sets(ctx context.Context) ([]Set, error) {
	var out []Set
	if err := c.do(ctx, http.MethodGet, "/api/sets", nil, &out, callTimeout); err != nil {
		return nil, err
	}
	return out, nil
}

type batchRequest struct {
	IDs     []string `json:"ids"`
	Enabled bool     `json:"enabled"`
}

// batchResponse — ответ batch-set-enabled: {"success": true, "updated": <n>}.
//
// Success — указатель НАМЕРЕННО. Форма ответа известна по исходникам b4
// (sets.go:634-698, docs/recon/b4-api.md), но самого тела с живого роутера
// в docs/recon/raw/ нет, а на роутере стоит 1.74.1 против 1.74.2 в разборе.
// Проверка по значению означала бы: сборка, которая поля не шлёт, декодируется
// в false — и ЛЮБОЕ переключение сета начинает падать. Отсутствие поля
// считаем успехом, ошибку возвращаем только на явном false.
type batchResponse struct {
	Success *bool `json:"success"`
	// Updated разбираем, но с числом отправленных id НЕ сверяем: updated:0
	// означает «запрошенное значение уже стояло» и остаётся успехом
	// (sets.go:679-683, ADR-0008).
	Updated int `json:"updated"`
}

// rejected — b4 явно сказал, что не выполнил запрос.
func (r batchResponse) rejected() bool { return r.Success != nil && !*r.Success }

// SetEnabled включает или выключает ОДИН сет, не трогая остальные.
//
// Это родная модель b4, а не наша: «текущего сета» в нём не существует, у
// каждого свой флаг enabled, включённых может быть сколько угодно, а порядок
// в списке задаёт приоритет обработки (docs/docs/sets/index.md:26). Прежняя
// эксклюзивность была нашей надстройкой и снята ADR-0033.
//
// Один вызов вместо двух — и это не оптимизация, а другое множество исходов.
// У двухшагового переключения существовала середина: прочие погашены, целевой
// не включён, обход выключен целиком. Здесь такого состояния нет, поэтому нет
// и ErrPartial: либо b4 применил запрошенное, либо не применил ничего.
//
// Список читается заранее ради двух вещей: неизвестный id обязан стать
// ErrNotFound (без этого b4 ответил бы 200 на ids с несуществующим uuid, и
// панель показала бы успех несделанного), а совпадающее значение не стоит
// сетевого вызова.
func (c *HTTP) SetEnabled(ctx context.Context, id string, on bool) error {
	sets, err := c.Sets(ctx)
	if err != nil {
		return err
	}

	var target *Set
	for i := range sets {
		if sets[i].ID == id {
			target = &sets[i]
			break
		}
	}
	if target == nil {
		return ErrNotFound
	}

	// Значение уже стоит. Не ошибка и не повод ходить в сеть: updated:0 у b4
	// означает ровно это (sets.go:679-683), и отличить его от успеха нечем.
	if target.Enabled == on {
		return nil
	}

	var resp batchResponse
	if err := c.do(ctx, http.MethodPost, "/api/sets/batch-set-enabled",
		batchRequest{IDs: []string{id}, Enabled: on}, &resp, switchTimeout); err != nil {
		return err
	}
	if resp.rejected() {
		return fmt.Errorf("%w: переключение сета", ErrRejected)
	}
	return nil
}

type socks5Response struct {
	Success *bool        `json:"success"`
	Config  Socks5Config `json:"config"`
}

// Socks5Config читает настройки SOCKS5-сервера b4: GET /api/socks5/config,
// ответ {"success":true,"config":{…}} (src/http/handler/socks5.go:22-34).
// Пароль b4 отдаёт открытым — прячет он только пароль веб-морды.
func (c *HTTP) Socks5Config(ctx context.Context) (Socks5Config, error) {
	var resp socks5Response
	if err := c.do(ctx, http.MethodGet, "/api/socks5/config", nil, &resp, callTimeout); err != nil {
		return Socks5Config{}, err
	}
	if resp.Success != nil && !*resp.Success {
		return Socks5Config{}, fmt.Errorf("%w: чтение настроек SOCKS5", ErrRejected)
	}
	return resp.Config, nil
}

// SetSocks5Config записывает секцию SOCKS5 целиком: POST /api/socks5/config.
//
// b4 сохраняет её в свой конфиг на диске сам (тот же saveAndPushConfig, что
// и у сетов), но слушатель перечитывает её только при старте — нужен
// Restart. Занятый порт b4 отвергает кодом 400 (preflight пробует
// net.Listen), и это ошибка, а не повод повторять: порт держит кто-то ещё.
func (c *HTTP) SetSocks5Config(ctx context.Context, cfg Socks5Config) error {
	var resp struct {
		Success *bool `json:"success"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/socks5/config", cfg, &resp, switchTimeout); err != nil {
		return err
	}
	if resp.Success != nil && !*resp.Success {
		return fmt.Errorf("%w: запись настроек SOCKS5", ErrRejected)
	}
	return nil
}

// Restart перезапускает b4: POST /api/system/restart. b4 сначала отвечает,
// а через полсекунды сам вызывает /etc/init.d/b4 restart
// (src/http/handler/system.go:166-214) — ответ означает «принято», а не
// «перезапущен». Дождаться нового процесса — забота вызывающего (Version).
func (c *HTTP) Restart(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/system/restart", nil, nil, callTimeout)
}

// Selected возвращает имя единственного включённого сета.
//
// Пусто, если включённых ноль или больше одного: во втором случае
// состояние выставлено мимо панели (через веб-морду b4), и выдавать
// первый попавшийся за «текущий» значило бы врать.
func Selected(sets []Set) string {
	name := ""
	n := 0
	for _, s := range sets {
		if s.Enabled {
			n++
			name = s.Name
		}
	}
	if n == 1 {
		return name
	}
	return ""
}

// PanelURL — адрес веб-морды b4, пригодный для браузера владельца.
//
// Живёт здесь, а не в internal/httpapi, потому что порт — внешняя поверхность,
// и подтверждать её обязан гейт: scripts/check-evidence.sh сканирует
// internal/b4, но internal/httpapi исключает намеренно. Порт, написанный
// литералом в обработчике, гейт молча пропустил бы — то есть адрес чужого
// сервиса появился бы в коде без единой ссылки на разведку.
//
// Второго литерала здесь нет и по той же причине: порт достаётся разбором
// DefaultBaseURL, который уже подтверждён в docs/recon/evidence.json. 127.0.0.1
// оттуда НЕ берётся — этот адрес верен для демона, но в браузере владельца
// указывал бы на его же ноутбук; хост подставляет вызывающий, из заголовка
// Host своего запроса.
//
// Сборка — только net.JoinHostPort: ручное host + ":" + port выдало бы для
// IPv6 `fd00::1:7000` — адрес, который браузер разберёт как другой хост без
// порта, то есть тихо неправильную ссылку вместо явной ошибки.
func PanelURL(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	base, err := url.Parse(DefaultBaseURL)
	if err != nil {
		return "", false
	}
	port := base.Port()
	if port == "" {
		return "", false
	}
	u := url.URL{Scheme: base.Scheme, Host: net.JoinHostPort(host, port), Path: "/"}
	return u.String(), true
}

// EnabledCount — сколько сетов включено.
func EnabledCount(sets []Set) int {
	n := 0
	for _, s := range sets {
		if s.Enabled {
			n++
		}
	}
	return n
}
