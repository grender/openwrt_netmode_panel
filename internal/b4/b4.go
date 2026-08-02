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
	"net"
	"net/http"
	"time"
)

// DefaultBaseURL — адрес b4 на этом роутере.
//
// Не настраивается: адрес — факт разведки, а конфигурируемость дала бы
// способ увести вызовы на чужой хост.
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
	// SelectOnly включает указанный сет и гасит остальные.
	SelectOnly(ctx context.Context, id string) error
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

// ErrPartial — переключение оборвалось между двумя вызовами.
//
// Прочие сеты уже погашены, целевой включить не удалось: b4 сейчас не
// обрабатывает трафик ни одним сетом. Отката нет и не будет (ADR-0006), демон
// не чинит состояние сам (ADR-0010) — а компенсирующий вызов и не помог бы:
// если b4 не ответил на включение, он с той же вероятностью не ответит и на
// компенсацию, и поверх одного непредсказуемого результата ляжет второй.
// Поэтому состояние называется вслух, а разбирается оно повтором нажатия.
var ErrPartial = errors.New("b4: прочие сеты выключены, целевой не включён")

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
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("b4: разбор ответа %s: %w", path, err)
	}
	return nil
}

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

// SelectOnly включает указанный сет и гасит все остальные.
//
// Эксклюзивность — НАША семантика, не b4. В самом b4 «текущего сета» не
// существует: у каждого свой флаг enabled, включённых может быть несколько,
// а порядок задаёт приоритет обработки (docs/docs/sets/index.md:26).
// Владелец выбрал модель «один из N», поэтому переключение — два вызова.
//
// Порядок именно такой: сначала гасим лишние, потом включаем нужный.
// Обратный порядок оставил бы промежуток, в котором включено два сета
// сразу, — а это уже другое поведение обхода.
func (c *HTTP) SelectOnly(ctx context.Context, id string) error {
	sets, err := c.Sets(ctx)
	if err != nil {
		return err
	}

	var target *Set
	var others []string
	for i := range sets {
		if sets[i].ID == id {
			target = &sets[i]
			continue
		}
		if sets[i].Enabled {
			others = append(others, sets[i].ID)
		}
	}
	if target == nil {
		return ErrNotFound
	}

	// Гашение УДАЛОСЬ — значит система уже не в исходном состоянии, и провал
	// второго шага надо называть иначе, чем провал первого.
	disabled := false

	if len(others) > 0 {
		var resp batchResponse
		err := c.do(ctx, http.MethodPost, "/api/sets/batch-set-enabled",
			batchRequest{IDs: others, Enabled: false}, &resp, switchTimeout)
		if err != nil {
			return fmt.Errorf("гашение прочих сетов: %w", err)
		}
		if resp.rejected() {
			return fmt.Errorf("%w: гашение прочих сетов", ErrRejected)
		}
		disabled = true
	}

	if !target.Enabled {
		var resp batchResponse
		err := c.do(ctx, http.MethodPost, "/api/sets/batch-set-enabled",
			batchRequest{IDs: []string{id}, Enabled: true}, &resp, switchTimeout)
		switch {
		case err != nil && disabled:
			// ErrUnavailable здесь была бы враньём: вызывающий не отличил бы
			// «ничего не начали» от «всё погасили». Внутренняя причина идёт
			// через %v, а не %w, чтобы errors.Is(ErrUnavailable) не увёл
			// обработчик в 503 «b4 не отвечает» мимо этого состояния.
			return fmt.Errorf("%w: %v", ErrPartial, err)
		case err != nil:
			return fmt.Errorf("включение сета: %w", err)
		case resp.rejected() && disabled:
			return fmt.Errorf("%w: b4 отклонил включение", ErrPartial)
		case resp.rejected():
			return fmt.Errorf("%w: включение сета", ErrRejected)
		}
	}
	return nil
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
